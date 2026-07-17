package sensenova

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultBaseURL = "https://token.sensenova.cn/v1"

// Client is the minimal SenseNova multimodal client used as a vision fallback.
// The key is the API key used by SenseNova's OpenAI-compatible endpoint.
type Client struct {
	token   string
	baseURL string
	model   string
	http    *http.Client
}

func NewClient(token, baseURL, model string, timeout time.Duration) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	return &Client{
		token:   strings.TrimSpace(token),
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   strings.TrimSpace(model),
		http:    &http.Client{Timeout: timeout},
	}
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

type response struct {
	Data struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	} `json:"data"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// DescribeImage asks SenseNova to describe an image in Chinese.
func (c *Client) DescribeImage(ctx context.Context, image []byte) (string, error) {
	if c == nil || c.token == "" {
		return "", fmt.Errorf("sensenova API key is not configured")
	}
	if c.model == "" {
		return "", fmt.Errorf("sensenova model is not configured")
	}
	if len(image) == 0 {
		return "", fmt.Errorf("empty image")
	}
	body := struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string        `json:"role"`
			Content []contentPart `json:"content"`
		} `json:"messages"`
		MaxNewTokens int     `json:"max_new_tokens"`
		Temperature  float64 `json:"temperature"`
		Stream       bool    `json:"stream"`
	}{
		Model: c.model,
		Messages: []struct {
			Role    string        `json:"role"`
			Content []contentPart `json:"content"`
		}{{
			Role: "user",
			Content: []contentPart{
				{Type: "image_url", ImageURL: &struct {
					URL string `json:"url"`
				}{URL: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(image)}},
				{Type: "text", Text: "请用中文简洁描述这张图片。如果图片里有文字，请尽量读出；如果是聊天截图，请说明人物和关键信息。不要编造看不见的细节。"},
			},
		}},
		MaxNewTokens: 220,
		Temperature:  0.1,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal sensenova request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("create sensenova request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("sensenova request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read sensenova response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("sensenova http %d: %s", resp.StatusCode, truncate(string(data), 800))
	}
	var parsed response
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("decode sensenova response: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("sensenova api %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	if len(parsed.Data.Choices) == 0 {
		return "", fmt.Errorf("sensenova response has no choices")
	}
	var message struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(parsed.Data.Choices[0].Message, &message); err == nil && strings.TrimSpace(message.Content) != "" {
		return strings.TrimSpace(message.Content), nil
	}
	var text string
	if err := json.Unmarshal(parsed.Data.Choices[0].Message, &text); err == nil && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text), nil
	}
	return "", fmt.Errorf("sensenova response has empty message")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
