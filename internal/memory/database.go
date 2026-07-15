package memory

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mediavault "feishu-companion-bot/internal/media"

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
	MediaStatusColumn     string
	MediaRoot             string
	MediaVault            string
	Embedder              Embedder
	EmbeddingDimension    int
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
	mediaStatusColumn     string
	mediaRoot             string
	mediaVault            *mediavault.Vault
	embedder              Embedder
	embeddingDimension    int
	embedQueue            chan string
	embedCancel           context.CancelFunc
	queryVectorMu         sync.Mutex
	queryVectorCache      map[string]cachedVectorLiteral
}

type cachedVectorLiteral struct {
	value     string
	expiresAt time.Time
}

type VectorDiagnostic struct {
	Table        string
	Exists       bool
	Rows         int64
	EmbeddedRows int64
	Indexes      []string
	Problem      string
}

type DatabaseDiagnostics struct {
	ServerVersion          string
	Tables                 []VectorDiagnostic
	DiscoveredVectorTables []string
	AvailableTables        []string
	MediaPaths             MediaPathDiagnostic
}

type MediaPathDiagnostic struct {
	Checked        int
	Valid          int
	Missing        int
	Unresolved     int
	ExampleMissing string
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
	if opts.EmbeddingDimension <= 0 || opts.EmbeddingDimension > 16000 {
		opts.EmbeddingDimension = 1024
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
	if !isSafeIdentifier(opts.MediaStatusColumn) {
		opts.MediaStatusColumn = "path_status"
	}
	db, err := sql.Open("mysql", opts.DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	var vault *mediavault.Vault
	if strings.TrimSpace(opts.MediaVault) != "" {
		vault, err = mediavault.NewVault(opts.MediaVault)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("initialize media vault: %w", err)
		}
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
		mediaStatusColumn:     opts.MediaStatusColumn,
		mediaRoot:             opts.MediaRoot,
		mediaVault:            vault,
		embedder:              opts.Embedder,
		embeddingDimension:    opts.EmbeddingDimension,
		embedQueue:            make(chan string, 500),
		queryVectorCache:      make(map[string]cachedVectorLiteral),
	}
	if err := store.ensureSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if store.includeMediaArchive {
		store.detectMediaStatusColumn()
		go store.auditMediaPaths(context.Background())
	}
	// Start an explicitly cancellable worker so tests and graceful shutdown do
	// not leave database goroutines behind.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	store.embedCancel = cancelWorker
	go store.startEmbeddingWorker(workerCtx)
	store.enqueueMissingEmbeddings()
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
  memory_type varchar(32) DEFAULT NULL,
  importance int DEFAULT NULL,
  confidence double DEFAULT NULL,
  last_used_at bigint DEFAULT NULL,
  expires_at bigint DEFAULT NULL,
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
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE bot_memories ADD COLUMN memory_type varchar(32) DEFAULT NULL`)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	for _, stmt := range []string{
		`ALTER TABLE bot_memories ADD COLUMN importance int DEFAULT NULL`,
		`ALTER TABLE bot_memories ADD COLUMN confidence double DEFAULT NULL`,
		`ALTER TABLE bot_memories ADD COLUMN last_used_at bigint DEFAULT NULL`,
		`ALTER TABLE bot_memories ADD COLUMN expires_at bigint DEFAULT NULL`,
		fmt.Sprintf(`ALTER TABLE bot_memories ADD COLUMN embedding vector(%d) DEFAULT NULL`, s.embeddingDimension),
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
	}
	_, err = s.db.Exec(fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS bot_media_assets (
  id varchar(64) NOT NULL,
  profile_id varchar(128) NOT NULL,
  media_key varchar(255) NOT NULL,
  content_hash varchar(64) NOT NULL,
  relative_path text NOT NULL,
  source_path text DEFAULT NULL,
  mime_type varchar(128) DEFAULT NULL,
  file_size bigint DEFAULT NULL,
  sender varchar(255) DEFAULT NULL,
  sent_at bigint DEFAULT NULL,
  ocr_text longtext DEFAULT NULL,
  caption longtext DEFAULT NULL,
  visibility varchar(32) NOT NULL,
  status varchar(16) NOT NULL DEFAULT 'valid',
  embedding vector(%d) DEFAULT NULL,
  created_at bigint NOT NULL,
  updated_at timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY idx_profile_media_key (profile_id, media_key),
  KEY idx_profile_media_sent (profile_id, sent_at),
  KEY idx_media_content_hash (content_hash)
) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`, s.embeddingDimension))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS relationship_state (
  profile_id varchar(64) NOT NULL,
  mood_score int DEFAULT 80,
  affinity_score int DEFAULT 80,
  last_sentiment varchar(32) DEFAULT 'neutral',
  updated_at bigint DEFAULT NULL,
  PRIMARY KEY (profile_id)
) DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS image_hash_cache (
  image_hash varchar(64) NOT NULL,
  ocr_text longtext DEFAULT NULL,
  caption longtext DEFAULT NULL,
  created_at bigint DEFAULT NULL,
  PRIMARY KEY (image_hash)
) DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS bot_media_path_status (
  profile_id varchar(128) NOT NULL,
  media_key varchar(255) NOT NULL,
  file_path text NOT NULL,
  status varchar(16) NOT NULL,
  checked_at bigint NOT NULL,
  PRIMARY KEY (profile_id, media_key)
) DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS knowledge_entities (
  id varchar(64) NOT NULL,
  name varchar(255) NOT NULL,
  category varchar(64) NOT NULL,
  description text DEFAULT NULL,
  created_at bigint DEFAULT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY idx_name_cat (name, category)
) DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS knowledge_relations (
  id varchar(64) NOT NULL,
  src_id varchar(64) NOT NULL,
  relation varchar(64) NOT NULL,
  dst_id varchar(64) NOT NULL,
  confidence double DEFAULT 1.0,
  created_at bigint DEFAULT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY idx_triple (src_id, relation, dst_id),
  KEY idx_src (src_id),
  KEY idx_dst (dst_id)
) DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS bot_configs (
  config_key varchar(64) NOT NULL,
  config_value varchar(255) NOT NULL,
  updated_at bigint DEFAULT NULL,
  PRIMARY KEY (config_key)
) DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
CREATE TABLE IF NOT EXISTS relationship_history (
  id varchar(64) NOT NULL,
  profile_id varchar(64) NOT NULL,
  mood_score int DEFAULT 80,
  affinity_score int DEFAULT 80,
  sentiment varchar(32) DEFAULT 'neutral',
  created_at bigint DEFAULT NULL,
  PRIMARY KEY (id),
  KEY idx_profile_created (profile_id, created_at)
) DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return err
	}
	return nil
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
	m.MemoryType = NormalizeMemoryType(m.MemoryType, m.Content)
	m.Importance = NormalizeImportance(m.Importance, m.MemoryType)
	m.Confidence = NormalizeConfidence(m.Confidence, m.MemoryType)
	m.ExpiresAt = NormalizeExpiresAt(m.ExpiresAt, m.MemoryType)

	_, err := s.db.Exec(`
INSERT INTO bot_memories
  (id, profile_id, content, raw, sender, category, memory_type, importance, confidence, last_used_at, expires_at, visibility, source_type, hash, created_at, embedding)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
ON DUPLICATE KEY UPDATE
  content=VALUES(content), raw=VALUES(raw), sender=VALUES(sender), category=VALUES(category),
  memory_type=VALUES(memory_type), importance=VALUES(importance), confidence=VALUES(confidence),
  last_used_at=VALUES(last_used_at), expires_at=VALUES(expires_at),
  visibility=VALUES(visibility), source_type=VALUES(source_type), hash=VALUES(hash)`,
		m.ID, s.profileID, m.Content, m.Raw, m.Sender, m.Category, string(m.MemoryType), m.Importance, m.Confidence, m.LastUsedAt, m.ExpiresAt, string(m.Visibility), m.SourceType, m.Hash, m.CreatedAt)
	if err == nil {
		if s.embedder != nil && s.embedQueue != nil {
			select {
			case s.embedQueue <- m.ID:
			default:
				// If channel is full, log or discard to prevent block
			}
		}
	}
	return err
}

func (s *DatabaseStore) startEmbeddingWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-s.embedQueue:
			if !ok {
				return
			}
			var content string
			err := s.db.QueryRow(`SELECT content FROM bot_memories WHERE profile_id=? AND id=?`, s.profileID, id).Scan(&content)
			if err != nil || content == "" {
				continue
			}
			if s.embedder != nil {
				vec, err := s.embedder.Embed(content)
				if err != nil {
					log.Printf("[向量索引] 生成记忆 embedding 失败 id=%s: %v", id, err)
					continue
				}
				if len(vec) > 0 && len(vec) != s.embeddingDimension {
					log.Printf("[向量索引] 跳过维度不匹配的 embedding id=%s got=%d want=%d", id, len(vec), s.embeddingDimension)
					continue
				}
				if len(vec) > 0 {
					if _, err := s.db.Exec(`UPDATE bot_memories SET embedding=? WHERE profile_id=? AND id=?`, vectorLiteral(vec), s.profileID, id); err != nil {
						log.Printf("[向量索引] 写入记忆 embedding 失败 id=%s dim=%d: %v", id, len(vec), err)
					}
				}
			}
		}
	}
}

func (s *DatabaseStore) enqueueMissingEmbeddings() {
	if s.embedder == nil || s.embedQueue == nil {
		return
	}
	rows, err := s.db.Query(`SELECT id FROM bot_memories WHERE profile_id=? AND embedding IS NULL ORDER BY created_at ASC LIMIT 500`, s.profileID)
	if err != nil {
		log.Printf("[向量索引] 查询待补记忆失败: %v", err)
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		select {
		case s.embedQueue <- id:
		default:
			return
		}
	}
}

func (s *DatabaseStore) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM bot_memories WHERE profile_id=? AND id=?`, s.profileID, id)
	return err
}

func (s *DatabaseStore) All() []Memory {
	rows, err := s.db.Query(`
SELECT id, content, COALESCE(raw, ''), COALESCE(sender, ''), COALESCE(category, ''), COALESCE(memory_type, ''),
       COALESCE(importance, 0), COALESCE(confidence, 0), COALESCE(last_used_at, 0), COALESCE(expires_at, 0),
       visibility, COALESCE(source_type, ''), COALESCE(hash, ''), created_at
FROM bot_memories
WHERE profile_id=?
ORDER BY created_at DESC`, s.profileID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var out []Memory
	for rows.Next() {
		var m Memory
		var memoryType string
		var visibility string
		if err := rows.Scan(&m.ID, &m.Content, &m.Raw, &m.Sender, &m.Category, &memoryType, &m.Importance, &m.Confidence, &m.LastUsedAt, &m.ExpiresAt, &visibility, &m.SourceType, &m.Hash, &m.CreatedAt); err == nil {
			m.MemoryType = NormalizeMemoryType(MemoryType(memoryType), m.Content)
			m.Importance = NormalizeImportance(m.Importance, m.MemoryType)
			m.Confidence = NormalizeConfidence(m.Confidence, m.MemoryType)
			m.ExpiresAt = NormalizeExpiresAt(m.ExpiresAt, m.MemoryType)
			m.Visibility = Visibility(visibility)
			out = append(out, m)
		}
	}
	return out
}

func (s *DatabaseStore) Search(query string, audience string) []string {
	results := s.SearchRelevant(query, audience)
	out := make([]string, 0, len(results))
	for _, item := range results {
		out = append(out, item.Text)
	}
	return out
}

func (s *DatabaseStore) SearchRelevant(query string, audience string) []RetrievedMemory {
	return s.SearchRelevantWithOptions(query, audience, RetrievalOptions{
		TopK: 8, IncludeBotMemory: true, IncludeChatArchive: s.includeChatArchive, IncludeMediaArchive: s.includeMediaArchive,
	})
}

func (s *DatabaseStore) SearchRelevantWithOptions(query string, audience string, opts RetrievalOptions) []RetrievedMemory {
	var results []RetrievedMemory
	if opts.TopK <= 0 {
		opts.TopK = 8
	}
	if opts.TopK > 20 {
		opts.TopK = 20
	}
	vecLiteral, _ := s.queryVectorLiteral(query)
	chatEnabled := opts.IncludeChatArchive && s.includeChatArchive && s.chatVisibleTo(audience)
	mediaEnabled := opts.IncludeMediaArchive && s.includeMediaArchive && s.mediaVisibleTo(audience)
	botLimit := opts.TopK
	if chatEnabled {
		botLimit -= minInt(2, botLimit)
	}
	if mediaEnabled && botLimit > 0 {
		botLimit--
	}
	if opts.IncludeBotMemory && botLimit > 0 {
		results = append(results, s.searchBotMemories(query, audience, botLimit, vecLiteral)...)
	}
	if chatEnabled {
		if vecLiteral != "" {
			results = append(results, wrapRetrievedTexts(s.searchChatArchiveHybrid(query, vecLiteral, 2), MemoryTypeArchiveChat, "chat_archive")...)
		} else {
			results = append(results, wrapRetrievedTexts(s.searchChatArchive(query, 2), MemoryTypeArchiveChat, "chat_archive")...)
		}
	}
	if mediaEnabled {
		for _, media := range s.searchMedia(query, audience, 1, vecLiteral) {
			results = append(results, RetrievedMemory{
				Text:       media.ContextText(),
				MemoryType: MemoryTypeArchiveMedia,
				SourceType: "media_archive",
			})
		}
	}
	// Each source has already ranked its own candidates by hybrid relevance.
	// Re-sorting here by memory type/importance would destroy that ordering
	// (for example, an exact episodic answer could be pushed behind vague
	// relational memories). Keep retrieval relevance as the primary signal.
	if len(results) > opts.TopK {
		results = results[:opts.TopK]
	}
	s.touchRetrieved(results)
	return results
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func wrapRetrievedTexts(texts []string, mt MemoryType, source string) []RetrievedMemory {
	results := make([]RetrievedMemory, 0, len(texts))
	for _, text := range texts {
		results = append(results, RetrievedMemory{
			Text:       text,
			MemoryType: mt,
			SourceType: source,
			Importance: NormalizeImportance(0, mt),
			Confidence: NormalizeConfidence(0, mt),
		})
	}
	return results
}

func (s *DatabaseStore) queryVectorLiteral(query string) (string, bool) {
	query = strings.TrimSpace(query)
	if query == "" || s.embedder == nil {
		return "", false
	}
	now := time.Now()
	s.queryVectorMu.Lock()
	if cached, ok := s.queryVectorCache[query]; ok && now.Before(cached.expiresAt) {
		s.queryVectorMu.Unlock()
		return cached.value, true
	}
	s.queryVectorMu.Unlock()
	vec, err := s.embedder.Embed(query)
	if err != nil || len(vec) == 0 {
		if err != nil {
			log.Printf("[向量检索] 查询 embedding 失败: %v", err)
		}
		return "", false
	}
	if len(vec) != s.embeddingDimension {
		log.Printf("[向量检索] 查询 embedding 维度不匹配 got=%d want=%d", len(vec), s.embeddingDimension)
		return "", false
	}
	literal := vectorLiteral(vec)
	s.queryVectorMu.Lock()
	if len(s.queryVectorCache) >= 128 {
		for key, cached := range s.queryVectorCache {
			if now.After(cached.expiresAt) {
				delete(s.queryVectorCache, key)
			}
		}
		if len(s.queryVectorCache) >= 128 {
			for key := range s.queryVectorCache {
				delete(s.queryVectorCache, key)
				break
			}
		}
	}
	s.queryVectorCache[query] = cachedVectorLiteral{value: literal, expiresAt: now.Add(15 * time.Minute)}
	s.queryVectorMu.Unlock()
	return literal, true
}

func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.Grow(len(vec) * 12)
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', 8, 32))
	}
	b.WriteByte(']')
	return b.String()
}

type scoredMemoryResult struct {
	result       RetrievedMemory
	vectorScore  float64
	ftScore      float64
	semanticRank int
	keywordRank  int
	score        float64
}

func (s *DatabaseStore) searchBotMemories(query string, audience string, limit int, vecLiteral string) []RetrievedMemory {
	visibility := allowedVisibility(audience)
	if len(visibility) == 0 {
		return nil
	}

	// If no embedding is available, fallback to standard text search
	if vecLiteral == "" {
		return s.searchBotMemoriesTextOnly(query, visibility, limit)
	}

	// Perform Hybrid Search (Vector + FullText)
	pool := limit * 4
	if pool < 20 {
		pool = 20
	}

	results := make(map[string]*scoredMemoryResult)

	// Way 1: Vector similarity path
	args := []interface{}{s.profileID}
	placeholders := make([]string, 0, len(visibility))
	for _, v := range visibility {
		placeholders = append(placeholders, "?")
		args = append(args, string(v))
	}
	where := fmt.Sprintf("embedding IS NOT NULL AND profile_id=? AND visibility IN (%s) AND (expires_at IS NULL OR expires_at=0 OR expires_at>?)", strings.Join(placeholders, ","))
	args = append(args, time.Now().Unix())
	args = append(args, pool)

	semanticRows, err := s.db.Query(fmt.Sprintf(`
SELECT id, content, COALESCE(memory_type, ''), COALESCE(source_type, ''), COALESCE(importance, 0), COALESCE(confidence, 0), COALESCE(last_used_at, 0), COALESCE(expires_at, 0), created_at,
       cosine_distance(embedding, ?) AS vector_distance,
       MATCH(content) AGAINST (?) AS ft_score
FROM bot_memories
WHERE %s
ORDER BY vector_distance ASC
LIMIT ?`, where), append([]interface{}{vecLiteral, query}, args...)...)
	if err == nil {
		defer func() { _ = semanticRows.Close() }()
		rank := 0
		for semanticRows.Next() {
			rank++
			var m RetrievedMemory
			var distance, ftScore float64
			var memoryType string
			if err := semanticRows.Scan(&m.ID, &m.Text, &memoryType, &m.SourceType, &m.Importance, &m.Confidence, &m.LastUsedAt, &m.ExpiresAt, &m.CreatedAt, &distance, &ftScore); err == nil {
				m.MemoryType = NormalizeMemoryType(MemoryType(memoryType), m.Text)
				m.Importance = NormalizeImportance(m.Importance, m.MemoryType)
				m.Confidence = NormalizeConfidence(m.Confidence, m.MemoryType)
				m.ExpiresAt = NormalizeExpiresAt(m.ExpiresAt, m.MemoryType)
				results[m.ID] = &scoredMemoryResult{result: m, semanticRank: rank, vectorScore: 1 - distance, ftScore: ftScore}
			}
		}
	} else {
		log.Printf("[向量检索] bot_memories 语义召回失败，继续走关键词召回: %v", err)
	}

	// Way 2: Keyword Fulltext path
	args2 := []interface{}{s.profileID}
	placeholders2 := make([]string, 0, len(visibility))
	for _, v := range visibility {
		placeholders2 = append(placeholders2, "?")
		args2 = append(args2, string(v))
	}
	where2 := fmt.Sprintf("profile_id=? AND visibility IN (%s) AND (expires_at IS NULL OR expires_at=0 OR expires_at>?)", strings.Join(placeholders2, ","))
	args2 = append(args2, time.Now().Unix())
	args2 = append(args2, query, "%"+query+"%", pool)

	keywordRows, err := s.db.Query(fmt.Sprintf(`
SELECT id, content, COALESCE(memory_type, ''), COALESCE(source_type, ''), COALESCE(importance, 0), COALESCE(confidence, 0), COALESCE(last_used_at, 0), COALESCE(expires_at, 0), created_at,
       cosine_distance(embedding, ?) AS vector_distance,
       MATCH(content) AGAINST (?) AS ft_score
FROM bot_memories
WHERE %s AND (MATCH(content) AGAINST (? IN NATURAL LANGUAGE MODE) OR content LIKE ?)
ORDER BY ft_score DESC
LIMIT ?`, where2), append([]interface{}{vecLiteral, query}, args2...)...)
	if err == nil {
		defer func() { _ = keywordRows.Close() }()
		rank := 0
		for keywordRows.Next() {
			rank++
			var m RetrievedMemory
			var distance, ftScore float64
			var memoryType string
			if err := keywordRows.Scan(&m.ID, &m.Text, &memoryType, &m.SourceType, &m.Importance, &m.Confidence, &m.LastUsedAt, &m.ExpiresAt, &m.CreatedAt, &distance, &ftScore); err == nil {
				m.MemoryType = NormalizeMemoryType(MemoryType(memoryType), m.Text)
				m.Importance = NormalizeImportance(m.Importance, m.MemoryType)
				m.Confidence = NormalizeConfidence(m.Confidence, m.MemoryType)
				m.ExpiresAt = NormalizeExpiresAt(m.ExpiresAt, m.MemoryType)
				item := results[m.ID]
				if item == nil {
					item = &scoredMemoryResult{result: m, vectorScore: 1 - distance}
					results[m.ID] = item
				}
				item.keywordRank = rank
				item.ftScore = ftScore
			}
		}
	} else {
		log.Printf("[向量检索] bot_memories 关键词召回失败: %v", err)
	}

	// Weighted reciprocal-rank fusion is stable across vector and full-text
	// score scales. The rank constant 60 matches OceanBase's RRF examples.
	ranked := make([]*scoredMemoryResult, 0, len(results))
	for _, item := range results {
		item.score = weightedRRF(item.semanticRank, item.keywordRank, 0.65, 0.35)
		item.score += float64(item.result.Importance) * 0.00005
		item.score += item.result.Confidence * 0.0001
		ranked = append(ranked, item)
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	var out []RetrievedMemory
	for i := 0; i < len(ranked) && i < limit; i++ {
		out = append(out, ranked[i].result)
	}
	return out
}

func (s *DatabaseStore) searchBotMemoriesTextOnly(query string, visibility []Visibility, limit int) []RetrievedMemory {
	args := []interface{}{s.profileID}
	placeholders := make([]string, 0, len(visibility))
	for _, v := range visibility {
		placeholders = append(placeholders, "?")
		args = append(args, string(v))
	}

	where := fmt.Sprintf("profile_id=? AND visibility IN (%s) AND (expires_at IS NULL OR expires_at=0 OR expires_at>?)", strings.Join(placeholders, ","))
	args = append(args, time.Now().Unix())
	if strings.TrimSpace(query) != "" {
		where += " AND (MATCH(content) AGAINST (? IN NATURAL LANGUAGE MODE) OR content LIKE ?)"
		args = append(args, query, "%"+query+"%")
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
SELECT id, content, COALESCE(memory_type, ''), COALESCE(source_type, ''), COALESCE(importance, 0), COALESCE(confidence, 0), COALESCE(last_used_at, 0), COALESCE(expires_at, 0), created_at
FROM bot_memories
WHERE `+where+`
ORDER BY importance DESC, confidence DESC, created_at DESC
LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var out []RetrievedMemory
	for rows.Next() {
		var id, content, memoryType, sourceType string
		var importance int
		var confidence float64
		var lastUsedAt, expiresAt, createdAt int64
		if err := rows.Scan(&id, &content, &memoryType, &sourceType, &importance, &confidence, &lastUsedAt, &expiresAt, &createdAt); err == nil {
			out = append(out, RetrievedMemory{
				ID:         id,
				Text:       content,
				MemoryType: NormalizeMemoryType(MemoryType(memoryType), content),
				SourceType: sourceType,
				Importance: NormalizeImportance(importance, MemoryType(memoryType)),
				Confidence: NormalizeConfidence(confidence, MemoryType(memoryType)),
				LastUsedAt: lastUsedAt,
				ExpiresAt:  NormalizeExpiresAt(expiresAt, MemoryType(memoryType)),
				CreatedAt:  createdAt,
			})
		}
	}
	return out
}

func (s *DatabaseStore) touchRetrieved(results []RetrievedMemory) {
	now := time.Now().Unix()
	for _, item := range results {
		if item.ID == "" || item.SourceType == "chat_archive" || item.SourceType == "media_archive" {
			continue
		}
		_, _ = s.db.Exec(`UPDATE bot_memories SET last_used_at=? WHERE profile_id=? AND id=?`, now, s.profileID, item.ID)
	}
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
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err == nil {
			out = append(out, content)
		}
	}
	return out
}

type scoredTextResult struct {
	key          string
	text         string
	vectorScore  float64
	ftScore      float64
	semanticRank int
	keywordRank  int
	score        float64
}

func (s *DatabaseStore) searchChatArchiveHybrid(query string, vecLiteral string, limit int) []string {
	if strings.TrimSpace(query) == "" || vecLiteral == "" {
		return nil
	}
	pool := limit * 20
	if pool < 80 {
		pool = 80
	}
	if pool > 300 {
		pool = 300
	}

	results := make(map[string]*scoredTextResult)
	semanticRows, err := s.db.Query(fmt.Sprintf(`
SELECT id,
       CONCAT('[聊天记录 ', DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'), '] ', %s),
       cosine_distance(embedding, ?) AS vector_distance,
       MATCH(%s) AGAINST (?) AS ft_score
FROM %s
WHERE embedding IS NOT NULL
ORDER BY vector_distance ASC
LIMIT ?`,
		s.chatArchiveTimeColumn, s.chatArchiveTextColumn,
		s.chatArchiveTextColumn,
		s.chatArchiveTable,
	), vecLiteral, query, pool)
	if err == nil {
		defer func() { _ = semanticRows.Close() }()
		rank := 0
		for semanticRows.Next() {
			rank++
			var id int64
			var text string
			var distance, ftScore float64
			if err := semanticRows.Scan(&id, &text, &distance, &ftScore); err != nil {
				continue
			}
			key := fmt.Sprint(id)
			item := &scoredTextResult{key: key, text: text, semanticRank: rank, vectorScore: 1 - distance, ftScore: ftScore}
			results[key] = item
		}
	} else {
		return s.searchChatArchive(query, limit)
	}

	keywordRows, err := s.db.Query(fmt.Sprintf(`
SELECT id,
       CONCAT('[聊天记录 ', DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'), '] ', %s),
       cosine_distance(embedding, ?) AS vector_distance,
       MATCH(%s) AGAINST (?) AS ft_score
FROM %s
WHERE MATCH(%s) AGAINST (? IN NATURAL LANGUAGE MODE)
ORDER BY ft_score DESC
LIMIT ?`,
		s.chatArchiveTimeColumn, s.chatArchiveTextColumn,
		s.chatArchiveTextColumn,
		s.chatArchiveTable,
		s.chatArchiveTextColumn,
	), vecLiteral, query, query, pool)
	if err == nil {
		defer func() { _ = keywordRows.Close() }()
		rank := 0
		for keywordRows.Next() {
			rank++
			var id int64
			var text string
			var distance, ftScore float64
			if err := keywordRows.Scan(&id, &text, &distance, &ftScore); err != nil {
				continue
			}
			key := fmt.Sprint(id)
			item := results[key]
			if item == nil {
				item = &scoredTextResult{key: key, text: text, vectorScore: 1 - distance}
				results[key] = item
			}
			item.keywordRank = rank
			item.ftScore = ftScore
		}
	}

	ranked := make([]*scoredTextResult, 0, len(results))
	for _, item := range results {
		item.score = weightedRRF(item.semanticRank, item.keywordRank, 0.65, 0.35)
		ranked = append(ranked, item)
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	out := make([]string, 0, limit)
	seen := make(map[string]struct{})
	for _, item := range ranked {
		text := strings.TrimSpace(item.text)
		if text == "" {
			continue
		}
		key := text
		if len(key) > 120 {
			key = key[:120]
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, text)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (s *DatabaseStore) SearchMedia(query string, audience string, limit int) []MediaResult {
	vecLiteral, _ := s.queryVectorLiteral(cleanMediaQuery(query))
	return s.searchMedia(query, audience, limit, vecLiteral)
}

func (s *DatabaseStore) searchMedia(query string, audience string, limit int, vecLiteral string) []MediaResult {
	if !s.includeMediaArchive || !s.mediaVisibleTo(audience) {
		return nil
	}
	if limit <= 0 {
		limit = 3
	}
	if limit > 10 {
		limit = 10
	}
	managed := s.searchManagedMedia(query, audience, limit, vecLiteral)
	if len(managed) >= limit {
		return managed[:limit]
	}
	term := cleanMediaQuery(query)
	if term == "" {
		return mergeMediaResults(managed, s.filterMediaPaths(s.recentMedia(limit)), limit)
	}
	var results []MediaResult
	if vecLiteral != "" {
		results = s.searchMediaHybrid(term, vecLiteral, limit)
	}
	if len(results) == 0 {
		results = s.searchMediaFullText(term, limit)
	}
	if len(results) == 0 {
		results = s.searchMediaLike(term, limit)
	}
	if len(results) == 0 && isVagueMediaQuery(query) {
		results = s.recentMedia(limit)
	}
	return mergeMediaResults(managed, s.filterMediaPaths(results), limit)
}

func mergeMediaResults(primary, secondary []MediaResult, limit int) []MediaResult {
	out := make([]MediaResult, 0, limit)
	seen := make(map[string]struct{})
	for _, group := range [][]MediaResult{primary, secondary} {
		for _, item := range group {
			key := mediaResultKey(item)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, item)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// detectMediaStatusColumn enables durable invalid-path marking when the
// archive table exposes the optional status column. Older archives continue
// to work with in-process filtering.
func (s *DatabaseStore) detectMediaStatusColumn() {
	var name string
	err := s.db.QueryRow("SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=? LIMIT 1", s.mediaArchiveTable, s.mediaStatusColumn).Scan(&name)
	if err != nil {
		s.mediaStatusColumn = ""
	}
}

func (s *DatabaseStore) filterMediaPaths(results []MediaResult) []MediaResult {
	valid := make([]MediaResult, 0, len(results))
	for _, result := range results {
		path := strings.TrimSpace(result.FilePath)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) && s.mediaRoot != "" {
			path = filepath.Join(s.mediaRoot, path)
			result.FilePath = path
		}
		key := mediaResultKey(result)
		var recordedPath, recordedStatus string
		_ = s.db.QueryRow(`SELECT file_path, status FROM bot_media_path_status WHERE profile_id=? AND media_key=?`, s.profileID, key).Scan(&recordedPath, &recordedStatus)
		if _, err := os.Stat(path); err != nil {
			s.saveMediaPathStatus(key, path, "invalid")
			s.markMediaPathInvalid(result)
			continue
		}
		if recordedStatus == "invalid" && recordedPath != path {
			logPathRepair(s.profileID, key, recordedPath, path)
		}
		s.saveMediaPathStatus(key, path, "valid")
		if s.mediaStatusColumn != "" {
			_, _ = s.db.Exec(fmt.Sprintf("UPDATE %s SET %s=? WHERE %s=?", s.mediaArchiveTable, s.mediaStatusColumn, s.mediaMsgIDColumn), "valid", result.MsgID)
		}
		valid = append(valid, result)
	}
	return valid
}

func (s *DatabaseStore) saveMediaPathStatus(key, path, status string) {
	if key == "" || path == "" {
		return
	}
	_, _ = s.db.Exec(`
INSERT INTO bot_media_path_status (profile_id, media_key, file_path, status, checked_at)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE file_path=VALUES(file_path), status=VALUES(status), checked_at=VALUES(checked_at)`,
		s.profileID, key, path, status, time.Now().Unix())
}

func logPathRepair(profileID, key, oldPath, newPath string) {
	if oldPath != "" && newPath != "" && oldPath != newPath {
		log.Printf("[媒体归档] 路径已恢复 profile=%s key=%s old=%s new=%s", profileID, key, oldPath, newPath)
	}
}

func (s *DatabaseStore) markMediaPathInvalid(result MediaResult) {
	if s.mediaStatusColumn == "" || result.MsgID == "" {
		return
	}
	_, _ = s.db.Exec(fmt.Sprintf("UPDATE %s SET %s=? WHERE %s=?", s.mediaArchiveTable, s.mediaStatusColumn, s.mediaMsgIDColumn), "invalid", result.MsgID)
}

func (s *DatabaseStore) auditMediaPaths(ctx context.Context) MediaPathDiagnostic {
	var result MediaPathDiagnostic
	if !s.includeMediaArchive {
		return result
	}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("SELECT COALESCE(%s, ''), COALESCE(%s, '') FROM %s WHERE COALESCE(%s, '')<>''", s.mediaFilePathColumn, s.mediaMsgIDColumn, s.mediaArchiveTable, s.mediaFilePathColumn))
	if err != nil {
		return result
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path, msgID string
		if rows.Scan(&path, &msgID) != nil {
			continue
		}
		result.Checked++
		resolved := strings.TrimSpace(path)
		if !filepath.IsAbs(resolved) {
			if s.mediaRoot == "" {
				result.Unresolved++
				continue
			}
			resolved = filepath.Join(s.mediaRoot, resolved)
		}
		media := MediaResult{MsgID: msgID, FilePath: resolved}
		key := mediaResultKey(media)
		if _, err := os.Stat(resolved); err != nil {
			result.Missing++
			if result.ExampleMissing == "" {
				result.ExampleMissing = resolved
			}
			s.saveMediaPathStatus(key, resolved, "invalid")
			s.markMediaPathInvalid(media)
			continue
		}
		result.Valid++
		s.saveMediaPathStatus(key, resolved, "valid")
	}
	return result
}

type scoredMediaResult struct {
	result       MediaResult
	vectorScore  float64
	ftScore      float64
	semanticRank int
	keywordRank  int
	score        float64
}

func (s *DatabaseStore) searchMediaHybrid(query string, vecLiteral string, limit int) []MediaResult {
	if strings.TrimSpace(query) == "" || vecLiteral == "" {
		return nil
	}
	pool := limit * 20
	if pool < 80 {
		pool = 80
	}
	if pool > 300 {
		pool = 300
	}

	results := make(map[string]*scoredMediaResult)
	semanticRows, err := s.db.Query(fmt.Sprintf(`
SELECT COALESCE(%s, ''), COALESCE(%s, ''), DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'),
       COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(has_text, 0),
       cosine_distance(embedding, ?) AS vector_distance,
       MATCH(%s, %s) AGAINST (?) AS ft_score
FROM %s
WHERE embedding IS NOT NULL
ORDER BY vector_distance ASC
LIMIT ?`,
		s.mediaMsgIDColumn, s.mediaSenderColumn, s.mediaTimeColumn,
		s.mediaFilePathColumn, s.mediaOCRColumn, s.mediaCaptionColumn, s.mediaMsgIDColumn,
		s.mediaOCRColumn, s.mediaCaptionColumn,
		s.mediaArchiveTable,
	), vecLiteral, query, pool)
	if err == nil {
		defer func() { _ = semanticRows.Close() }()
		rank := 0
		for semanticRows.Next() {
			rank++
			result, distance, ftScore, ok := scanScoredMediaRow(semanticRows)
			if !ok {
				continue
			}
			key := mediaResultKey(result)
			results[key] = &scoredMediaResult{result: result, semanticRank: rank, vectorScore: 1 - distance, ftScore: ftScore}
		}
	} else {
		return nil
	}

	keywordRows, err := s.db.Query(fmt.Sprintf(`
SELECT COALESCE(%s, ''), COALESCE(%s, ''), DATE_FORMAT(%s, '%%Y-%%m-%%d %%H:%%i'),
       COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(%s, ''), COALESCE(has_text, 0),
       cosine_distance(embedding, ?) AS vector_distance,
       MATCH(%s, %s) AGAINST (?) AS ft_score
FROM %s
WHERE MATCH(%s, %s) AGAINST (? IN NATURAL LANGUAGE MODE)
ORDER BY ft_score DESC
LIMIT ?`,
		s.mediaMsgIDColumn, s.mediaSenderColumn, s.mediaTimeColumn,
		s.mediaFilePathColumn, s.mediaOCRColumn, s.mediaCaptionColumn, s.mediaMsgIDColumn,
		s.mediaOCRColumn, s.mediaCaptionColumn,
		s.mediaArchiveTable,
		s.mediaOCRColumn, s.mediaCaptionColumn,
	), vecLiteral, query, query, pool)
	if err == nil {
		defer func() { _ = keywordRows.Close() }()
		rank := 0
		for keywordRows.Next() {
			rank++
			result, distance, ftScore, ok := scanScoredMediaRow(keywordRows)
			if !ok {
				continue
			}
			key := mediaResultKey(result)
			item := results[key]
			if item == nil {
				item = &scoredMediaResult{result: result, vectorScore: 1 - distance}
				results[key] = item
			}
			item.keywordRank = rank
			item.ftScore = ftScore
		}
	}

	ranked := make([]*scoredMediaResult, 0, len(results))
	for _, item := range results {
		item.score = weightedRRF(item.semanticRank, item.keywordRank, 0.70, 0.30)
		ranked = append(ranked, item)
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	out := make([]MediaResult, 0, limit)
	seen := make(map[string]struct{})
	for _, item := range ranked {
		key := mediaResultKey(item.result)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item.result)
		if len(out) >= limit {
			break
		}
	}
	return out
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
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

func scanScoredMediaRow(rows *sql.Rows) (MediaResult, float64, float64, bool) {
	var result MediaResult
	var hasText int
	var distance, ftScore float64
	if err := rows.Scan(&result.MsgID, &result.Sender, &result.SentAt, &result.FilePath, &result.OCRText, &result.Caption, &result.MessageID, &hasText, &distance, &ftScore); err != nil {
		return MediaResult{}, 0, 0, false
	}
	result.HasText = hasText != 0
	return result, distance, ftScore, true
}

func mediaResultKey(result MediaResult) string {
	if result.MsgID != "" {
		return result.MsgID
	}
	if result.MessageID != "" {
		return result.MessageID
	}
	return result.SentAt + "|" + result.FilePath
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

func weightedRRF(semanticRank, keywordRank int, semanticWeight, keywordWeight float64) float64 {
	const rankConstant = 60.0
	var score float64
	if semanticRank > 0 {
		score += semanticWeight / (rankConstant + float64(semanticRank))
	}
	if keywordRank > 0 {
		score += keywordWeight / (rankConstant + float64(keywordRank))
	}
	return score
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
	if s.embedCancel != nil {
		s.embedCancel()
	}
	return s.db.Close()
}

func (s *DatabaseStore) GetDBConn() *sql.DB {
	return s.db
}

// Diagnostics returns schema-level vector health without exposing memory or
// archive contents. It is used by memtool and the health workflow.
func (s *DatabaseStore) Diagnostics() DatabaseDiagnostics {
	var result DatabaseDiagnostics
	_ = s.db.QueryRow("SELECT VERSION()").Scan(&result.ServerVersion)
	vectorRows, err := s.db.Query(`
SELECT DISTINCT TABLE_NAME
FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_SCHEMA=DATABASE() AND LOWER(COLUMN_TYPE) LIKE 'vector%'
ORDER BY TABLE_NAME`)
	if err == nil {
		for vectorRows.Next() {
			var table string
			if vectorRows.Scan(&table) == nil {
				result.DiscoveredVectorTables = append(result.DiscoveredVectorTables, table)
			}
		}
		_ = vectorRows.Close()
	}
	tableRows, err := s.db.Query(`
SELECT TABLE_NAME
FROM INFORMATION_SCHEMA.TABLES
WHERE TABLE_SCHEMA=DATABASE() AND TABLE_TYPE='BASE TABLE'
ORDER BY TABLE_NAME`)
	if err == nil {
		for tableRows.Next() {
			var table string
			if tableRows.Scan(&table) == nil {
				result.AvailableTables = append(result.AvailableTables, table)
			}
		}
		_ = tableRows.Close()
	}
	tables := []string{"bot_memories", "bot_media_assets"}
	if s.includeChatArchive {
		tables = append(tables, s.chatArchiveTable)
	}
	if s.includeMediaArchive {
		tables = append(tables, s.mediaArchiveTable)
	}
	seen := make(map[string]bool)
	for _, table := range tables {
		if seen[table] || !isSafeIdentifier(table) {
			continue
		}
		seen[table] = true
		item := VectorDiagnostic{Table: table}
		if err := s.db.QueryRow(fmt.Sprintf("SELECT COUNT(*), COALESCE(SUM(embedding IS NOT NULL), 0) FROM %s", table)).Scan(&item.Rows, &item.EmbeddedRows); err != nil {
			item.Problem = err.Error()
			result.Tables = append(result.Tables, item)
			continue
		}
		item.Exists = true
		rows, err := s.db.Query(`
SELECT INDEX_NAME, COALESCE(INDEX_TYPE, ''), GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX)
FROM INFORMATION_SCHEMA.STATISTICS
WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?
GROUP BY INDEX_NAME, INDEX_TYPE
ORDER BY INDEX_NAME`, table)
		if err == nil {
			for rows.Next() {
				var name, indexType, columns string
				if rows.Scan(&name, &indexType, &columns) == nil {
					item.Indexes = append(item.Indexes, fmt.Sprintf("%s (%s: %s)", name, indexType, columns))
				}
			}
			_ = rows.Close()
		}
		result.Tables = append(result.Tables, item)
	}
	result.MediaPaths = s.auditMediaPaths(context.Background())
	return result
}

func (s *DatabaseStore) GetProfileID() string {
	return s.profileID
}

func (s *DatabaseStore) GetRelationshipState() (RelationshipState, error) {
	var state RelationshipState
	err := s.db.QueryRow(`
SELECT mood_score, affinity_score, COALESCE(last_sentiment, 'neutral')
FROM relationship_state
WHERE profile_id=?`, s.profileID).Scan(&state.MoodScore, &state.AffinityScore, &state.LastSentiment)
	if err == sql.ErrNoRows {
		return RelationshipState{MoodScore: 80, AffinityScore: 80, LastSentiment: "neutral"}, nil
	}
	return state, err
}

func (s *DatabaseStore) UpdateRelationshipState(state RelationshipState) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
INSERT INTO relationship_state (profile_id, mood_score, affinity_score, last_sentiment, updated_at)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  mood_score=VALUES(mood_score), affinity_score=VALUES(affinity_score),
  last_sentiment=VALUES(last_sentiment), updated_at=VALUES(updated_at)`,
		s.profileID, state.MoodScore, state.AffinityScore, state.LastSentiment, now)
	if err != nil {
		return err
	}

	histID := fmt.Sprintf("hist_%s", HashContent(fmt.Sprintf("%s_%d_%d_%d", s.profileID, now, state.MoodScore, state.AffinityScore))[:16])
	_, _ = s.db.Exec(`
INSERT INTO relationship_history (id, profile_id, mood_score, affinity_score, sentiment, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		histID, s.profileID, state.MoodScore, state.AffinityScore, state.LastSentiment, now)

	return nil
}

func (s *DatabaseStore) GetConfig(key string, defaultVal string) string {
	var val string
	err := s.db.QueryRow(`SELECT config_value FROM bot_configs WHERE config_key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return defaultVal
	}
	if err != nil {
		return defaultVal
	}
	return val
}

func (s *DatabaseStore) SetConfig(key string, value string) error {
	_, err := s.db.Exec(`
INSERT INTO bot_configs (config_key, config_value, updated_at)
VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE config_value = VALUES(config_value), updated_at = VALUES(updated_at)`,
		key, value, time.Now().Unix())
	return err
}

func (s *DatabaseStore) GetEmotionHistory() ([]RelationshipState, []int64, error) {
	rows, err := s.db.Query(`
SELECT mood_score, affinity_score, sentiment, created_at
FROM relationship_history
WHERE profile_id = ?
ORDER BY created_at ASC`, s.profileID)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	var states []RelationshipState
	var times []int64
	for rows.Next() {
		var state RelationshipState
		var t int64
		if err := rows.Scan(&state.MoodScore, &state.AffinityScore, &state.LastSentiment, &t); err == nil {
			states = append(states, state)
			times = append(times, t)
		}
	}
	return states, times, nil
}

func (s *DatabaseStore) GetImageHashCache(hash string) (ocr string, caption string, err error) {
	err = s.db.QueryRow(`
SELECT ocr_text, caption
FROM image_hash_cache
WHERE image_hash=?`, hash).Scan(&ocr, &caption)
	return ocr, caption, err
}

func (s *DatabaseStore) SaveImageHashCache(hash string, ocr string, caption string) error {
	_, err := s.db.Exec(`
INSERT INTO image_hash_cache (image_hash, ocr_text, caption, created_at)
VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  ocr_text=VALUES(ocr_text), caption=VALUES(caption)`,
		hash, ocr, caption, time.Now().Unix())
	return err
}

func (s *DatabaseStore) SaveEntity(name string, category string) (string, error) {
	name = strings.TrimSpace(name)
	category = strings.TrimSpace(category)
	if name == "" || category == "" {
		return "", fmt.Errorf("entity name or category cannot be empty")
	}
	id := fmt.Sprintf("ent_%s", HashContent(name)[:16])
	now := time.Now().Unix()

	_, err := s.db.Exec(`
INSERT INTO knowledge_entities (id, name, category, created_at)
VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE name=VALUES(name), category=VALUES(category)`, id, name, category, now)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *DatabaseStore) SaveRelation(srcName string, relation string, dstName string) error {
	srcName = strings.TrimSpace(srcName)
	dstName = strings.TrimSpace(dstName)
	relation = strings.TrimSpace(relation)
	if srcName == "" || dstName == "" || relation == "" {
		return fmt.Errorf("relation triplet elements cannot be empty")
	}

	srcCat := "person"
	dstCat := "person"
	if relation == "is_alias_of" {
		srcCat = "alias"
	}

	srcId, err := s.SaveEntity(srcName, srcCat)
	if err != nil {
		return err
	}
	dstId, err := s.SaveEntity(dstName, dstCat)
	if err != nil {
		return err
	}

	id := fmt.Sprintf("rel_%s", HashContent(fmt.Sprintf("%s_%s_%s", srcId, relation, dstId))[:16])
	now := time.Now().Unix()

	_, err = s.db.Exec(`
INSERT INTO knowledge_relations (id, src_id, relation, dst_id, confidence, created_at)
VALUES (?, ?, ?, ?, 1.0, ?)
ON DUPLICATE KEY UPDATE confidence=VALUES(confidence)`, id, srcId, relation, dstId, now)
	return err
}

func (s *DatabaseStore) ResolveAliases(entityNames []string) []string {
	if len(entityNames) == 0 {
		return entityNames
	}

	resultMap := make(map[string]bool)
	for _, name := range entityNames {
		resultMap[name] = true
	}

	for _, name := range entityNames {
		var dstName string
		err := s.db.QueryRow(`
SELECT e2.name
FROM knowledge_entities e1
JOIN knowledge_relations r ON e1.id = r.src_id
JOIN knowledge_entities e2 ON r.dst_id = e2.id
WHERE r.relation = 'is_alias_of' AND e1.name = ?`, name).Scan(&dstName)
		if err == nil && dstName != "" {
			resultMap[dstName] = true
		}
	}

	var resolved []string
	for name := range resultMap {
		resolved = append(resolved, name)
	}
	return resolved
}

func (s *DatabaseStore) GetEntityRelations(entityNames []string) []string {
	if len(entityNames) == 0 {
		return nil
	}

	placeholders := make([]string, len(entityNames))
	args := make([]interface{}, len(entityNames)*2)
	for i, name := range entityNames {
		placeholders[i] = "?"
		args[i] = name
		args[len(entityNames)+i] = name
	}
	inClause := strings.Join(placeholders, ",")

	query := fmt.Sprintf(`
SELECT e1.name, r.relation, e2.name
FROM knowledge_relations r
JOIN knowledge_entities e1 ON r.src_id = e1.id
JOIN knowledge_entities e2 ON r.dst_id = e2.id
WHERE e1.name IN (%s) OR e2.name IN (%s)`, inClause, inClause)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var facts []string
	seen := make(map[string]bool)

	for rows.Next() {
		var src, rel, dst string
		if err := rows.Scan(&src, &rel, &dst); err == nil {
			var fact string
			switch rel {
			case "is_alias_of":
				fact = fmt.Sprintf("%s是%s的别名。", src, dst)
			case "mother_of", "mother_is":
				fact = fmt.Sprintf("%s是%s的妈妈。", src, dst)
			case "father_of", "father_is":
				fact = fmt.Sprintf("%s是%s的爸爸。", src, dst)
			case "friend_of":
				fact = fmt.Sprintf("%s和%s是朋友。", src, dst)
			case "colleague_of":
				fact = fmt.Sprintf("%s和%s是同事关系。", src, dst)
			case "likes":
				fact = fmt.Sprintf("%s喜欢%s。", src, dst)
			default:
				fact = fmt.Sprintf("%s和%s的关系是：%s。", src, dst, rel)
			}
			if !seen[fact] {
				seen[fact] = true
				facts = append(facts, fact)
			}
		}
	}
	return facts
}

func (s *DatabaseStore) GetRelationDestinations(srcName string, relation string) ([]string, error) {
	srcName = strings.TrimSpace(srcName)
	relation = strings.TrimSpace(relation)
	if srcName == "" || relation == "" {
		return nil, nil
	}

	rows, err := s.db.Query(`
SELECT e2.name
FROM knowledge_relations r
JOIN knowledge_entities e1 ON r.src_id = e1.id
JOIN knowledge_entities e2 ON r.dst_id = e2.id
WHERE e1.name = ? AND r.relation = ?`, srcName, relation)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var dests []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			dests = append(dests, name)
		}
	}
	return dests, nil
}

func (s *DatabaseStore) DeleteRelation(srcName string, relation string, dstName string) error {
	srcName = strings.TrimSpace(srcName)
	relation = strings.TrimSpace(relation)
	dstName = strings.TrimSpace(dstName)
	if srcName == "" || relation == "" || dstName == "" {
		return fmt.Errorf("relation elements cannot be empty")
	}

	var srcId, dstId string
	err := s.db.QueryRow(`SELECT id FROM knowledge_entities WHERE name = ?`, srcName).Scan(&srcId)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	err = s.db.QueryRow(`SELECT id FROM knowledge_entities WHERE name = ?`, dstName).Scan(&dstId)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}

	_, err = s.db.Exec(`
DELETE FROM knowledge_relations
WHERE src_id = ? AND relation = ? AND dst_id = ?`, srcId, relation, dstId)
	return err
}

var _ MemoryStore = (*DatabaseStore)(nil)
