package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type DatabaseOptions struct {
	DSN                string
	ProfileID          string
	IncludeChatArchive bool
	ChatVisibility     Visibility
	// Chat archive source (read-only). Defaults are applied in NewDatabaseStore.
	ChatArchiveTable      string
	ChatArchiveTextColumn string
	ChatArchiveTimeColumn string
	IncludeMediaArchive   bool
	MediaVisibility       Visibility
	MediaArchiveTable     string
	MediaOCRColumn        string
	MediaCaptionColumn    string
	MediaTimeColumn       string
	MediaSenderColumn     string
	MediaFilePathColumn   string
	MediaMsgIDColumn      string
}

type DatabaseStore struct {
	db                    *sql.DB
	profileID             string
	includeChatArchive    bool
	chatVisibility        Visibility
	chatArchiveTable      string
	chatArchiveTextColumn string
	chatArchiveTimeColumn string
	includeMediaArchive   bool
	mediaVisibility       Visibility
	mediaArchiveTable     string
	mediaOCRColumn        string
	mediaCaptionColumn    string
	mediaTimeColumn       string
	mediaSenderColumn     string
	mediaFilePathColumn   string
	mediaMsgIDColumn      string
}

func NewDatabaseStore(opts DatabaseOptions) (*DatabaseStore, error) {
	if opts.DSN == "" {
		return nil, fmt.Errorf("memory database dsn is empty")
	}
	if opts.ProfileID == "" {
		opts.ProfileID = "default"
	}
	if opts.ChatVisibility == "" {
		opts.ChatVisibility = VisOwnerOnly
	}
	if opts.MediaVisibility == "" {
		opts.MediaVisibility = VisOwnerOnly
	}
	if !isSafeIdentifier(opts.ChatArchiveTable) {
		opts.ChatArchiveTable = "chat_message_chunks"
	}
	if !isSafeIdentifier(opts.ChatArchiveTextColumn) {
		opts.ChatArchiveTextColumn = "chunk_text"
	}
	if !isSafeIdentifier(opts.ChatArchiveTimeColumn) {
		opts.ChatArchiveTimeColumn = "end_time"
	}
	if !isSafeIdentifier(opts.MediaArchiveTable) {
		opts.MediaArchiveTable = "media_assets"
	}
	if !isSafeIdentifier(opts.MediaOCRColumn) {
		opts.MediaOCRColumn = "ocr_text"
	}
	if !isSafeIdentifier(opts.MediaCaptionColumn) {
		opts.MediaCaptionColumn = "caption"
	}
	if !isSafeIdentifier(opts.MediaTimeColumn) {
		opts.MediaTimeColumn = "sent_at"
	}
	if !isSafeIdentifier(opts.MediaSenderColumn) {
		opts.MediaSenderColumn = "sender"
	}
	if !isSafeIdentifier(opts.MediaFilePathColumn) {
		opts.MediaFilePathColumn = "file_path"
	}
	if !isSafeIdentifier(opts.MediaMsgIDColumn) {
		opts.MediaMsgIDColumn = "msgid"
	}
	db, err := sql.Open("mysql", opts.DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	store := &DatabaseStore{
		db:                    db,
		profileID:             opts.ProfileID,
		includeChatArchive:    opts.IncludeChatArchive,
		chatVisibility:        opts.ChatVisibility,
		chatArchiveTable:      opts.ChatArchiveTable,
		chatArchiveTextColumn: opts.ChatArchiveTextColumn,
		chatArchiveTimeColumn: opts.ChatArchiveTimeColumn,
		includeMediaArchive:   opts.IncludeMediaArchive,
		mediaVisibility:       opts.MediaVisibility,
		mediaArchiveTable:     opts.MediaArchiveTable,
		mediaOCRColumn:        opts.MediaOCRColumn,
		mediaCaptionColumn:    opts.MediaCaptionColumn,
		mediaTimeColumn:       opts.MediaTimeColumn,
		mediaSenderColumn:     opts.MediaSenderColumn,
		mediaFilePathColumn:   opts.MediaFilePathColumn,
		mediaMsgIDColumn:      opts.MediaMsgIDColumn,
	}
	if err := store.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *DatabaseStore) ensureSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS bot_memories (
  id varchar(64) NOT NULL,
  profile_id varchar(128) NOT NULL,
  content longtext NOT NULL,
  raw longtext DEFAULT NULL,
  sender varchar(255) DEFAULT NULL,
  category varchar(64) DEFAULT NULL,
  visibility varchar(32) NOT NULL,
  source_type varchar(64) DEFAULT NULL,
  hash varchar(64) DEFAULT NULL,
  created_at bigint NOT NULL,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_profile_visibility (profile_id, visibility),
  KEY idx_profile_created_at (profile_id, created_at),
  KEY idx_hash (hash),
  FULLTEXT KEY ft_bot_memories_content (content) WITH PARSER ngram PARSER_PROPERTIES=(ngram_token_size=2)
) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`)
	return err
}

func (s *DatabaseStore) Add(m Memory) error {
	if m.ID == "" {
		m.ID = fmt.Sprintf("mem_%d", time.Now().UnixNano())
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().Unix()
	}
	if m.Hash == "" {
		m.Hash = HashContent(m.Content)
	}
	_, err := s.db.Exec(`
INSERT INTO bot_memories
  (id, profile_id, content, raw, sender, category, visibility, source_type, hash, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  content=VALUES(content), raw=VALUES(raw), sender=VALUES(sender), category=VALUES(category),
  visibility=VALUES(visibility), source_type=VALUES(source_type), hash=VALUES(hash)`,
		m.ID, s.profileID, m.Content, m.Raw, m.Sender, m.Category, string(m.Visibility), m.SourceType, m.Hash, m.CreatedAt)
	return err
}

func (s *DatabaseStore) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM bot_memories WHERE profile_id=? AND id=?`, s.profileID, id)
	return err
}

func (s *DatabaseStore) All() []Memory {
	rows, err := s.db.Query(`
SELECT id, content, COALESCE(raw, ''), COALESCE(sender, ''), COALESCE(category, ''),
       visibility, COALESCE(source_type, ''), COALESCE(hash, ''), created_at
FROM bot_memories
WHERE profile_id=?
ORDER BY created_at DESC`, s.profileID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []Memory
	for rows.Next() {
		var m Memory
		var visibility string
		if err := rows.Scan(&m.ID, &m.Content, &m.Raw, &m.Sender, &m.Category, &visibility, &m.SourceType, &m.Hash, &m.CreatedAt); err == nil {
			m.Visibility = Visibility(visibility)
			out = append(out, m)
		}
	}
	return out
}

func (s *DatabaseStore) Search(query string, audience string) []string {
	results := s.searchBotMemories(query, audience, 5)
	if s.includeChatArchive && s.chatVisibleTo(audience) {
		results = append(results, s.searchChatArchive(query, 5)...)
	}
	if s.includeMediaArchive && s.mediaVisibleTo(audience) {
		for _, media := range s.SearchMedia(query, audience, 3) {
			results = append(results, media.ContextText())
		}
	}
	if len(results) > 8 {
		return results[:8]
	}
	return results
}

func (s *DatabaseStore) searchBotMemories(query string, audience string, limit int) []string {
	visibility := allowedVisibility(audience)
	if len(visibility) == 0 {
		return nil
	}
	args := []interface{}{s.profileID}
	placeholders := make([]string, 0, len(visibility))
	for _, v := range visibility {
		placeholders = append(placeholders, "?")
		args = append(args, string(v))
	}

	where := fmt.Sprintf("profile_id=? AND visibility IN (%s)", strings.Join(placeholders, ","))
	if strings.TrimSpace(query) != "" {
		where += " AND (MATCH(content) AGAINST (? IN NATURAL LANGUAGE MODE) OR content LIKE ?)"
		args = append(args, query, "%"+query+"%")
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
SELECT content
FROM bot_memories
WHERE `+where+`
ORDER BY created_at DESC
LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err == nil {
			out = append(out, content)
		}
	}
	return out
}

func (s *DatabaseStore) searchChatArchive(query string, limit int) []string {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	// Table/column names can't be parameterized; they are validated as safe
	// identifiers in NewDatabaseStore, so interpolation here is bounded.
	rows, err := s.db.Query(fmt.Sprintf(`
SELECT CONCAT('[聊天记录 ', DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'), '] ', %s)
FROM %s
WHERE MATCH(%s) AGAINST (? IN NATURAL LANGUAGE MODE) OR %s LIKE ?
ORDER BY %s DESC
LIMIT ?`,
		s.chatArchiveTimeColumn, s.chatArchiveTextColumn,
		s.chatArchiveTable,
		s.chatArchiveTextColumn, s.chatArchiveTextColumn,
		s.chatArchiveTimeColumn,
	), query, "%"+query+"%", limit)
	if err != nil {
		rows, err = s.db.Query(fmt.Sprintf(`
SELECT CONCAT('[聊天记录 ', DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'), '] ', %s)
FROM %s
WHERE %s LIKE ?
ORDER BY %s DESC
LIMIT ?`,
			s.chatArchiveTimeColumn, s.chatArchiveTextColumn,
			s.chatArchiveTable,
			s.chatArchiveTextColumn,
			s.chatArchiveTimeColumn,
		), "%"+query+"%", limit)
		if err != nil {
			return nil
		}
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err == nil {
			out = append(out, content)
		}
	}
	return out
}

func (s *DatabaseStore) SearchMedia(query string, audience string, limit int) []MediaResult {
	if !s.includeMediaArchive || !s.mediaVisibleTo(audience) {
		return nil
	}
	if limit <= 0 {
		limit = 3
	}
	if limit > 10 {
		limit = 10
	}
	term := cleanMediaQuery(query)
	if term == "" {
		return s.recentMedia(limit)
	}
	results := s.searchMediaFullText(term, limit)
	if len(results) == 0 {
		results = s.searchMediaLike(term, limit)
	}
	if len(results) == 0 && isVagueMediaQuery(query) {
		results = s.recentMedia(limit)
	}
	return results
}

func (s *DatabaseStore) searchMediaFullText(query string, limit int) []MediaResult {
	rows, err := s.db.Query(fmt.Sprintf(`
SELECT COALESCE(%s, ''), COALESCE(%s, ''), DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'),
       COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(has_text, 0)
FROM %s
WHERE MATCH(%s, %s) AGAINST (? IN NATURAL LANGUAGE MODE)
ORDER BY %s DESC
LIMIT ?`,
		s.mediaMsgIDColumn, s.mediaSenderColumn, s.mediaTimeColumn,
		s.mediaFilePathColumn, s.mediaOCRColumn, s.mediaCaptionColumn, s.mediaMsgIDColumn,
		s.mediaArchiveTable,
		s.mediaOCRColumn, s.mediaCaptionColumn,
		s.mediaTimeColumn,
	), query, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanMediaRows(rows)
}

func (s *DatabaseStore) searchMediaLike(query string, limit int) []MediaResult {
	terms := strings.Fields(query)
	if len(terms) == 0 {
		terms = []string{query}
	}
	clauses := make([]string, 0, len(terms)*2)
	args := make([]interface{}, 0, len(terms)*2+1)
	for _, term := range terms {
		pattern := "%" + term + "%"
		clauses = append(clauses, fmt.Sprintf("%s LIKE ?", s.mediaOCRColumn), fmt.Sprintf("%s LIKE ?", s.mediaCaptionColumn))
		args = append(args, pattern, pattern)
	}
	args = append(args, limit)
	rows, err := s.db.Query(fmt.Sprintf(`
SELECT COALESCE(%s, ''), COALESCE(%s, ''), DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'),
       COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(has_text, 0)
FROM %s
WHERE %s
ORDER BY %s DESC
LIMIT ?`,
		s.mediaMsgIDColumn, s.mediaSenderColumn, s.mediaTimeColumn,
		s.mediaFilePathColumn, s.mediaOCRColumn, s.mediaCaptionColumn, s.mediaMsgIDColumn,
		s.mediaArchiveTable,
		strings.Join(clauses, " OR "),
		s.mediaTimeColumn,
	), args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanMediaRows(rows)
}

func (s *DatabaseStore) recentMedia(limit int) []MediaResult {
	rows, err := s.db.Query(fmt.Sprintf(`
SELECT COALESCE(%s, ''), COALESCE(%s, ''), DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'),
       COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(has_text, 0)
FROM %s
ORDER BY %s DESC
LIMIT ?`,
		s.mediaMsgIDColumn, s.mediaSenderColumn, s.mediaTimeColumn,
		s.mediaFilePathColumn, s.mediaOCRColumn, s.mediaCaptionColumn, s.mediaMsgIDColumn,
		s.mediaArchiveTable,
		s.mediaTimeColumn,
	), limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanMediaRows(rows)
}

func scanMediaRows(rows *sql.Rows) []MediaResult {
	var out []MediaResult
	for rows.Next() {
		var result MediaResult
		var hasText int
		if err := rows.Scan(&result.MsgID, &result.Sender, &result.SentAt, &result.FilePath, &result.OCRText, &result.Caption, &result.MessageID, &hasText); err == nil {
			result.HasText = hasText != 0
			out = append(out, result)
		}
	}
	return out
}

func cleanMediaQuery(query string) string {
	q := strings.TrimSpace(query)
	replacer := strings.NewReplacer(
		"图片", " ", "照片", " ", "截图", " ", "图", " ",
		"找一下", " ", "找找", " ", "找", " ", "搜一下", " ", "搜索", " ",
		"发我", " ", "发一下", " ", "看看", " ", "看一下", " ",
		"回忆", " ", "我们", " ", "做了什么", " ", "干了什么", " ",
		"那张", " ", "那个", " ", "这个", " ", "一下", " ",
	)
	q = replacer.Replace(q)
	q = strings.Join(strings.Fields(q), " ")
	return q
}

func isVagueMediaQuery(query string) bool {
	q := strings.TrimSpace(query)
	if q == "" {
		return true
	}
	return strings.Contains(q, "回忆") || strings.Contains(q, "做了什么") || strings.Contains(q, "干了什么")
}

// isSafeIdentifier guards interpolated table/column names against SQL injection.
// Only bare SQL identifiers (letters, digits, underscore, leading non-digit) pass.
func isSafeIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func (s *DatabaseStore) chatVisibleTo(audience string) bool {
	switch s.chatVisibility {
	case VisPrivate:
		return false
	case VisOwnerOnly:
		return audience == "owner"
	case VisPublicToTarget:
		return audience == "owner" || audience == "target"
	default:
		return false
	}
}

func (s *DatabaseStore) mediaVisibleTo(audience string) bool {
	switch s.mediaVisibility {
	case VisPrivate:
		return false
	case VisOwnerOnly:
		return audience == "owner"
	case VisPublicToTarget:
		return audience == "owner" || audience == "target"
	default:
		return false
	}
}

func allowedVisibility(audience string) []Visibility {
	switch audience {
	case "owner":
		return []Visibility{VisOwnerOnly, VisPublicToTarget}
	case "target":
		return []Visibility{VisPublicToTarget}
	default:
		return nil
	}
}

func (s *DatabaseStore) Close() error {
	return s.db.Close()
}

var _ MemoryStore = (*DatabaseStore)(nil)
