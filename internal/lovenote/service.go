package lovenote

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"feishu-companion-bot/internal/config"
	"feishu-companion-bot/internal/llm"
	"feishu-companion-bot/internal/notes"
	"feishu-companion-bot/internal/profile"
	"feishu-companion-bot/internal/safety"
	"feishu-companion-bot/internal/state"
)

type Service struct {
	cfg    *config.Config
	client *notes.Client
	llm    *llm.Client
	state  *state.State
	prof   *profile.Profile
}

func New(cfg *config.Config, llmClient *llm.Client, st *state.State, prof *profile.Profile) *Service {
	return &Service{cfg: cfg, client: notes.NewClient(cfg.FeishuAppID, cfg.FeishuAppSecret), llm: llmClient, state: st, prof: prof}
}
func (s *Service) Run(ctx context.Context) error {
	if s.state == nil {
		return fmt.Errorf("恋爱笔记状态未初始化")
	}
	doc := strings.TrimSpace(s.cfg.LoveNoteDocToken)
	var err error
	if doc == "" && s.cfg.LoveNoteWikiToken != "" {
		doc, err = s.client.ResolveWikiDocToken(ctx, s.cfg.LoveNoteWikiToken)
		if err != nil {
			return err
		}
	}
	if doc == "" {
		return fmt.Errorf("未配置 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")
	}
	blocks, err := s.client.GetBlocks(ctx, doc)
	if err != nil {
		return err
	}
	date := time.Now().Format("2006-01-02")
	seen, sent := s.state.LoveNoteSnapshot(date)
	if len(seen) == 0 {
		for _, b := range blocks {
			seen[b.ID] = true
		}
		return s.state.SaveLoveNote(seen, date, 0)
	}
	fresh := make([]notes.Block, 0)
	for _, b := range blocks {
		if !seen[b.ID] {
			fresh = append(fresh, b)
			seen[b.ID] = true
		}
	}
	if len(fresh) == 0 {
		return s.state.SaveLoveNote(seen, date, 0)
	}
	max := s.cfg.LoveNoteMaxDailyComments
	if max <= 0 {
		max = 2
	}
	remaining := max - sent
	if remaining <= 0 || s.llm == nil {
		return s.state.SaveLoveNote(seen, date, 0)
	}
	candidates := make([]map[string]string, 0, len(fresh))
	for _, b := range fresh {
		text := safety.SanitizeForLLM(b.Text)
		if len(text) > 220 {
			text = text[:220]
		}
		candidates = append(candidates, map[string]string{"block_id": b.ID, "text": text})
	}
	ownerName, targetName, botRole := "文档作者", "伴侣", "陪伴机器人"
	if s.prof != nil {
		if strings.TrimSpace(s.prof.OwnerName) != "" {
			ownerName = s.prof.OwnerName
		}
		if strings.TrimSpace(s.prof.TargetDisplay()) != "" {
			targetName = s.prof.TargetDisplay()
		}
		if strings.TrimSpace(s.prof.BotRole) != "" {
			botRole = s.prof.BotRole
		}
	}
	prompt := fmt.Sprintf("你是%s，读%s和%s的恋爱笔记新增段落，只挑值得自然回应的甜蜜或有意义片段评论。最多输出%d条，可以少于上限；普通功能说明不要评论。每条35到90字，像自然旁观感想，不要总结、冒充任何一方或编造。只输出JSON数组：[{\"block_id\":\"...\",\"comment\":\"...\"}]。新增段落：%s", botRole, ownerName, targetName, remaining, mustJSON(candidates))
	raw, err := s.llm.Chat(ctx, []llm.Message{{Role: "system", Content: "只输出合法 JSON 数组。"}, {Role: "user", Content: prompt}}, llm.WithTemperature(0.75), llm.WithMaxTokens(700))
	if err != nil {
		return s.state.SaveLoveNote(seen, date, 0)
	}
	var choices []struct {
		BlockID string `json:"block_id"`
		Comment string `json:"comment"`
	}
	if err = json.Unmarshal([]byte(extractJSONArray(raw)), &choices); err != nil {
		return s.state.SaveLoveNote(seen, date, 0)
	}
	valid := map[string]bool{}
	for _, b := range fresh {
		valid[b.ID] = true
	}
	created := 0
	for _, c := range choices {
		comment := strings.TrimSpace(safety.SanitizeForLLM(c.Comment))
		if !valid[c.BlockID] || len([]rune(comment)) < 8 || len([]rune(comment)) > 140 {
			continue
		}
		if _, err = s.client.CreateDocxComment(ctx, doc, c.BlockID, comment); err != nil {
			log.Printf("[恋爱笔记] 评论失败 block=%s: %v", c.BlockID, err)
			continue
		}
		created++
		valid[c.BlockID] = false
		if created >= remaining {
			break
		}
	}
	log.Printf("[恋爱笔记] 新增段落=%d，新增评论=%d", len(fresh), created)
	return s.state.SaveLoveNote(seen, date, created)
}
func mustJSON(v interface{}) string { b, _ := json.Marshal(v); return string(b) }
func extractJSONArray(raw string) string {
	start, end := strings.Index(raw, "["), strings.LastIndex(raw, "]")
	if start >= 0 && end >= start {
		return raw[start : end+1]
	}
	return raw
}
