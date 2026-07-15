package memory

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Visibility string

const (
	VisPrivate        Visibility = "private"
	VisOwnerOnly      Visibility = "owner_only"
	VisPublicToTarget Visibility = "public_to_target"
)

type MemoryType string

const (
	MemoryTypeWorking      MemoryType = "working"
	MemoryTypeEpisodic     MemoryType = "episodic"
	MemoryTypeSemantic     MemoryType = "semantic"
	MemoryTypeRelational   MemoryType = "relational"
	MemoryTypeArchiveChat  MemoryType = "archive_chat"
	MemoryTypeArchiveMedia MemoryType = "archive_media"
)

type Memory struct {
	ID         string     `json:"id"`
	Content    string     `json:"content"`
	Raw        string     `json:"raw"`
	Sender     string     `json:"sender"`
	Category   string     `json:"category"`
	MemoryType MemoryType `json:"memory_type"`
	Importance int        `json:"importance"`
	Confidence float64    `json:"confidence"`
	LastUsedAt int64      `json:"last_used_at"`
	ExpiresAt  int64      `json:"expires_at"`
	Visibility Visibility `json:"visibility"`
	SourceType string     `json:"source_type"`
	Hash       string     `json:"hash"`
	CreatedAt  int64      `json:"created_at"`
}

type RetrievedMemory struct {
	ID         string
	Text       string
	MemoryType MemoryType
	SourceType string
	Importance int
	Confidence float64
	LastUsedAt int64
	ExpiresAt  int64
	CreatedAt  int64
}

type RetrievalOptions struct {
	TopK                int
	IncludeBotMemory    bool
	IncludeChatArchive  bool
	IncludeMediaArchive bool
}

type PlannedRetriever interface {
	SearchRelevantWithOptions(query, audience string, opts RetrievalOptions) []RetrievedMemory
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

func (m RetrievedMemory) PromptText() string {
	prefix := MemoryTypeLabel(m.MemoryType)
	if prefix == "" {
		return m.Text
	}
	return fmt.Sprintf("[%s] %s", prefix, m.Text)
}

func MemoryTypeLabel(mt MemoryType) string {
	switch mt {
	case MemoryTypeWorking:
		return "工作记忆"
	case MemoryTypeEpisodic:
		return "情景记忆"
	case MemoryTypeSemantic:
		return "语义记忆"
	case MemoryTypeRelational:
		return "关系记忆"
	case MemoryTypeArchiveChat:
		return "聊天归档"
	case MemoryTypeArchiveMedia:
		return "图片归档"
	default:
		return ""
	}
}

func NormalizeMemoryType(mt MemoryType, content string) MemoryType {
	switch mt {
	case MemoryTypeWorking, MemoryTypeEpisodic, MemoryTypeSemantic, MemoryTypeRelational, MemoryTypeArchiveChat, MemoryTypeArchiveMedia:
		return mt
	}
	return InferMemoryType(content)
}

func NormalizeImportance(v int, mt MemoryType) int {
	if v > 0 {
		if v > 5 {
			return 5
		}
		return v
	}
	switch mt {
	case MemoryTypeRelational:
		return 5
	case MemoryTypeSemantic:
		return 4
	case MemoryTypeEpisodic:
		return 3
	case MemoryTypeWorking:
		return 2
	default:
		return 3
	}
}

func NormalizeConfidence(v float64, mt MemoryType) float64 {
	if v > 0 {
		if v > 1 {
			return 1
		}
		return v
	}
	switch mt {
	case MemoryTypeRelational, MemoryTypeSemantic:
		return 0.85
	case MemoryTypeEpisodic:
		return 0.75
	case MemoryTypeWorking:
		return 0.6
	default:
		return 0.7
	}
}

func NormalizeExpiresAt(v int64, mt MemoryType) int64 {
	if v > 0 {
		return v
	}
	switch mt {
	case MemoryTypeEpisodic:
		return time.Now().Add(365 * 24 * time.Hour).Unix()
	case MemoryTypeWorking:
		return time.Now().Add(72 * time.Hour).Unix()
	default:
		return 0
	}
}

func IsExpired(expiresAt int64) bool {
	return expiresAt > 0 && time.Now().Unix() >= expiresAt
}

func InferMemoryType(content string) MemoryType {
	lower := strings.ToLower(strings.TrimSpace(content))
	switch {
	case strings.Contains(lower, "叫她") || strings.Contains(lower, "称呼") || strings.Contains(lower, "安慰") || strings.Contains(lower, "心软") || strings.Contains(lower, "关系") || strings.Contains(lower, "老婆"):
		return MemoryTypeRelational
	case strings.Contains(lower, "喜欢") || strings.Contains(lower, "不加糖") || strings.Contains(lower, "平时") || strings.Contains(lower, "家住") || strings.Contains(lower, "本科") || strings.Contains(lower, "大学") || strings.Contains(lower, "学校"):
		return MemoryTypeSemantic
	case strings.Contains(lower, "今天") || strings.Contains(lower, "昨晚") || strings.Contains(lower, "这次") || strings.Contains(lower, "刚刚") || strings.Contains(lower, "一起") || strings.Contains(lower, "第一次见面"):
		return MemoryTypeEpisodic
	default:
		return MemoryTypeSemantic
	}
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
	for i := range s.Items {
		if s.Items[i].ID == m.ID {
			s.Items[i] = m
			return s.flush()
		}
	}
	s.Items = append(s.Items, m)
	return s.flush()
}

func (s *Store) Search(query string, audience string) []string {
	results := s.SearchRelevant(query, audience)
	out := make([]string, 0, len(results))
	for _, item := range results {
		out = append(out, item.Text)
	}
	return out
}

func (s *Store) SearchRelevant(query string, audience string) []RetrievedMemory {
	return s.SearchRelevantWithOptions(query, audience, RetrievalOptions{TopK: 8, IncludeBotMemory: true})
}

func (s *Store) SearchRelevantWithOptions(query string, audience string, opts RetrievalOptions) []RetrievedMemory {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !opts.IncludeBotMemory {
		return nil
	}
	if opts.TopK <= 0 {
		opts.TopK = 8
	}
	var results []RetrievedMemory
	for _, m := range s.Items {
		if IsExpired(m.ExpiresAt) {
			continue
		}
		if !s.visibleTo(m, audience) {
			continue
		}
		results = append(results, RetrievedMemory{
			ID:         m.ID,
			Text:       m.Content,
			MemoryType: NormalizeMemoryType(m.MemoryType, m.Content),
			SourceType: m.SourceType,
			Importance: NormalizeImportance(m.Importance, m.MemoryType),
			Confidence: NormalizeConfidence(m.Confidence, m.MemoryType),
			LastUsedAt: m.LastUsedAt,
			ExpiresAt:  NormalizeExpiresAt(m.ExpiresAt, m.MemoryType),
			CreatedAt:  m.CreatedAt,
		})
	}
	sortRetrievedMemories(results)
	if len(results) > opts.TopK {
		results = results[:opts.TopK]
	}
	s.touchRetrieved(results)
	return results
}

func (s *Store) touchRetrieved(results []RetrievedMemory) {
	now := time.Now().Unix()
	changed := false
	for _, result := range results {
		for i := range s.Items {
			if s.Items[i].ID == result.ID && result.ID != "" {
				s.Items[i].LastUsedAt = now
				changed = true
				break
			}
		}
	}
	if changed {
		_ = s.flush()
	}
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

// RelationshipState tracks emotional metrics and affinity scores for custom人设.
type RelationshipState struct {
	MoodScore     int    `json:"mood_score"`
	AffinityScore int    `json:"affinity_score"`
	LastSentiment string `json:"last_sentiment"`
}

// MemoryStore is the interface for memory storage operations.
type MemoryStore interface {
	All() []Memory
	Add(m Memory) error
	Delete(id string) error
	Search(query string, audience string) []string
	SearchRelevant(query string, audience string) []RetrievedMemory

	// 情绪与亲密度管理
	GetRelationshipState() (RelationshipState, error)
	UpdateRelationshipState(state RelationshipState) error

	// 图片 MD5 缓存管理
	GetImageHashCache(hash string) (ocr string, caption string, err error)
	SaveImageHashCache(hash string, ocr string, caption string) error

	// 知识图谱 (GraphRAG) 管理
	SaveEntity(name string, category string) (string, error)
	SaveRelation(srcName string, relation string, dstName string) error
	GetEntityRelations(entityNames []string) []string
	ResolveAliases(entityNames []string) []string
	GetRelationDestinations(srcName string, relation string) ([]string, error)
	DeleteRelation(srcName string, relation string, dstName string) error

	// 配置管理与情感历史
	GetConfig(key string, defaultVal string) string
	SetConfig(key string, value string) error
	GetEmotionHistory() ([]RelationshipState, []int64, error)
	GetProfileID() string
}

func (s *Store) GetRelationshipState() (RelationshipState, error) {
	return RelationshipState{MoodScore: 80, AffinityScore: 80, LastSentiment: "neutral"}, nil
}

func (s *Store) UpdateRelationshipState(state RelationshipState) error {
	return nil
}

func (s *Store) SaveEntity(name string, category string) (string, error) {
	return "", nil
}

func (s *Store) SaveRelation(srcName string, relation string, dstName string) error {
	return nil
}

func (s *Store) GetEntityRelations(entityNames []string) []string {
	return nil
}

func (s *Store) ResolveAliases(entityNames []string) []string {
	return entityNames
}

func (s *Store) GetRelationDestinations(srcName string, relation string) ([]string, error) {
	return nil, nil
}

func (s *Store) DeleteRelation(srcName string, relation string, dstName string) error {
	return nil
}

func (s *Store) GetConfig(key string, defaultVal string) string {
	return defaultVal
}

func (s *Store) SetConfig(key string, value string) error {
	return nil
}

func (s *Store) GetEmotionHistory() ([]RelationshipState, []int64, error) {
	return nil, nil, nil
}

func (s *Store) GetProfileID() string {
	return "dummy"
}

func (s *Store) GetImageHashCache(hash string) (ocr string, caption string, err error) {
	return "", "", fmt.Errorf("not supported in file store")
}

func (s *Store) SaveImageHashCache(hash string, ocr string, caption string) error {
	return nil
}

var _ MemoryStore = (*Store)(nil)
var _ MemoryStore = (*SearchStore)(nil)

func HashContent(content string) string {
	h := sha1.Sum([]byte(content))
	return fmt.Sprintf("%x", h[:])
}

func sortRetrievedMemories(results []RetrievedMemory) {
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].MemoryType != results[j].MemoryType {
			return memoryTypePriority(results[i].MemoryType) < memoryTypePriority(results[j].MemoryType)
		}
		if results[i].Importance != results[j].Importance {
			return results[i].Importance > results[j].Importance
		}
		if results[i].Confidence != results[j].Confidence {
			return results[i].Confidence > results[j].Confidence
		}
		if results[i].LastUsedAt != results[j].LastUsedAt {
			return results[i].LastUsedAt > results[j].LastUsedAt
		}
		return results[i].CreatedAt > results[j].CreatedAt
	})
}

func memoryTypePriority(mt MemoryType) int {
	switch mt {
	case MemoryTypeWorking:
		return 0
	case MemoryTypeRelational:
		return 1
	case MemoryTypeSemantic:
		return 2
	case MemoryTypeEpisodic:
		return 3
	case MemoryTypeArchiveChat:
		return 4
	case MemoryTypeArchiveMedia:
		return 5
	default:
		return 6
	}
}
