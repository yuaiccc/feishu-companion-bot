package ocr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Result struct {
	Text         string        `json:"text"`
	Observations []Observation `json:"observations"`
	ElapsedMS    int           `json:"elapsed_ms"`
}

type Observation struct {
	Text       string  `json:"text"`
	Confidence float32 `json:"confidence"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Width      float64 `json:"width"`
	Height     float64 `json:"height"`
}

type AppleVision struct {
	Binary    string
	Languages []string
	Timeout   time.Duration
}

func NewAppleVision(binary string, timeout time.Duration) *AppleVision {
	if strings.TrimSpace(binary) == "" {
		binary = "bin/macos-vision-ocr"
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &AppleVision{Binary: binary, Languages: []string{"zh-Hans", "en-US"}, Timeout: timeout}
}

func (e *AppleVision) Available() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("apple vision OCR requires macOS")
	}
	info, err := os.Stat(e.Binary)
	if err != nil {
		return fmt.Errorf("apple vision OCR helper unavailable: %w", err)
	}
	if info.Mode()&0111 == 0 {
		return fmt.Errorf("apple vision OCR helper is not executable: %s", e.Binary)
	}
	return nil
}

func (e *AppleVision) RecognizeBytes(ctx context.Context, image []byte) (Result, error) {
	if len(image) == 0 {
		return Result{}, fmt.Errorf("empty image")
	}
	tmp, err := os.CreateTemp("", "feishu-companion-ocr-*.image")
	if err != nil {
		return Result{}, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(image); err != nil {
		tmp.Close()
		return Result{}, err
	}
	if err := tmp.Close(); err != nil {
		return Result{}, err
	}
	return e.RecognizeFile(ctx, path)
}

func (e *AppleVision) RecognizeFile(ctx context.Context, imagePath string) (Result, error) {
	if err := e.Available(); err != nil {
		return Result{}, err
	}
	absPath, err := filepath.Abs(imagePath)
	if err != nil {
		return Result{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	cmd := exec.CommandContext(callCtx, e.Binary, absPath, strings.Join(e.Languages, ","))
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return Result{}, fmt.Errorf("apple vision OCR failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return Result{}, fmt.Errorf("apple vision OCR failed: %w", err)
	}
	var result Result
	if err := json.Unmarshal(output, &result); err != nil {
		return Result{}, fmt.Errorf("decode apple vision OCR output: %w", err)
	}
	result.Text = strings.TrimSpace(result.Text)
	return result, nil
}
