package memory

import (
	"os"
	"path/filepath"
	"testing"
)

type countingEmbedder struct{ calls int }

func (e *countingEmbedder) Embed(text string) ([]float32, error) {
	e.calls++
	return []float32{0.1, 0.2, 0.3}, nil
}

func TestStoreVisibility(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := NewStore("test-vis", tmpDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	store.Add(Memory{
		ID:         "1",
		Content:    "owner only memory",
		Visibility: VisOwnerOnly,
	})
	store.Add(Memory{
		ID:         "2",
		Content:    "public to target memory",
		Visibility: VisPublicToTarget,
	})
	store.Add(Memory{
		ID:         "3",
		Content:    "private memory",
		Visibility: VisPrivate,
	})

	// owner sees owner_only and public_to_target, not private
	ownerResults := store.Search("memory", "owner")
	if len(ownerResults) != 2 {
		t.Errorf("owner sees %d results, want 2", len(ownerResults))
	}

	// target sees only public_to_target
	targetResults := store.Search("memory", "target")
	if len(targetResults) != 1 {
		t.Errorf("target sees %d results, want 1", len(targetResults))
	}
}

func TestStorePath(t *testing.T) {
	tmpDir := t.TempDir()
	profileID := "my-profile"
	store, err := NewStore(profileID, tmpDir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	store.Add(Memory{ID: "test1", Content: "hello"})
	store.Add(Memory{ID: "test2", Content: "world"})

	// Verify file path
	expectedPath := filepath.Join(tmpDir, profileID, "memories.json")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("memories.json not created at %s", expectedPath)
	}

	// Reload and verify
	store2, err := NewStore(profileID, tmpDir)
	if err != nil {
		t.Fatalf("NewStore reload: %v", err)
	}
	if len(store2.All()) != 2 {
		t.Errorf("reloaded %d memories, want 2", len(store2.All()))
	}
}

func TestHashContent(t *testing.T) {
	h1 := HashContent("hello")
	h2 := HashContent("hello")
	h3 := HashContent("world")

	if h1 != h2 {
		t.Errorf("same content should produce same hash")
	}
	if h1 == h3 {
		t.Errorf("different content should produce different hash")
	}
}

func TestQueryVectorLiteralCachesRepeatedQuery(t *testing.T) {
	embedder := &countingEmbedder{}
	store := &DatabaseStore{embedder: embedder, embeddingDimension: 3, queryVectorCache: make(map[string]cachedVectorLiteral)}
	first, ok := store.queryVectorLiteral("同一个问题")
	if !ok || first == "" {
		t.Fatal("first query should return a vector literal")
	}
	second, ok := store.queryVectorLiteral("同一个问题")
	if !ok || second != first {
		t.Fatal("cached query should return the same vector literal")
	}
	if embedder.calls != 1 {
		t.Fatalf("embedder calls = %d, want 1", embedder.calls)
	}
}

func TestWeightedRRFRewardsBothRetrievers(t *testing.T) {
	both := weightedRRF(1, 1, 0.65, 0.35)
	semanticOnly := weightedRRF(1, 0, 0.65, 0.35)
	keywordOnly := weightedRRF(0, 1, 0.65, 0.35)
	if both <= semanticOnly || both <= keywordOnly {
		t.Fatalf("both=%f semantic=%f keyword=%f", both, semanticOnly, keywordOnly)
	}
}
