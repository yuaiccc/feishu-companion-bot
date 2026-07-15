package memoryquality

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"feishu-companion-bot/internal/llm"
	"feishu-companion-bot/internal/memory"
	"feishu-companion-bot/internal/safety"
)

type Decision struct {
	ID          string  `json:"id"`
	Action      string  `json:"action"`
	Replacement string  `json:"replacement,omitempty"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
}

type Report struct {
	Total     int        `json:"total"`
	Decisions []Decision `json:"decisions"`
}

type ApplyStats struct {
	Kept      int `json:"kept"`
	Deleted   int `json:"deleted"`
	Rewritten int `json:"rewritten"`
	Skipped   int `json:"skipped"`
}

type Auditor struct {
	client *llm.Client
}

func New(client *llm.Client) *Auditor { return &Auditor{client: client} }

func (a *Auditor) Audit(ctx context.Context, memories []memory.Memory) (Report, error) {
	if a.client == nil {
		return Report{}, fmt.Errorf("llm client is nil")
	}
	report := Report{Total: len(memories)}
	const batchSize = 24
	for start := 0; start < len(memories); start += batchSize {
		end := start + batchSize
		if end > len(memories) {
			end = len(memories)
		}
		items := make([]map[string]any, 0, end-start)
		for _, item := range memories[start:end] {
			items = append(items, map[string]any{
				"id": item.ID, "content": safety.SanitizeForLLM(item.Content),
				"type": item.MemoryType, "source": item.SourceType, "created_at": item.CreatedAt,
			})
		}
		payload, _ := json.Marshal(items)
		prompt := `你是长期记忆质量审计器。逐条判断，只返回 JSON 数组：
[{"id":"原ID","action":"keep|delete|rewrite","replacement":"仅 rewrite 时填写","confidence":0.0,"reason":"简短原因"}]

准则：
- keep：稳定事实、关系偏好、重要经历、可帮助未来对话的背景。
- delete：纯寒暄、机器人过程说明、无信息噪声、明显错误、已被同批更完整记忆覆盖的重复项。
- rewrite：事实有价值但表达含糊、混入无关过程或主体不清；不得发明新事实。
- 涉及私人关系的真实细节不是噪声。不同时间发生的相似经历不能误删成重复。
- 不确定时 keep；delete/rewrite 只有证据充分时 confidence 才能 >=0.9。

待审计记忆：` + string(payload)
		reply, err := a.client.Chat(ctx, []llm.Message{{Role: "system", Content: "你是谨慎、保守的记忆数据库审计器。"}, {Role: "user", Content: prompt}}, llm.WithTemperature(0), llm.WithMaxTokens(2200))
		if err != nil {
			return report, err
		}
		var decisions []Decision
		if err := json.Unmarshal([]byte(extractJSONArray(reply)), &decisions); err != nil {
			return report, fmt.Errorf("decode audit response: %w", err)
		}
		report.Decisions = append(report.Decisions, decisions...)
	}
	return report, nil
}

func Apply(store memory.MemoryStore, report Report, threshold float64) (ApplyStats, error) {
	byID := make(map[string]memory.Memory)
	for _, item := range store.All() {
		byID[item.ID] = item
	}
	var stats ApplyStats
	for _, decision := range report.Decisions {
		item, exists := byID[decision.ID]
		if !exists || decision.Confidence < threshold {
			stats.Skipped++
			continue
		}
		switch strings.ToLower(decision.Action) {
		case "keep":
			stats.Kept++
		case "delete":
			if err := store.Delete(decision.ID); err != nil {
				return stats, err
			}
			stats.Deleted++
		case "rewrite":
			replacement := strings.TrimSpace(decision.Replacement)
			if replacement == "" || strings.Contains(replacement, "[已隐藏") {
				stats.Skipped++
				continue
			}
			item.Content = replacement
			item.Hash = memory.HashContent(replacement)
			if err := store.Add(item); err != nil {
				return stats, err
			}
			stats.Rewritten++
		default:
			stats.Skipped++
		}
	}
	return stats, nil
}

func extractJSONArray(text string) string {
	text = strings.TrimSpace(text)
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start >= 0 && end >= start {
		return text[start : end+1]
	}
	return text
}
