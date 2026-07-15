package memory

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManagedMediaInput is an image owned by the bot rather than a path into an
// external chat export. MediaKey must be stable so repeated ingestion is safe.
type ManagedMediaInput struct {
	MediaKey   string
	Data       []byte
	SourcePath string
	Sender     string
	SentAt     int64
	OCRText    string
	Caption    string
	Visibility Visibility
}

type ManagedMediaAsset struct {
	ID           string
	ContentHash  string
	RelativePath string
	AbsolutePath string
}

func (s *DatabaseStore) SaveManagedMedia(ctx context.Context, input ManagedMediaInput) (ManagedMediaAsset, error) {
	if s.mediaVault == nil {
		return ManagedMediaAsset{}, fmt.Errorf("media vault is not configured")
	}
	if strings.TrimSpace(input.MediaKey) == "" {
		return ManagedMediaAsset{}, fmt.Errorf("media key is empty")
	}
	asset, err := s.mediaVault.StoreBytes(input.Data, input.SourcePath)
	if err != nil {
		return ManagedMediaAsset{}, err
	}
	if input.SentAt == 0 {
		input.SentAt = time.Now().Unix()
	}
	if input.Visibility == "" {
		input.Visibility = s.mediaVisibility
	}
	id := "media_" + HashContent(s.profileID+"|"+input.MediaKey)
	if len(id) > 64 {
		id = id[:64]
	}
	var embedding any
	searchText := strings.TrimSpace(strings.Join([]string{input.Caption, input.OCRText}, "\n"))
	if searchText != "" && s.embedder != nil {
		vec, embedErr := s.embedder.Embed(searchText)
		if embedErr != nil {
			log.Printf("[媒体入库] embedding 失败，保留图片和文本元数据: %v", embedErr)
		} else if len(vec) != s.embeddingDimension {
			log.Printf("[媒体入库] embedding 维度不匹配，保留图片和文本元数据: got=%d want=%d", len(vec), s.embeddingDimension)
		} else {
			embedding = vectorLiteral(vec)
		}
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO bot_media_assets
  (id, profile_id, media_key, content_hash, relative_path, source_path, mime_type, file_size, sender, sent_at, ocr_text, caption, visibility, status, embedding, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'valid', ?, ?)
ON DUPLICATE KEY UPDATE
  content_hash=VALUES(content_hash), relative_path=VALUES(relative_path), source_path=VALUES(source_path),
  mime_type=VALUES(mime_type), file_size=VALUES(file_size), sender=VALUES(sender), sent_at=VALUES(sent_at),
  ocr_text=VALUES(ocr_text), caption=VALUES(caption), visibility=VALUES(visibility), status='valid',
  embedding=COALESCE(VALUES(embedding), embedding)`,
		id, s.profileID, input.MediaKey, asset.Hash, asset.RelativePath, input.SourcePath,
		asset.MIMEType, asset.Size, input.Sender, input.SentAt, input.OCRText, input.Caption,
		string(input.Visibility), embedding, time.Now().Unix())
	if err != nil {
		return ManagedMediaAsset{}, err
	}
	return ManagedMediaAsset{ID: id, ContentHash: asset.Hash, RelativePath: asset.RelativePath, AbsolutePath: asset.AbsolutePath}, nil
}

func (s *DatabaseStore) searchManagedMedia(query, audience string, limit int, vecLiteral string) []MediaResult {
	if s.mediaVault == nil || limit <= 0 {
		return nil
	}
	visibility, ok := managedMediaVisibilityClause(audience)
	if !ok {
		return nil
	}
	term := cleanMediaQuery(query)
	var rows scannerRows
	var err error
	base := `
SELECT media_key, COALESCE(sender, ''), DATE_FORMAT(FROM_UNIXTIME(sent_at), '%Y-%m-%d %H:%i'),
       relative_path, COALESCE(ocr_text, ''), COALESCE(caption, '')
FROM bot_media_assets
WHERE profile_id=? AND status='valid' AND ` + visibility
	if term != "" && vecLiteral != "" {
		rows, err = s.db.Query(base+` AND embedding IS NOT NULL ORDER BY cosine_distance(embedding, ?) ASC LIMIT ?`, s.profileID, vecLiteral, limit)
	} else if term != "" {
		pattern := "%" + term + "%"
		rows, err = s.db.Query(base+` AND (ocr_text LIKE ? OR caption LIKE ?) ORDER BY sent_at DESC LIMIT ?`, s.profileID, pattern, pattern, limit)
	} else {
		rows, err = s.db.Query(base+` ORDER BY sent_at DESC LIMIT ?`, s.profileID, limit)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	results := make([]MediaResult, 0, limit)
	for rows.Next() {
		var result MediaResult
		var relative string
		if rows.Scan(&result.MsgID, &result.Sender, &result.SentAt, &relative, &result.OCRText, &result.Caption) != nil {
			continue
		}
		absolute, resolveErr := s.mediaVault.Resolve(relative)
		if resolveErr != nil {
			continue
		}
		if _, statErr := os.Stat(absolute); statErr != nil {
			_, _ = s.db.Exec(`UPDATE bot_media_assets SET status='missing' WHERE profile_id=? AND media_key=?`, s.profileID, result.MsgID)
			continue
		}
		result.MessageID = result.MsgID
		result.FilePath = absolute
		result.HasText = result.OCRText != ""
		results = append(results, result)
	}
	return results
}

type scannerRows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
}

func managedMediaVisibilityClause(audience string) (string, bool) {
	switch audience {
	case "owner":
		return "visibility IN ('owner_only', 'public_to_target')", true
	case "target":
		return "visibility='public_to_target'", true
	default:
		return "", false
	}
}

type MediaReingestStats struct {
	Rows      int `json:"rows"`
	Matched   int `json:"matched"`
	Imported  int `json:"imported"`
	AlreadyOK int `json:"already_ok"`
	Missing   int `json:"missing"`
	Ambiguous int `json:"ambiguous"`
	Errors    int `json:"errors"`
}

// ReingestLegacyMedia copies recoverable legacy archive images into the
// content-addressed vault. It is idempotent because media_key is unique per
// profile and the vault deduplicates bytes by SHA-256.
func (s *DatabaseStore) ReingestLegacyMedia(ctx context.Context, roots []string, apply bool, progress func(MediaReingestStats)) (MediaReingestStats, error) {
	if !s.includeMediaArchive {
		return MediaReingestStats{}, fmt.Errorf("legacy media archive is disabled")
	}
	if s.mediaVault == nil {
		return MediaReingestStats{}, fmt.Errorf("media vault is not configured")
	}
	byBase, err := indexImageRoots(roots)
	if err != nil {
		return MediaReingestStats{}, err
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
SELECT COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''),
       COALESCE(%s, ''), COALESCE(UNIX_TIMESTAMP(%s), 0)
FROM %s ORDER BY %s ASC`, s.mediaMsgIDColumn, s.mediaSenderColumn, s.mediaFilePathColumn,
		s.mediaOCRColumn, s.mediaCaptionColumn, s.mediaTimeColumn, s.mediaArchiveTable, s.mediaTimeColumn))
	if err != nil {
		return MediaReingestStats{}, err
	}
	defer rows.Close()
	var stats MediaReingestStats
	for rows.Next() {
		var key, sender, oldPath, ocr, caption string
		var sentAt int64
		if err := rows.Scan(&key, &sender, &oldPath, &ocr, &caption, &sentAt); err != nil {
			stats.Errors++
			continue
		}
		stats.Rows++
		if s.managedMediaExists(key) {
			stats.AlreadyOK++
			continue
		}
		path, ambiguous := resolveLegacyMediaPath(oldPath, s.mediaRoot, byBase)
		if ambiguous {
			stats.Ambiguous++
			continue
		}
		if path == "" {
			stats.Missing++
			continue
		}
		stats.Matched++
		if apply {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				stats.Errors++
				continue
			}
			_, saveErr := s.SaveManagedMedia(ctx, ManagedMediaInput{MediaKey: key, Data: data, SourcePath: path, Sender: sender, SentAt: sentAt, OCRText: ocr, Caption: caption, Visibility: s.mediaVisibility})
			if saveErr != nil {
				stats.Errors++
				continue
			}
			stats.Imported++
		}
		if progress != nil && stats.Rows%25 == 0 {
			progress(stats)
		}
	}
	if progress != nil {
		progress(stats)
	}
	return stats, rows.Err()
}

func (s *DatabaseStore) managedMediaExists(key string) bool {
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM bot_media_assets WHERE profile_id=? AND media_key=? AND status='valid'`, s.profileID, key).Scan(&count)
	return count > 0
}

func indexImageRoots(roots []string) (map[string][]string, error) {
	byBase := make(map[string][]string)
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			switch ext {
			case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".heic", ".tif", ".tiff", ".bmp":
				base := strings.ToLower(filepath.Base(path))
				byBase[base] = append(byBase[base], path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for _, paths := range byBase {
		sort.Strings(paths)
	}
	return byBase, nil
}

func resolveLegacyMediaPath(oldPath, mediaRoot string, byBase map[string][]string) (string, bool) {
	candidate := strings.TrimSpace(oldPath)
	if candidate != "" && !filepath.IsAbs(candidate) && mediaRoot != "" {
		candidate = filepath.Join(mediaRoot, candidate)
	}
	if candidate != "" {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, false
		}
	}
	matches := byBase[strings.ToLower(filepath.Base(oldPath))]
	if len(matches) == 1 {
		return matches[0], false
	}
	return "", len(matches) > 1
}
