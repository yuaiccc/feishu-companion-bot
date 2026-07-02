package memory

import (
	"os"
	"path/filepath"
	"testing"
)

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
