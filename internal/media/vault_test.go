package media

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVaultStoresContentAddressedAssetsIdempotently(t *testing.T) {
	vault, err := NewVault(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("not-a-real-png")
	first, err := vault.StoreBytes(data, "sample.png")
	if err != nil {
		t.Fatal(err)
	}
	second, err := vault.StoreBytes(data, "renamed.png")
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash || first.AbsolutePath != second.AbsolutePath {
		t.Fatalf("same content should deduplicate: first=%+v second=%+v", first, second)
	}
	if _, err := os.Stat(first.AbsolutePath); err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(first.RelativePath) {
		t.Fatalf("relative path must stay portable: %s", first.RelativePath)
	}
}

func TestVaultRejectsEscapingPath(t *testing.T) {
	vault, err := NewVault(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Resolve("../secret"); err == nil {
		t.Fatal("expected path traversal rejection")
	}
}
