package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type Backend string

const (
	BackendDeerFlow Backend = "deerflow"
	BackendOpenClaw Backend = "openclaw"
)

type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Summary string `json:"summary"`
}

type Client struct {
	backend       Backend
	deerflowDir   string
	deerflowPython string
	openclawCLI   string
	httpCli       *http.Client
}

func NewClient(backend, deerflowDir, deerflowPython, openclawCLI string) *Client {
	return &Client{
		backend:        Backend(backend),
		deerflowDir:    deerflowDir,
		deerflowPython: deerflowPython,
		openclawCLI:    openclawCLI,
		httpCli:        &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	switch c.backend {
	case BackendDeerFlow:
		return c.searchDeerFlow(ctx, query)
	case BackendOpenClaw:
		return c.searchOpenClaw(ctx, query)
	default:
		return nil, fmt.Errorf("unknown search backend: %s", c.backend)
	}
}

func (c *Client) searchDeerFlow(ctx context.Context, query string) ([]Result, error) {
	// Call local DeerFlow Python client
	python := c.deerflowPython
	if python == "" {
		python = "python"
	}
	script := c.deerflowDir + "/client.py"
	if script == "/client.py" {
		return nil, fmt.Errorf("DEERFLOW_BACKEND_DIR not set")
	}

	cmd := exec.CommandContext(ctx, python, script, "--query", query)
	cmd.Dir = c.deerflowDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case <-ctx.Done():
		cmd.Process.Signal(syscall.SIGKILL)
		return nil, ctx.Err()
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("deerflow: %v, stderr: %s", err, stderr.String())
		}
	}

	var results []Result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		return nil, fmt.Errorf("parse deerflow output: %w", err)
	}
	return results, nil
}

func (c *Client) searchOpenClaw(ctx context.Context, query string) ([]Result, error) {
	// Call openclaw infer web search
	cmd := exec.CommandContext(ctx, c.openclawCLI, "infer", "web", "search", "--query", query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case <-ctx.Done():
		cmd.Process.Signal(syscall.SIGKILL)
		return nil, ctx.Err()
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("openclaw: %v, stderr: %s", err, stderr.String())
		}
	}

	// Parse openclaw output — it returns lines of JSON
	var results []Result
	lines := strings.Split(stdout.String(), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(line), &r); err == nil {
			results = append(results, r)
		}
	}
	return results, nil
}

// Summarize converts raw search results into a natural language summary.
func Summarize(query string, results []Result) string {
	if len(results) == 0 {
		return fmt.Sprintf("关于「%s」，没有找到相关信息。", query)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("关于「%s」：\n\n", query))
	for _, r := range results {
		if r.Summary != "" {
			b.WriteString(fmt.Sprintf("• %s\n", r.Summary))
		} else {
			b.WriteString(fmt.Sprintf("• %s：%s\n", r.Title, r.URL))
		}
	}
	return b.String()
}
