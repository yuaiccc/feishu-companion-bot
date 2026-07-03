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
}

type DatabaseStore struct {
	db                 *sql.DB
	profileID          string
	includeChatArchive bool
	chatVisibility     Visibility
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
		db:                 db,
		profileID:          opts.ProfileID,
		includeChatArchive: opts.IncludeChatArchive,
		chatVisibility:     opts.ChatVisibility,
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
	rows, err := s.db.Query(`
SELECT CONCAT('[聊天记录 ', DATE_FORMAT(start_time, '%Y-%m-%d %H:%i'), ' - ', DATE_FORMAT(end_time, '%H:%i'), '] ', chunk_text)
FROM shuye_message_chunks
WHERE MATCH(chunk_text) AGAINST (? IN NATURAL LANGUAGE MODE) OR chunk_text LIKE ?
ORDER BY end_time DESC
LIMIT ?`, query, "%"+query+"%", limit)
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
