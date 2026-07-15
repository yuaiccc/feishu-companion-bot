package media

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Asset struct {
	Hash         string
	RelativePath string
	AbsolutePath string
	MIMEType     string
	Size         int64
}

type Vault struct {
	Root string
}

func NewVault(root string) (*Vault, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("media vault root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	return &Vault{Root: abs}, nil
}

func (v *Vault) StoreBytes(data []byte, originalName string) (Asset, error) {
	if len(data) == 0 {
		return Asset{}, fmt.Errorf("media asset is empty")
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	mimeType := http.DetectContentType(data)
	ext := normalizedExtension(originalName, mimeType)
	relative := filepath.Join(hash[:2], hash[2:4], hash+ext)
	absolute := filepath.Join(v.Root, relative)
	if info, err := os.Stat(absolute); err == nil {
		return Asset{Hash: hash, RelativePath: relative, AbsolutePath: absolute, MIMEType: mimeType, Size: info.Size()}, nil
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return Asset{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(absolute), ".asset-*")
	if err != nil {
		return Asset{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return Asset{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Asset{}, err
	}
	if err := tmp.Close(); err != nil {
		return Asset{}, err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return Asset{}, err
	}
	if err := os.Rename(tmpPath, absolute); err != nil {
		return Asset{}, err
	}
	return Asset{Hash: hash, RelativePath: relative, AbsolutePath: absolute, MIMEType: mimeType, Size: int64(len(data))}, nil
}

func (v *Vault) IngestFile(path string) (Asset, error) {
	f, err := os.Open(path)
	if err != nil {
		return Asset{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return Asset{}, err
	}
	return v.StoreBytes(data, filepath.Base(path))
}

func (v *Vault) Resolve(relative string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(relative))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid media vault path: %q", relative)
	}
	abs := filepath.Join(v.Root, clean)
	if !strings.HasPrefix(abs, v.Root+string(filepath.Separator)) {
		return "", fmt.Errorf("media path escapes vault: %q", relative)
	}
	return abs, nil
}

func normalizedExtension(name, mimeType string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".heic", ".tif", ".tiff", ".bmp":
		return ext
	}
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/tiff":
		return ".tiff"
	default:
		return ".bin"
	}
}
