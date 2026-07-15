package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Embedder produces vector embeddings for text.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// HashEmbedder is a local fallback that produces deterministic
// "fake" embeddings from content hash. Privacy-safe since no external call.
type HashEmbedder struct{}

func (e *HashEmbedder) Embed(text string) ([]float32, error) {
	h := sha256.Sum256([]byte(text))
	vec := make([]float32, 256)
	for i := 0; i < 256; i++ {
		vec[i] = float32(h[i%32]) / 255.0
	}
	normalize(vec)
	return vec, nil
}

// OllamaEmbedder calls a local Ollama server for real embeddings.
type OllamaEmbedder struct {
	BaseURL string
	Model   string
	Timeout time.Duration
}

func NewOllamaEmbedder(baseURL, model string) *OllamaEmbedder {
	return &OllamaEmbedder{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Model:   model,
		Timeout: 30 * time.Second,
	}
}

func (e *OllamaEmbedder) Embed(text string) ([]float32, error) {
	if e.BaseURL == "" || e.Model == "" {
		return nil, fmt.Errorf("ollama: baseURL or model not configured")
	}

	body, _ := json.Marshal(map[string]string{
		"model":  e.Model,
		"prompt": text,
	})
	req, err := http.NewRequest("POST", e.BaseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: e.Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Embedding, nil
}

// CosineSim computes cosine similarity between two vectors.
func CosineSim(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(na))) * float32(math.Sqrt(float64(nb))))
}

func normalize(v []float32) {
	var norm float32
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		return
	}
	norm = float32(math.Sqrt(float64(norm)))
	for i := range v {
		v[i] /= norm
	}
}

// SearchOptions controls the retrieval phase.
type SearchOptions struct {
	TopK        int
	MinScore    float32
	FilterOwner string // "owner" or "target"
}

// SearchStore augments Store with embedding-based retrieval.
type SearchStore struct {
	*Store
	embedder Embedder
}

// NewSearchStore creates a search-capable memory store.
func NewSearchStore(profileID, dataDir string, embedder Embedder) (*SearchStore, error) {
	store, err := NewStore(profileID, dataDir)
	if err != nil {
		return nil, err
	}
	if embedder == nil {
		embedder = &HashEmbedder{}
	}
	return &SearchStore{Store: store, embedder: embedder}, nil
}

// SemanticSearch finds memories by embedding similarity.
func (s *SearchStore) SemanticSearch(query string, opts SearchOptions) []string {
	if opts.TopK <= 0 {
		opts.TopK = 5
	}
	queryVec, err := s.embedder.Embed(query)
	if err != nil || queryVec == nil {
		return KeywordSearch(s.Store, query, opts.TopK, opts.FilterOwner)
	}

	var scored []struct {
		text  string
		score float32
	}

	for _, m := range s.All() {
		// visibility filter
		if opts.FilterOwner == "owner" && m.Visibility == VisPrivate {
			continue
		}
		if opts.FilterOwner == "target" && (m.Visibility == VisOwnerOnly || m.Visibility == VisPrivate) {
			continue
		}

		memVec, err := s.embedder.Embed(m.Content)
		if err != nil || memVec == nil {
			continue
		}
		score := CosineSim(queryVec, memVec)
		if score >= opts.MinScore {
			scored = append(scored, struct {
				text  string
				score float32
			}{m.Content, score})
		}
	}

	// sort by score descending
	for i := 0; i < len(scored); i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[j].score > scored[i].score {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}

	var results []string
	for i := 0; i < min(opts.TopK, len(scored)); i++ {
		results = append(results, scored[i].text)
	}
	return results
}

// KeywordSearch does simple substring matching.
func KeywordSearch(store *Store, query string, topK int, filterOwner string) []string {
	if topK <= 0 {
		topK = 5
	}
	lower := strings.ToLower(query)
	var matched []Memory
	for _, m := range store.All() {
		if filterOwner == "owner" && m.Visibility == VisPrivate {
			continue
		}
		if filterOwner == "target" && (m.Visibility == VisOwnerOnly || m.Visibility == VisPrivate) {
			continue
		}
		if strings.Contains(strings.ToLower(m.Content), lower) {
			matched = append(matched, m)
		}
	}
	if len(matched) > topK {
		matched = matched[:topK]
	}
	var results []string
	for _, m := range matched {
		results = append(results, m.Content)
	}
	return results
}

// KeywordSearch does simple substring matching on the search store.
func (s *SearchStore) KeywordSearch(query string, opts SearchOptions) []string {
	return KeywordSearch(s.Store, query, opts.TopK, opts.FilterOwner)
}

// Search delegates to KeywordSearch for backward compatibility with Store.Search.
func (s *SearchStore) Search(query string, audience string) []string {
	return s.KeywordSearch(query, SearchOptions{TopK: 5, FilterOwner: audience})
}

func (s *SearchStore) SearchRelevant(query string, audience string) []RetrievedMemory {
	texts := s.Search(query, audience)
	results := make([]RetrievedMemory, 0, len(texts))
	for _, text := range texts {
		results = append(results, RetrievedMemory{
			Text:       text,
			MemoryType: InferMemoryType(text),
			SourceType: "local_memory",
			Importance: NormalizeImportance(0, InferMemoryType(text)),
			Confidence: NormalizeConfidence(0, InferMemoryType(text)),
		})
	}
	sortRetrievedMemories(results)
	return results
}

// Delete removes a memory by ID.
func (s *SearchStore) Delete(id string) error {
	return s.Store.Delete(id)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = json.Marshal // compile check
