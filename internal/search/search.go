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
	backend            Backend
	deerflowDir        string
	deerflowPython     string
	deerflowGatewayURL string
	openclawCLI        string
	httpCli            *http.Client
}

func NewClient(backend, deerflowDir, deerflowPython, deerflowGatewayURL, openclawCLI string) *Client {
	return &Client{
		backend:            Backend(backend),
		deerflowDir:        deerflowDir,
		deerflowPython:     deerflowPython,
		deerflowGatewayURL: deerflowGatewayURL,
		openclawCLI:        openclawCLI,
		httpCli:            &http.Client{Timeout: 60 * time.Second},
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
	if c.deerflowGatewayURL != "" {
		return c.searchDeerFlowHTTP(ctx, query)
	}

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
		_ = cmd.Process.Signal(syscall.SIGKILL)
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
		_ = cmd.Process.Signal(syscall.SIGKILL)
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
	_, _ = fmt.Fprintf(&b, "关于「%s」：\n\n", query)
	for _, r := range results {
		if r.Summary != "" {
			_, _ = fmt.Fprintf(&b, "• %s\n", r.Summary)
		} else {
			_, _ = fmt.Fprintf(&b, "• %s：%s\n", r.Title, r.URL)
		}
	}
	return b.String()
}

func (c *Client) searchDeerFlowHTTP(ctx context.Context, query string) ([]Result, error) {
	reqBody, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(c.deerflowGatewayURL, "/") + "/api/search"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request to deerflow gateway failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var errData bytes.Buffer
		_, _ = errData.ReadFrom(resp.Body)
		return nil, fmt.Errorf("deerflow gateway returned status %d: %s", resp.StatusCode, errData.String())
	}

	var results []Result
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("failed to parse deerflow gateway response: %w", err)
	}
	return results, nil
}
