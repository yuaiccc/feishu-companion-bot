package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	appID     string
	appSecret string
	httpCli   *http.Client
	token     string
	tokenTime time.Time
}

func NewClient(appID, appSecret string) *Client {
	return &Client{
		appID:     appID,
		appSecret: appSecret,
		httpCli:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) refreshToken(ctx context.Context) error {
	if c.token != "" && time.Since(c.tokenTime) < 2*time.Hour {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": c.appID, "app_secret": c.appSecret})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://open.feishu.cn/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var result struct {
		Code              int    `json:"code"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if result.Code != 0 {
		return fmt.Errorf("token: code=%d", result.Code)
	}
	c.token = result.TenantAccessToken
	c.tokenTime = time.Now()
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}) (json.RawMessage, error) {
	if err := c.refreshToken(ctx); err != nil {
		return nil, err
	}
	var reqBody io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://open.feishu.cn/open-apis"+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var result struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("api %d: %s", result.Code, result.Msg)
	}
	return result.Data, nil
}

// Comment represents a document comment.
type Comment struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Position string `json:"position"`
}

// AddComment adds a comment to a Feishu document.
// The docToken and docRevision come from the document URL.
func (c *Client) AddComment(ctx context.Context, docToken, content, position string) (string, error) {
	data, err := c.do(ctx, "POST",
		fmt.Sprintf("/doc/v1/documents/%s/comments", docToken),
		map[string]interface{}{
			"content": map[string]string{"text": content},
			"position": map[string]string{"index_node_id": position},
		})
	if err != nil {
		return "", err
	}
	var result struct {
		Comment struct {
			ID string `json:"id"`
		} `json:"comment"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	return result.Comment.ID, nil
}

// GetComments retrieves comments on a document.
func (c *Client) GetComments(ctx context.Context, docToken string) ([]Comment, error) {
	data, err := c.do(ctx, "GET", fmt.Sprintf("/doc/v1/documents/%s/comments?page_size=50", docToken), nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []struct {
			ID      string `json:"id"`
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
			Position struct {
				IndexNodeID string `json:"index_node_id"`
			} `json:"position"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	var comments []Comment
	for _, item := range result.Items {
		comments = append(comments, Comment{
			ID:       item.ID,
			Content:  item.Content.Text,
			Position: item.Position.IndexNodeID,
		})
	}
	return comments, nil
}

// DeleteComment removes a comment from a document.
func (c *Client) DeleteComment(ctx context.Context, docToken, commentID string) error {
	_, err := c.do(ctx, "DELETE", fmt.Sprintf("/doc/v1/documents/%s/comments/%s", docToken, commentID), nil)
	return err
}
