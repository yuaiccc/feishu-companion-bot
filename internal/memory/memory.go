package memory

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Visibility string

const (
	VisPrivate        Visibility = "private"
	VisOwnerOnly      Visibility = "owner_only"
	VisPublicToTarget Visibility = "public_to_target"
)

type Memory struct {
	ID         string     `json:"id"`
	Content    string     `json:"content"`
	Raw        string     `json:"raw"`
	Sender     string     `json:"sender"`
	Category   string     `json:"category"`
	Visibility Visibility `json:"visibility"`
	SourceType string     `json:"source_type"`
	Hash       string     `json:"hash"`
	CreatedAt  int64      `json:"created_at"`
}

type MediaResult struct {
	MessageID string
	MsgID     string
	Sender    string
	SentAt    string
	FilePath  string
	OCRText   string
	Caption   string
	HasText   bool
}

func (m MediaResult) ContextText() string {
	parts := []string{fmt.Sprintf("[图片记录 %s %s]", m.SentAt, m.Sender)}
	if m.Caption != "" {
		parts = append(parts, "描述："+m.Caption)
	}
	if m.OCRText != "" {
		parts = append(parts, "文字："+m.OCRText)
	}
	return strings.Join(parts, " ")
}

type Store struct {
	mu    sync.RWMutex
	path  string
	Items []Memory `json:"memories"`
}

func NewStore(profileID, dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, profileID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "memories.json")

	s := &Store{path: path}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &s.Items)
	}
	return s, nil
}

func (s *Store) Add(m Memory) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Items = append(s.Items, m)
	return s.flush()
}

func (s *Store) Search(query string, audience string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []string
	for _, m := range s.Items {
		if !s.visibleTo(m, audience) {
			continue
		}
		results = append(results, m.Content)
	}
	return results
}

func (s *Store) visibleTo(m Memory, audience string) bool {
	switch m.Visibility {
	case VisPrivate:
		return false
	case VisOwnerOnly:
		return audience == "owner"
	case VisPublicToTarget:
		return audience == "target" || audience == "owner"
	default:
		return false
	}
}

func (s *Store) flush() error {
	data, err := json.MarshalIndent(s.Items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *Store) All() []Memory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Memory, len(s.Items))
	copy(out, s.Items)
	return out
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.Items {
		if m.ID == id {
			s.Items = append(s.Items[:i], s.Items[i+1:]...)
			return s.flush()
		}
	}
	return nil
}

// MemoryStore is the interface for memory storage operations.
type MemoryStore interface {
	All() []Memory
	Add(m Memory) error
	Delete(id string) error
	Search(query string, audience string) []string
}

var _ MemoryStore = (*Store)(nil)
var _ MemoryStore = (*SearchStore)(nil)

func HashContent(content string) string {
	h := sha1.Sum([]byte(content))
	return fmt.Sprintf("%x", h[:])
}
