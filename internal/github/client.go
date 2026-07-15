package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Client struct {
	username string
	token    string
	httpCli  *http.Client
}

func NewClient(username, token string) *Client {
	return &Client{
		username: username,
		token:    token,
		httpCli:  &http.Client{Timeout: 30 * time.Second},
	}
}

type Event struct {
	ID        string                 `json:"id"`
	Type      string                 `json:"type"`
	Repo      map[string]interface{} `json:"repo"`
	Actor     map[string]interface{} `json:"actor"`
	Payload   map[string]interface{} `json:"payload"`
	CreatedAt string                 `json:"created_at"`
	Public    bool                   `json:"public"`
}

type Activity struct {
	Type      string
	Repo      string
	CreatedAt string
	Text      string
}

// FetchEvents fetches public events for the authenticated user.
func (c *Client) FetchEvents(ctx context.Context) ([]Event, error) {
	url := fmt.Sprintf("https://api.github.com/users/%s/events", c.username)
	return c.fetchEvents(ctx, url)
}

func (c *Client) fetchEvents(ctx context.Context, url string) ([]Event, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github api %d: %s", resp.StatusCode, string(data))
	}

	var events []Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}
	return events, nil
}

// FetchPrivateCommits fetches recent commits from a private repo.
func (c *Client) FetchPrivateCommits(ctx context.Context, repo string) ([]Event, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits", repo)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api %d", resp.StatusCode)
	}

	var commits []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&commits); err != nil {
		return nil, err
	}

	var events []Event
	for _, commit := range commits {
		sha, _ := commit["sha"].(string)
		commitMap, _ := commit["commit"].(map[string]interface{})
		commitDetail, _ := commitMap["message"].(string)
		htmlURL, _ := commit["html_url"].(string)
		// Use actual commit author date, fallback to now only if missing
		createdAt := time.Now().Format(time.RFC3339)
		if author, ok := commitMap["author"].(map[string]interface{}); ok {
			if dateStr, ok := author["date"].(string); ok {
				createdAt = dateStr
			}
		}

		events = append(events, Event{
			ID:   sha,
			Type: "PushEvent",
			Repo: map[string]interface{}{"name": repo},
			Payload: map[string]interface{}{
				"ref":  "refs/heads/main",
				"size": float64(1),
				"commits": []interface{}{
					map[string]interface{}{"sha": sha, "message": commitDetail, "url": htmlURL},
				},
			},
			CreatedAt: createdAt,
			Public:    true,
		})
	}
	return events, nil
}

// DedupEvents removes duplicate events by ID and PushEvent head_sha.
func DedupEvents(events []Event) []Event {
	seen := make(map[string]bool)
	pushSeen := make(map[string]bool)
	var out []Event

	for _, e := range events {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true

		if e.Type == "PushEvent" {
			if commits, ok := e.Payload["commits"].([]interface{}); ok && len(commits) > 0 {
				if c, ok := commits[0].(map[string]interface{}); ok {
					if sha, ok := c["sha"].(string); ok {
						if pushSeen[sha] {
							continue
						}
						pushSeen[sha] = true
					}
				}
			}
		}
		out = append(out, e)
	}
	return out
}

// ParseActivity converts an event to a human-readable activity text.
func ParseActivity(e Event) Activity {
	repo, _ := e.Repo["name"].(string)

	createdAt := e.CreatedAt
	if createdAt != "" {
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			createdAt = t.In(time.FixedZone("CST", 8*3600)).Format("01-02 15:04")
		}
	}

	text := ""
	switch e.Type {
	case "PushEvent":
		commits, _ := e.Payload["commits"].([]interface{})
		size, _ := e.Payload["size"].(float64)
		if len(commits) > 0 {
			if c, ok := commits[0].(map[string]interface{}); ok {
				msg, _ := c["message"].(string)
				msg = firstLine(msg)
				text = fmt.Sprintf("向 %s 推送了 %d 个提交：%s", repo, int(size), msg)
			}
		} else {
			text = fmt.Sprintf("向 %s 推送了代码", repo)
		}
	case "CreateEvent":
		refType, _ := e.Payload["ref_type"].(string)
		ref, _ := e.Payload["ref"].(string)
		text = fmt.Sprintf("在 %s 创建了 %s：%s", repo, refType, ref)
	case "DeleteEvent":
		refType, _ := e.Payload["ref_type"].(string)
		ref, _ := e.Payload["ref"].(string)
		text = fmt.Sprintf("在 %s 删除了 %s：%s", repo, refType, ref)
	case "IssuesEvent":
		action, _ := e.Payload["action"].(string)
		if issue, ok := e.Payload["issue"].(map[string]interface{}); ok {
			title, _ := issue["title"].(string)
			text = fmt.Sprintf("在 %s %s了 Issue：%s", repo, action, title)
		}
	case "PullRequestEvent":
		action, _ := e.Payload["action"].(string)
		if pr, ok := e.Payload["pull_request"].(map[string]interface{}); ok {
			title, _ := pr["title"].(string)
			text = fmt.Sprintf("在 %s %s了 PR：%s", repo, action, title)
		}
	case "WatchEvent":
		text = fmt.Sprintf("收藏了 %s", repo)
	case "ForkEvent":
		if forkee, ok := e.Payload["forkee"].(map[string]interface{}); ok {
			fullName, _ := forkee["full_name"].(string)
			text = fmt.Sprintf("Fork 了 %s 到 %s", repo, fullName)
		}
	case "ReleaseEvent":
		action, _ := e.Payload["action"].(string)
		if release, ok := e.Payload["release"].(map[string]interface{}); ok {
			tag, _ := release["tag_name"].(string)
			text = fmt.Sprintf("在 %s %s了 Release %s", repo, action, tag)
		}
	default:
		text = fmt.Sprintf("%s 在 %s", e.Type, repo)
	}

	return Activity{
		Type:      e.Type,
		Repo:      repo,
		CreatedAt: createdAt,
		Text:      text,
	}
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	// strip emoji prefix
	re := regexp.MustCompile(`^[\x{1F300}-\x{1F9FF}]+\s*`)
	s = re.ReplaceAllString(s, "")
	return s
}

func SortByCreatedAt(events []Event) {
	sort.Slice(events, func(i, j int) bool {
		return events[i].CreatedAt > events[j].CreatedAt
	})
}
