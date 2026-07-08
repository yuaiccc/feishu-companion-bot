package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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
	Embedder              Embedder
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
	embedder              Embedder
	embedQueue            chan string
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
		embedder:              opts.Embedder,
		embedQueue:            make(chan string, 500),
	}
	if err := store.ensureSchema(); err != nil {
		db.Close()
		return nil, err
	}
	// Start async embedding background worker
	go store.startEmbeddingWorker(context.Background())
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
		`ALTER TABLE bot_memories ADD COLUMN embedding vector(1024) DEFAULT NULL`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return err
		}
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
			err := s.db.QueryRow(`SELECT content FROM bot_memories WHERE id=?`, id).Scan(&content)
			if err != nil || content == "" {
				continue
			}
			if s.embedder != nil {
				vec, err := s.embedder.Embed(content)
				if err == nil && len(vec) > 0 {
					_, _ = s.db.Exec(`UPDATE bot_memories SET embedding=? WHERE id=?`, vectorLiteral(vec), id)
				}
			}
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
	defer rows.Close()

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
	var results []RetrievedMemory
	results = append(results, s.searchBotMemories(query, audience, 6)...)
	if s.includeChatArchive && s.chatVisibleTo(audience) {
		if vec, ok := s.queryVectorLiteral(query); ok {
			results = append(results, wrapRetrievedTexts(s.searchChatArchiveHybrid(query, vec, 3), MemoryTypeArchiveChat, "chat_archive")...)
		} else {
			results = append(results, wrapRetrievedTexts(s.searchChatArchive(query, 3), MemoryTypeArchiveChat, "chat_archive")...)
		}
	}
	if s.includeMediaArchive && s.mediaVisibleTo(audience) {
		for _, media := range s.SearchMedia(query, audience, 2) {
			results = append(results, RetrievedMemory{
				Text:       media.ContextText(),
				MemoryType: MemoryTypeArchiveMedia,
				SourceType: "media_archive",
			})
		}
	}
	if len(results) > 8 {
		results = results[:8]
	}
	sortRetrievedMemories(results)
	s.touchRetrieved(results)
	return results
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
	vec, err := s.embedder.Embed(query)
	if err != nil || len(vec) == 0 {
		return "", false
	}
	return vectorLiteral(vec), true
}

func vectorLiteral(vec []float32) string {
	var b strings.Builder
	b.Grow(len(vec) * 12)
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(fmt.Sprintf("%.8g", v))
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

func (s *DatabaseStore) searchBotMemories(query string, audience string, limit int) []RetrievedMemory {
	visibility := allowedVisibility(audience)
	if len(visibility) == 0 {
		return nil
	}

	// Calculate query embedding vector
	var vecLiteral string
	if s.embedder != nil && strings.TrimSpace(query) != "" {
		if vec, err := s.embedder.Embed(query); err == nil && len(vec) > 0 {
			vecLiteral = vectorLiteral(vec)
		}
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
	args := []interface{}{vecLiteral, s.profileID}
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
		defer semanticRows.Close()
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
	}

	// Way 2: Keyword Fulltext path
	args2 := []interface{}{vecLiteral, s.profileID}
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
		defer keywordRows.Close()
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
	}

	// Mix scoring and RRF reranking
	ranked := make([]*scoredMemoryResult, 0, len(results))
	for _, item := range results {
		keywordScore := 0.0
		if item.ftScore > 0 {
			keywordScore = item.ftScore / (item.ftScore + 10)
		}
		rankBonus := 0.0
		if item.semanticRank > 0 {
			rankBonus += 1.0 / float64(60+item.semanticRank)
		}
		if item.keywordRank > 0 {
			rankBonus += 1.0 / float64(60+item.keywordRank)
		}
		item.score = 0.65*maxFloat(0, item.vectorScore) + 0.35*keywordScore + 6*rankBonus
		// Bonus for manually set importance
		if item.result.Importance > 0 {
			item.score += float64(item.result.Importance) * 0.05
		}
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
	defer rows.Close()

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
		defer semanticRows.Close()
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
		defer keywordRows.Close()
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
		keywordScore := 0.0
		if item.ftScore > 0 {
			keywordScore = item.ftScore / (item.ftScore + 10)
		}
		rankBonus := 0.0
		if item.semanticRank > 0 {
			rankBonus += 1.0 / float64(60+item.semanticRank)
		}
		if item.keywordRank > 0 {
			rankBonus += 1.0 / float64(60+item.keywordRank)
		}
		item.score = 0.65*maxFloat(0, item.vectorScore) + 0.35*keywordScore + 6*rankBonus
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
	var results []MediaResult
	if vec, ok := s.queryVectorLiteral(term); ok {
		results = s.searchMediaHybrid(term, vec, limit)
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
	return results
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
		defer semanticRows.Close()
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
		defer keywordRows.Close()
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
		keywordScore := 0.0
		if item.ftScore > 0 {
			keywordScore = item.ftScore / (item.ftScore + 10)
		}
		rankBonus := 0.0
		if item.semanticRank > 0 {
			rankBonus += 1.0 / float64(60+item.semanticRank)
		}
		if item.keywordRank > 0 {
			rankBonus += 1.0 / float64(60+item.keywordRank)
		}
		item.score = 0.70*maxFloat(0, item.vectorScore) + 0.30*keywordScore + 6*rankBonus
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

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
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

func (s *DatabaseStore) GetDBConn() *sql.DB {
	return s.db
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
	_, err := s.db.Exec(`
INSERT INTO relationship_state (profile_id, mood_score, affinity_score, last_sentiment, updated_at)
VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  mood_score=VALUES(mood_score), affinity_score=VALUES(affinity_score),
  last_sentiment=VALUES(last_sentiment), updated_at=VALUES(updated_at)`,
		s.profileID, state.MoodScore, state.AffinityScore, state.LastSentiment, time.Now().Unix())
	return err
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
	id := fmt.Sprintf("ent_%s", HashContent(fmt.Sprintf("%s_%s", category, name))[:16])
	now := time.Now().Unix()

	_, err := s.db.Exec(`
INSERT INTO knowledge_entities (id, name, category, created_at)
VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE name=VALUES(name)`, id, name, category, now)
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
	defer rows.Close()

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

var _ MemoryStore = (*DatabaseStore)(nil)
