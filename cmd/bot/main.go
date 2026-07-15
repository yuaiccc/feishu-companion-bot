package main

import (
	"bytes"
	"crypto/md5"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	stdctx "context"

	"feishu-companion-bot/internal/config"
	ctxmgr "feishu-companion-bot/internal/context"
	"feishu-companion-bot/internal/feishu"
	"feishu-companion-bot/internal/github"
	"feishu-companion-bot/internal/health"
	"feishu-companion-bot/internal/latency"
	"feishu-companion-bot/internal/llm"
	"feishu-companion-bot/internal/localapps"
	"feishu-companion-bot/internal/lovenote"
	"feishu-companion-bot/internal/memory"
	localocr "feishu-companion-bot/internal/ocr"
	"feishu-companion-bot/internal/profile"
	"feishu-companion-bot/internal/safety"
	"feishu-companion-bot/internal/search"
	"feishu-companion-bot/internal/state"
)

//go:embed web/dist/*
var webAssets embed.FS

var globalMemStore memory.MemoryStore

// ---- Card builders (Feishu Card 2.0) ----

// buildCard creates a proper Feishu Card 2.0 structure with body.elements.
func buildCard(headerTitle, headerTemplate string, elements []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"msg_type": "interactive",
		"card": map[string]interface{}{
			"schema": "2.0",
			"header": map[string]interface{}{
				"title":    map[string]string{"tag": "plain_text", "content": headerTitle},
				"template": headerTemplate,
			},
			"body": map[string]interface{}{
				"elements": elements,
			},
		},
	}
}

// ---- Intent ----

type Intent string

const (
	IntentNone        Intent = "none"
	IntentGitHub      Intent = "github"
	IntentHealth      Intent = "health"
	IntentMemoryAudit Intent = "memory_audit"
	IntentSearch      Intent = "search"
	IntentStatus      Intent = "status"
	IntentMedia       Intent = "media"
	IntentRecall      Intent = "recall"
)

type mediaMemoryStore interface {
	SearchMedia(query string, audience string, limit int) []memory.MediaResult
}

type managedMediaStore interface {
	SaveManagedMedia(ctx stdctx.Context, input memory.ManagedMediaInput) (memory.ManagedMediaAsset, error)
}

type replyPlan struct {
	ShouldReply      bool
	UseMemory        bool
	UseMedia         bool
	MemoryQuery      string
	MemoryTopK       int
	IncludeChat      bool
	IncludeMedia     bool
	RecentLimit      int
	ContextMaxChars  int
	UseGraph         bool
	TemporalAlign    bool
	DetectedEmotion  string
	ReplyStyle       string
	MaxTokens        int
	Reason           string
	DetectedEntities []string
}

type conversationState struct {
	ChatID           string
	RecentTopic      string
	LastImageQuery   string
	LastEmotion      string
	LastActiveAt     time.Time
	LastOwnerAt      time.Time
	LastTargetAt     time.Time
	LastBotReplyAt   time.Time
	MessagesSinceBot int
}

type stateManager struct {
	mu    sync.Mutex
	items map[string]*conversationState
}

func newStateManager() *stateManager {
	return &stateManager{items: make(map[string]*conversationState)}
}

func (m *stateManager) UpdateMessage(msg feishu.Message, audience string) *conversationState {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	st := m.items[msg.ChatID]
	if st == nil {
		st = &conversationState{ChatID: msg.ChatID}
		m.items[msg.ChatID] = st
	}
	st.LastActiveAt = now
	st.MessagesSinceBot++
	switch audience {
	case "owner":
		st.LastOwnerAt = now
	case "target":
		st.LastTargetAt = now
	}
	if topic := inferTopic(msg.Content); topic != "" {
		st.RecentTopic = topic
	}
	return st.Clone()
}

func (m *stateManager) SetEmotion(chatID, emotion string) {
	emotion = strings.ToLower(strings.TrimSpace(emotion))
	switch emotion {
	case "neutral", "happy", "sad", "angry", "tired", "anxious":
	default:
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.items[chatID]
	if st == nil {
		st = &conversationState{ChatID: chatID}
		m.items[chatID] = st
	}
	st.LastEmotion = emotion
}

func (m *stateManager) MarkBotReply(chatID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.items[chatID]
	if st == nil {
		st = &conversationState{ChatID: chatID}
		m.items[chatID] = st
	}
	st.MessagesSinceBot = 0
	st.LastActiveAt = time.Now()
	st.LastBotReplyAt = st.LastActiveAt
}

func (m *stateManager) MarkMediaSearch(chatID, query string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.items[chatID]
	if st == nil {
		st = &conversationState{ChatID: chatID}
		m.items[chatID] = st
	}
	st.LastImageQuery = query
	st.RecentTopic = "图片回忆：" + query
	st.LastActiveAt = time.Now()
}

func (m *stateManager) Get(chatID string) conversationState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.items[chatID]; st != nil {
		return *st.Clone()
	}
	return conversationState{ChatID: chatID}
}

func (s *conversationState) Clone() *conversationState {
	if s == nil {
		return &conversationState{}
	}
	copied := *s
	return &copied
}

func (s conversationState) PromptText() string {
	var parts []string
	if s.RecentTopic != "" {
		parts = append(parts, "最近话题："+s.RecentTopic)
	}
	if s.LastImageQuery != "" {
		parts = append(parts, "上次图片搜索："+s.LastImageQuery)
	}
	if s.LastEmotion != "" {
		parts = append(parts, "最近情绪："+s.LastEmotion)
	}
	if s.MessagesSinceBot > 0 {
		parts = append(parts, fmt.Sprintf("机器人上次回复后已有 %d 条新消息", s.MessagesSinceBot))
	}
	if !s.LastBotReplyAt.IsZero() {
		parts = append(parts, fmt.Sprintf("机器人上次回复距今约 %d 分钟", int(time.Since(s.LastBotReplyAt).Minutes())))
	}
	return strings.Join(parts, "\n")
}

func classifyIntent(ctx stdctx.Context, llmClient *llm.Client, content string, isRecentMediaSearch bool) Intent {
	if llmClient == nil {
		return IntentNone
	}

	prompt := fmt.Sprintf(`你是飞书陪伴机器人的消息意图分类器。只返回 JSON：
{"intent":"github|health|memory_audit|search|status|recall|media|none","reason":"极短原因"}

分类规则：
- github：查询主人在 GitHub 的 commit 提交、代码变动、近期活动等。
- health：自检命令，包含健康检查、服务状态、自检、自测状态等。
- memory_audit：审计记忆、看我的记忆、记忆审计面板、管理记忆等。
- search：要求去上网搜索、查百度、看网页、联网查询最新资讯、搜索当前新闻等。
- status：询问主人现在在做什么、是否在电脑前、当前电脑活动等。
- recall：要求机器人撤回消息（如‘撤回刚才发错的消息’、‘把刚才那条收回’、‘撤回’、‘发错了收回’）。
- media：看照片、看截图、回忆图片、发张图、有没有照片、换一张、看别的图片等。
- none：其他普通的闲聊、询问、指令等。

如果 context 表明刚刚发生过图片查询，且用户当前输入如“换一张”、“再来一张”、“下一张”，则必须判定为 "media"。

刚刚是否发生过图片查询：%v
用户消息：%s`, isRecentMediaSearch, safety.SanitizeForLLM(content))

	reply, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(100))
	if err != nil {
		log.Printf("[意图分类] LLM 调用失败，按普通对话处理: %v", err)
		return IntentNone
	}

	var parsed struct {
		Intent string `json:"intent"`
	}
	if err := json.Unmarshal([]byte(extractJSON(reply)), &parsed); err != nil {
		log.Printf("[意图分类] JSON 解析失败: %v raw=%q", err, reply)
		return IntentNone
	}

	switch parsed.Intent {
	case "github":
		return IntentGitHub
	case "health":
		return IntentHealth
	case "memory_audit":
		return IntentMemoryAudit
	case "search":
		return IntentSearch
	case "status":
		return IntentStatus
	case "recall":
		return IntentRecall
	case "media":
		return IntentMedia
	default:
		return IntentNone
	}
}

func inferTopic(content string) string {
	text := strings.TrimSpace(content)
	if text == "" || text == "只叫了你一声" {
		return ""
	}
	if len([]rune(text)) > 36 {
		runes := []rune(text)
		text = string(runes[:36]) + "..."
	}
	return text
}

func planReply(ctx stdctx.Context, llmClient *llm.Client, msg feishu.Message, intent Intent, st conversationState, prof *profile.Profile, senderLabel string, mem memory.MemoryStore) replyPlan {
	plan := defaultReplyPlan(msg, intent, st)
	passiveGroup := msg.ChatType == "group" && !msg.IsMentioned
	if passiveGroup && llmClient == nil {
		plan.ShouldReply = false
		plan.Reason = "passive_no_llm"
		return plan
	}
	if llmClient == nil || (intent != IntentNone && !passiveGroup) {
		return plan
	}
	stateText := st.PromptText()
	if stateText == "" {
		stateText = "暂无"
	}
	prompt := fmt.Sprintf(`你是飞书陪伴机器人的上下文 Planner。只返回 JSON：
{"should_reply":true/false,"use_memory":true/false,"use_media":true/false,"memory_query":"适合检索的独立问题","memory_top_k":6,"include_chat_archive":true/false,"include_media_archive":true/false,"recent_message_limit":20,"context_max_chars":12000,"use_graph":true/false,"temporal_align":true/false,"detected_emotion":"neutral|happy|sad|angry|tired|anxious","reply_style":"short|normal|comfort|detailed","max_tokens":600,"detected_entities":["标准实体名1","标准实体名2"],"reason":"极短原因"}

判断规则：
- 群聊中被 @ 时通常要回复；私聊通常要回复。
- 群聊未 @ 时必须由你理解语义后决定是否插话，不能按关键词机械匹配；只有你确实能帮上忙、接住情绪、回答问题、补充图片/记忆时才插话。
- 群聊未 @ 且机器人刚回复过时，要更克制，除非用户明显在接着问你或需要帮助。
- 当前发言人是 %s。不要把当前发言人和被提到的人混淆；不要把当前发言人叫成另一个成员。
- 回复长度服从当前问题：寒暄和简单确认用 short，日常聊天用 normal，需要安慰用 comfort，只有解释复杂问题时才用 detailed。
- 如果用户表达低落/生气/累，comfort，并查记忆。
- 只有当前问题确实依赖偏好、习惯、承诺、过去经历或关系背景时才开启记忆检索；普通寒暄不要查长期记忆。
- 如果问题涉及图片、截图，再 use_media。
- memory_query 要补全代词和省略信息，成为不依赖当前消息也能理解的检索问题；不查记忆时留空。
- 仅在确有需要时打开聊天归档、图片归档、图谱和时间对齐。普通寒暄只读近期消息；回忆共同经历可查聊天归档；找图才查图片归档。
- memory_top_k 通常 4-8；recent_message_limit 通常 8-30；context_max_chars 通常 6000-16000。
- **核心实体识别 (detected_entities)**：分析最新用户消息和上下文，提取其中所指的人名、外号、别名、宠物名和专属代名词（例如：结合成员资料纠正昵称错别字；把“她”、“我妈”等代词结合前文还原为标准实体名）。如果有提取出来的实体，放进 detected_entities 列表中；如果没有提取出任何特定实体，返回空数组 []。
- max_tokens 通常推荐在 500 到 600 之间，以保证回答内容丰满有深度。

会话状态：
%s

用户消息：%s`, senderLabel, stateText, safety.SanitizeForLLM(msg.Content))
	reply, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: buildSystemPrompt(prof, mem)},
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(256))
	if err != nil {
		log.Printf("[回复决策] LLM 失败，使用默认计划: %v", err)
		if passiveGroup {
			plan.ShouldReply = false
			plan.Reason = "passive_llm_error"
		}
		return plan
	}
	var parsed struct {
		ShouldReply      bool     `json:"should_reply"`
		UseMemory        bool     `json:"use_memory"`
		UseMedia         bool     `json:"use_media"`
		MemoryQuery      string   `json:"memory_query"`
		MemoryTopK       int      `json:"memory_top_k"`
		IncludeChat      bool     `json:"include_chat_archive"`
		IncludeMedia     bool     `json:"include_media_archive"`
		RecentLimit      int      `json:"recent_message_limit"`
		ContextMaxChars  int      `json:"context_max_chars"`
		UseGraph         bool     `json:"use_graph"`
		TemporalAlign    bool     `json:"temporal_align"`
		DetectedEmotion  string   `json:"detected_emotion"`
		ReplyStyle       string   `json:"reply_style"`
		MaxTokens        int      `json:"max_tokens"`
		Reason           string   `json:"reason"`
		DetectedEntities []string `json:"detected_entities"`
	}
	if err := json.Unmarshal([]byte(extractJSON(reply)), &parsed); err != nil {
		log.Printf("[回复决策] JSON 解析失败，使用默认计划: %v raw=%q", err, reply)
		if passiveGroup {
			plan.ShouldReply = false
			plan.Reason = "passive_plan_parse_error"
		}
		return plan
	}
	if parsed.MaxTokens <= 0 {
		parsed.MaxTokens = plan.MaxTokens
	}
	if parsed.MaxTokens > 700 {
		parsed.MaxTokens = 700
	}
	if parsed.ReplyStyle == "" {
		parsed.ReplyStyle = plan.ReplyStyle
	}
	parsed.MemoryTopK = clamp(parsed.MemoryTopK, 1, 12, plan.MemoryTopK)
	parsed.RecentLimit = clamp(parsed.RecentLimit, 6, 40, plan.RecentLimit)
	parsed.ContextMaxChars = clamp(parsed.ContextMaxChars, 4000, 18000, plan.ContextMaxChars)
	return replyPlan{
		ShouldReply:      parsed.ShouldReply,
		UseMemory:        parsed.UseMemory,
		UseMedia:         parsed.UseMedia,
		MemoryQuery:      strings.TrimSpace(parsed.MemoryQuery),
		MemoryTopK:       parsed.MemoryTopK,
		IncludeChat:      parsed.IncludeChat,
		IncludeMedia:     parsed.IncludeMedia,
		RecentLimit:      parsed.RecentLimit,
		ContextMaxChars:  parsed.ContextMaxChars,
		UseGraph:         parsed.UseGraph,
		TemporalAlign:    parsed.TemporalAlign,
		DetectedEmotion:  parsed.DetectedEmotion,
		ReplyStyle:       parsed.ReplyStyle,
		MaxTokens:        parsed.MaxTokens,
		Reason:           parsed.Reason,
		DetectedEntities: parsed.DetectedEntities,
	}
}

func clamp(value, low, high, fallback int) int {
	if value == 0 {
		value = fallback
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func defaultReplyPlan(msg feishu.Message, intent Intent, st conversationState) replyPlan {
	plan := replyPlan{ShouldReply: true, UseMemory: true, UseMedia: false, MemoryTopK: 6, RecentLimit: 20, ContextMaxChars: 12000, UseGraph: true, TemporalAlign: true, ReplyStyle: "normal", MaxTokens: 500, Reason: "default"}
	if msg.ChatType == "group" && !msg.IsMentioned {
		plan.ShouldReply = false
		plan.Reason = "passive_wait_llm"
		return plan
	}
	if intent != IntentNone {
		plan.UseMemory = intent == IntentMedia || intent == IntentStatus || intent == IntentGitHub || intent == IntentSearch
		plan.UseMedia = intent == IntentMedia
		plan.MaxTokens = 500
		plan.Reason = "intent"
		return plan
	}
	if len([]rune(msg.Content)) <= 3 {
		plan.UseMemory = false
		plan.ReplyStyle = "short"
		plan.MaxTokens = 160
	}
	if st.LastEmotion == "sad" || st.LastEmotion == "angry" || st.LastEmotion == "anxious" {
		plan.UseMemory = true
		plan.ReplyStyle = "comfort"
		plan.MaxTokens = 500
		plan.Reason = "emotion"
	}
	return plan
}

func replyStyleInstruction(plan replyPlan, st conversationState) string {
	var parts []string
	if stateText := st.PromptText(); stateText != "" {
		parts = append(parts, "会话状态：\n"+stateText)
	}
	switch plan.ReplyStyle {
	case "short":
		parts = append(parts, "回复要短，1-3 句即可。")
	case "comfort":
		parts = append(parts, "优先安慰和接住情绪，先表明态度，再轻轻回应事情本身。")
	case "detailed":
		parts = append(parts, "把问题说明清楚并给出必要依据，只使用与当前问题直接相关且对当前发言人可见的记忆；避免堆砌无关私人细节。")
	default:
		parts = append(parts, "回复自然，不要过长。")
	}
	if plan.Reason != "" {
		parts = append(parts, "回复前决策原因："+plan.Reason)
	}
	return strings.Join(parts, "\n")
}

// ---- Passive Assistant ----

type PassiveAssistant struct {
	lastGroupMsgTime time.Time
	lastTopicSent    time.Time
	cooldown         time.Duration
}

func NewPassiveAssistant() *PassiveAssistant {
	return &PassiveAssistant{
		lastGroupMsgTime: time.Now(),
		lastTopicSent:    time.Time{},
		cooldown:         30 * time.Minute,
	}
}

func (p *PassiveAssistant) OnMessage(msg feishu.Message) {
	if msg.ChatType == "group" {
		p.lastGroupMsgTime = time.Now()
	}
}

func (p *PassiveAssistant) ShouldSendProactiveTopic() bool {
	if time.Since(p.lastTopicSent) < 24*time.Hour {
		return false
	}
	if time.Since(p.lastGroupMsgTime) < p.cooldown {
		return false
	}
	return true
}

func (p *PassiveAssistant) MarkTopicSent() {
	p.lastTopicSent = time.Now()
}

// ---- Emoji picker ----

func pickEmoji(content string, fromOwner bool, prof *profile.Profile) string {
	lower := strings.ToLower(content)
	// Intimate emojis only apply when an intimate target is configured; generic
	// profiles (no target_name) fall through to the casual branch below.
	if !fromOwner && prof.TargetDisplay() != "" {
		if strings.ContainsAny(lower, "想你爱你喜欢亲抱宝贝么么mua") {
			return "KISS"
		}
		if strings.ContainsAny(lower, "哈哈开心好棒嘻嘻") {
			return "LAUGH"
		}
		if strings.ContainsAny(lower, "难过哭难受委屈") {
			return "COMFORT"
		}
		if strings.ContainsAny(lower, "谢谢感谢") {
			return "THANKS"
		}
		return "SMOOCH"
	}
	// owner / generic messages
	if strings.ContainsAny(lower, "哈哈搞笑笑死") {
		return "LOL"
	}
	if strings.ContainsAny(lower, "牛厉害棒nice") {
		return "PRAISE"
	}
	if strings.ContainsAny(lower, "谢谢感谢") {
		return "THANKS"
	}
	if strings.ContainsAny(lower, "累困想睡") {
		return "DULL"
	}
	if strings.ContainsAny(lower, "为什么怎么啥") {
		return "THINKING"
	}
	if strings.ContainsAny(lower, "好的行可以ok嗯") {
		return "DONE"
	}
	return "THUMBSUP"
}

// ---- Memory candidate decision ----

func alignTemporalMemory(ctx stdctx.Context, llmClient *llm.Client, memories []string) []string {
	if llmClient == nil || len(memories) < 2 {
		return memories
	}

	var sb strings.Builder
	for i, m := range memories {
		_, _ = fmt.Fprintf(&sb, "[%d] %s\n", i, m)
	}
	memList := sb.String()

	prompt := fmt.Sprintf(`你是一个历史记忆对齐与消解管家。下面是从数据库中检索出来的多条针对同一个用户的近期和历史记忆片段（包括以前的微信聊天记录、图片OCR以及机器人主动沉淀的事实记忆）。

请分析这几条记忆。如果较新的记忆（如飞书对话中提炼出的事实）明确否定、修正或冲突了较老旧的历史记录（如带有 [聊天记录 YYYY-MM-DD] 的过去微信对话），说明老旧的记录信息已经失效或发生了实质改变。

你必须仅保留当前最新生效的记忆事实，并在返回的 JSON 中列出应当被“保留”的记忆条目索引号。

召回的记忆列表：
%s

只返回 JSON，不要解释。格式：
{"valid_indices": [0, 2, ...]}`, memList)

	resp, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(128))
	if err != nil {
		log.Printf("[记忆对齐] 过滤失败: %v", err)
		return memories
	}

	m := extractJSON(resp)
	if m == "" {
		return memories
	}
	var result struct {
		ValidIndices []int `json:"valid_indices"`
	}
	if err := json.Unmarshal([]byte(m), &result); err != nil {
		log.Printf("[记忆对齐] 解析 JSON 失败: %v", err)
		return memories
	}

	if len(result.ValidIndices) == 0 {
		return memories
	}

	var aligned []string
	seen := make(map[int]bool)
	for _, idx := range result.ValidIndices {
		if idx >= 0 && idx < len(memories) {
			aligned = append(aligned, memories[idx])
			seen[idx] = true
		}
	}

	log.Printf("[记忆对齐] 原始记忆数: %d -> 对齐后留存数: %d", len(memories), len(aligned))
	return aligned
}

func updateEmotionMetrics(ctx stdctx.Context, llmClient *llm.Client, memStore memory.MemoryStore, userMsg string, replyMsg string) {
	if memStore == nil || llmClient == nil || userMsg == "" || replyMsg == "" {
		return
	}

	oldState, err := memStore.GetRelationshipState()
	if err != nil {
		oldState = memory.RelationshipState{MoodScore: 80, AffinityScore: 80, LastSentiment: "neutral"}
	}

	prompt := fmt.Sprintf(`你是一个陪伴机器人的情感关系分析管家。请根据用户最新发的消息和机器人的回复，评估并更新当前用户的“情绪指数”（Mood）和“人机亲密好感度”（Affinity）。

当前值：
- 大哥情绪分值（0到100）：%d
- 大哥亲密好感度（0到100）：%d
- 上次情感状态：%s

用户消息：%s
机器人回复：%s

更新规则：
- mood_score 评估用户的当下精神情绪。如果表现为疲惫、委屈、难过或生气，分值应适当降低（如50-70）；如果表现为开心、放松、开玩笑，分值应提升（如85-95）。
- affinity_score 评估用户与机器人小弟之间的亲近程度。如果用户夸奖小弟、认可小弟、进行温馨的私密分享，亲密度分值上升；如果吐槽、嫌弃、态度冰冷，亲密度分值可以微降。
- last_sentiment 必须为以下四种之一：happy (高兴/轻松), tired (疲惫/难过), angry (生气/烦躁), neutral (平静/普通)。

只返回 JSON，不要解释。格式：
{"mood_score": 整数, "affinity_score": 整数, "last_sentiment": "四种状态之一"}`, oldState.MoodScore, oldState.AffinityScore, oldState.LastSentiment, userMsg, replyMsg)

	resp, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(128))
	if err != nil {
		log.Printf("[情绪更新] LLM 分析失败: %v", err)
		return
	}

	m := extractJSON(resp)
	if m == "" {
		return
	}
	var result struct {
		MoodScore     int    `json:"mood_score"`
		AffinityScore int    `json:"affinity_score"`
		LastSentiment string `json:"last_sentiment"`
	}
	if err := json.Unmarshal([]byte(m), &result); err != nil {
		return
	}

	if result.MoodScore < 0 {
		result.MoodScore = 0
	}
	if result.MoodScore > 100 {
		result.MoodScore = 100
	}
	if result.AffinityScore < 0 {
		result.AffinityScore = 0
	}
	if result.AffinityScore > 100 {
		result.AffinityScore = 100
	}

	newState := memory.RelationshipState{
		MoodScore:     result.MoodScore,
		AffinityScore: result.AffinityScore,
		LastSentiment: result.LastSentiment,
	}
	err = memStore.UpdateRelationshipState(newState)
	if err != nil {
		log.Printf("[情绪更新] 写入 relationship_state 失败: %v", err)
	} else {
		log.Printf("[情绪更新] 成功! 情绪值: %d, 亲密值: %d, 状态: %s", newState.MoodScore, newState.AffinityScore, newState.LastSentiment)
	}
}

type workingMemoryTurn struct {
	Sender  string
	Content string
}

func shouldRememberViaLLM(ctx stdctx.Context, content string, fromOwner bool, prof *profile.Profile, llmClient *llm.Client, recentTurns []workingMemoryTurn) (bool, string, string) {
	limit := 500
	if strings.Contains(content, "图片理解：") {
		limit = 2000
	}
	if llmClient == nil || content == "" || len(content) < 3 || len(content) > limit {
		return false, "", ""
	}

	sender := prof.OwnerDisplay()
	if !fromOwner {
		if t := prof.TargetDisplay(); t != "" {
			sender = t
		} else {
			sender = "对方"
		}
	}

	var sb strings.Builder
	if len(recentTurns) > 0 {
		sb.WriteString("\n【近期对话历史（按时间由远及近）：】\n")
		start := 0
		if len(recentTurns) > 5 {
			start = len(recentTurns) - 5
		}
		for _, turn := range recentTurns[start:] {
			_, _ = fmt.Fprintf(&sb, "- %s: %s\n", turn.Sender, turn.Content)
		}
	}

	systemPrompt := fmt.Sprintf(`你是飞书陪伴机器人的记忆管家。判断当前的一条聊天消息是否值得进入长期记忆候选。
你可以结合提供的【近期对话历史】来理解用户的最新消息。如果用户的最新消息是确认、简答（如‘对的’、‘好的’、‘就定这个’）或带有代词，请顺着对话历史中的语境还原出具体的事实内容，提炼为一句主谓宾完整、含义清晰明确的长期中文记忆。

只返回 JSON，不要解释。格式：
{"remember": true/false, "memory": "一句自然中文记忆", "memory_type": "semantic|relational|episodic", "reason": "极短原因"}

判断标准：
- 记住稳定偏好、重要事实、长期习惯、关系边界、称呼方式、明确承诺、重要计划。
- 如果消息中包含“图片理解：视觉描述/OCR文字”，说明发送人发了一张照片。请结合视觉描述或文字提炼出生活相关的重要事实、动作、喜好或场景状态（例如：‘用户和朋友去吃了烤肉’，‘伴侣发了自拍，表示今天很开心’）。对于这类视觉相关的具体动作/事件记忆，memory_type 优先归类为 episodic；如果是图片反映出的长期稳定喜好，则归类为 semantic。
- memory_type 含义：semantic=稳定事实/偏好保存，relational=相处方式/边界/称呼/安慰方式，episodic=值得长期留痕的重要事件。
- 不记普通寒暄、即时情绪、重复废话、临时闲聊、表情语气、已经过时的细枝末节。
- 不要编造消息里没有的信息。
- memory 要适合给 owner 私聊确认，简短、克制、可长期复用。
%s`, sb.String())

	resp, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: fmt.Sprintf("发送者：%s\n消息：%s", sender, content)},
	}, llm.WithTemperature(0), llm.WithMaxTokens(256))
	if err != nil {
		log.Printf("[记忆判断] LLM 失败: %v", err)
		return false, "", ""
	}

	m := extractJSON(resp)
	if m == "" {
		return false, "", ""
	}
	var result struct {
		Remember   bool   `json:"remember"`
		Memory     string `json:"memory"`
		MemoryType string `json:"memory_type"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(m), &result); err != nil {
		log.Printf("[记忆判断] JSON 解析失败: %v", err)
		return false, "", ""
	}
	return result.Remember, result.Memory, result.MemoryType
}

func consolidateMemory(ctx stdctx.Context, llmClient *llm.Client, memStore memory.MemoryStore, content string, mType string, sender string, prof *profile.Profile) (string, bool) {
	if memStore == nil || llmClient == nil {
		return content, true
	}

	existing := memStore.SearchRelevant(content, "owner")
	if len(existing) == 0 {
		return content, true
	}

	var sb strings.Builder
	for _, m := range existing {
		if m.ID != "" && m.SourceType != "chat_archive" && m.SourceType != "media_archive" {
			_, _ = fmt.Fprintf(&sb, "- ID: %s | 内容: %s\n", m.ID, m.Text)
		}
	}
	similars := sb.String()
	if similars == "" {
		return content, true
	}

	prompt := fmt.Sprintf(`你是飞书陪伴机器人的记忆整合管家。判断新提炼的候选记忆与已有的多条长期记忆是否存在冲突、冗余或重合。
新候选记忆：%s

已有的相似记忆：
%s

只返回 JSON，不要任何解释。格式：
{"action":"none|ignore|delete|update","target_id":"冲突的老记忆ID","merged_content":"融合更新后的内容","reason":"理由"}

规则：
- none：与已有记忆无冲突、无冗余，这是一条全新的补充事实。
- ignore：新记忆的内容与已有记忆完全一致或高度冗余（老记忆已包含该信息），无需新记，应忽略。
- delete：已有记忆内容已过时、被否定或与新候选发生实质性冲突（比如：老记忆说‘去北京’，新记忆说‘取消北京改去上海’）。此时应删除被冲突的老记忆（即返回老记忆ID，让系统删除它），并让新记忆直接新建。
- update：新老记忆存在信息交集或有补充部分（比如：老记忆说‘喜欢喝可乐’，新记忆说‘平时爱喝无糖可口可乐’）。此时应融合提炼成一句更精确完整的新描述放入 merged_content，并返回对应 target_id 予以更新覆盖（老记忆ID对应的老内容将被删除）。

请仔细辨析。`, content, similars)

	resp, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(256))
	if err != nil {
		log.Printf("[记忆整合] LLM 失败: %v", err)
		return content, true
	}

	var result struct {
		Action        string `json:"action"`
		TargetID      string `json:"target_id"`
		MergedContent string `json:"merged_content"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp)), &result); err != nil {
		log.Printf("[记忆整合] 解析 JSON 失败: %v", err)
		return content, true
	}

	log.Printf("[记忆整理] 行为: %s, 目标ID: %s, 融合后内容: %s", result.Action, result.TargetID, result.MergedContent)

	switch result.Action {
	case "ignore":
		return "", false
	case "delete":
		if result.TargetID != "" {
			_ = memStore.Delete(result.TargetID)
			log.Printf("[记忆整理] 已成功删除被冲突记忆 ID=%s", result.TargetID)
		}
		return content, true
	case "update":
		if result.TargetID != "" {
			_ = memStore.Delete(result.TargetID)
			log.Printf("[记忆整理] 已成功合并更新冲突记忆 ID=%s", result.TargetID)
		}
		if result.MergedContent != "" {
			return result.MergedContent, true
		}
		return content, true
	default:
		return content, true
	}
}

// ---- Streaming reply ----

// emptyReplyPlaceholder is shown when the LLM returns no content, so the user
// never sees a blank message.
const emptyReplyPlaceholder = "（暂时没想好怎么回，稍后再问问我吧）"

// streamingReply streams the LLM reply into a Feishu CardKit streaming card
// (official typewriter interface). If the card never reaches the chat, it
// falls back to text-message + edit streaming so the user still gets a reply.
func streamingReply(
	ctx stdctx.Context,
	llmClient *llm.Client,
	msgs []llm.Message,
	fsClient *feishu.Client,
	msg feishu.Message,
	updateInterval time.Duration,
	maxTokens int,
) (string, error) {
	fullText, sent, err := streamingCardReply(ctx, llmClient, msgs, fsClient, msg, updateInterval, maxTokens)
	if err == nil {
		return fullText, nil
	}
	if sent {
		// Card already delivered (possibly partial) — don't double-send.
		return fullText, err
	}
	log.Printf("[流式] CardKit 流式失败，回退文本编辑: %v", err)
	return streamingTextReply(ctx, llmClient, msgs, fsClient, msg, updateInterval, maxTokens)
}

// streamingCardReply drives the CardKit streaming flow: create a streaming card
// entity, send it, push full text incrementally, then close streaming. The sent
// flag is true once the card is visible in chat, so the caller knows whether a
// fallback would double up.
func streamingCardReply(
	ctx stdctx.Context,
	llmClient *llm.Client,
	msgs []llm.Message,
	fsClient *feishu.Client,
	msg feishu.Message,
	updateInterval time.Duration,
	maxTokens int,
) (fullText string, sent bool, err error) {
	cardJSON := feishu.BuildStreamingCardJSON("")
	cardID, err := fsClient.CreateStreamingCard(ctx, cardJSON)
	if err != nil {
		return "", false, fmt.Errorf("create card: %w", err)
	}
	messageID, err := fsClient.SendCardEntity(ctx, cardID, msg.ChatID)
	if err != nil {
		return "", false, fmt.Errorf("send card: %w", err)
	}
	sent = true
	_ = messageID

	var lastUpdate time.Time
	var updateErr error
	seq := 0
	streamErr := llmClient.ChatStream(ctx, msgs, func(chunk string) {
		fullText += chunk
		now := time.Now()
		if now.Sub(lastUpdate) >= updateInterval || endsWithPunct(chunk) {
			seq++
			if err := fsClient.StreamUpdateCardText(ctx, cardID, feishu.StreamingCardElementID, fullText, seq); err != nil && updateErr == nil {
				updateErr = err
			}
			lastUpdate = now
		}
	}, llm.WithMaxTokens(maxTokens))
	// Final flush + close streaming regardless of stream error.
	if fullText == "" {
		fullText = emptyReplyPlaceholder
	}
	seq++
	finalUpdateErr := fsClient.StreamUpdateCardText(ctx, cardID, feishu.StreamingCardElementID, fullText, seq)
	closeErr := fsClient.CloseStreamingCard(ctx, cardID, seq+1)
	if streamErr != nil {
		return fullText, sent, streamErr
	}
	if updateErr != nil {
		return fullText, sent, fmt.Errorf("stream card update: %w", updateErr)
	}
	if finalUpdateErr != nil {
		return fullText, sent, fmt.Errorf("final card update: %w", finalUpdateErr)
	}
	if closeErr != nil {
		return fullText, sent, fmt.Errorf("close streaming card: %w", closeErr)
	}
	return fullText, sent, nil
}

// streamingTextReply is the fallback: send a text message then edit it as the
// stream progresses. Used only when CardKit streaming is unavailable.
func streamingTextReply(
	ctx stdctx.Context,
	llmClient *llm.Client,
	msgs []llm.Message,
	fsClient *feishu.Client,
	msg feishu.Message,
	updateInterval time.Duration,
	maxTokens int,
) (string, error) {
	initialID, err := fsClient.SendText(ctx, "正在输入...", msg.ChatID)
	if err != nil {
		return "", err
	}

	fullText := ""
	var lastUpdate time.Time
	var updateErr error

	err = llmClient.ChatStream(ctx, msgs, func(chunk string) {
		fullText += chunk
		now := time.Now()
		if now.Sub(lastUpdate) >= updateInterval || endsWithPunct(chunk) {
			if err := fsClient.UpdateTextMessage(ctx, initialID, fullText); err != nil && updateErr == nil {
				updateErr = err
			}
			lastUpdate = now
		}
	}, llm.WithMaxTokens(maxTokens))
	if err != nil {
		return fullText, err
	}
	if updateErr != nil {
		return fullText, fmt.Errorf("update streaming text: %w", updateErr)
	}
	if fullText == "" {
		fullText = emptyReplyPlaceholder
	}
	if err := fsClient.UpdateTextMessage(ctx, initialID, fullText); err != nil {
		return fullText, fmt.Errorf("final streaming text update: %w", err)
	}
	return fullText, nil
}

func endsWithPunct(s string) bool {
	if s == "" {
		return false
	}
	last := rune(s[len(s)-1])
	return last == '.' || last == '。' || last == '！' || last == '?' || last == '？' || last == '\n'
}

// ---- Build prompt ----

func buildSystemPrompt(prof *profile.Profile, mem memory.MemoryStore) string {
	if mem != nil {
		if customPrompt := mem.GetConfig("system_prompt", ""); customPrompt != "" {
			var b strings.Builder
			b.WriteString(customPrompt)
			if state, err := mem.GetRelationshipState(); err == nil {
				_, _ = fmt.Fprintf(&b, " 【当前情感氛围】当前大哥的情绪指数为 %d/100（状态：%s，当分值较小如小于70时，说明他疲惫或受挫，请加倍温和与多给安慰；分值正常时可幽默）；你们当前的亲密度分值为 %d/100。请根据此分值自动微调你的语调。", state.MoodScore, state.LastSentiment, state.AffinityScore)
			}
			b.WriteString(" 【重要记忆与会话一体化原则】提供给你的“相关记忆”中如果包含 `[聊天记录 YYYY-MM-DD HH:MM]` 前缀的文本，这与“对话历史”一样，全都是你与 owner 过去在不同渠道（如微信、飞书等）留下的连续会话足迹。你必须把它们视为一个整体且连贯的聊天时间线。在回复时，请极其自然、直接地融入这些过去的聊天背景，像是一直在同一个会话中无间断聊天一样，直接使用这些历史内容作为你回答的背景和默契事实。绝不要生硬地向用户提及“根据我的记忆”、“在微信聊天记录里查到”等带有机器痕迹的词汇，让所有历史对话上下文与当前对话浑然一体。")
			b.WriteString(" 不要在回复末尾加\"正在输入\"或\"整理中\"这类占位文字。")
			return b.String()
		}
	}

	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "你是%s，%s的小助手。", prof.BotRoleText(), prof.OwnerName)
	if prof.TargetName != "" {
		_, _ = fmt.Fprintf(&b, " 和%s关系亲密。", prof.TargetName)
	}
	b.WriteString(" 你不是owner本人，只是小弟/助手。语气轻松自然，克制不腻。")
	_, _ = fmt.Fprintf(&b, " 必须分清发言人身份：owner 是%s", prof.OwnerDisplay())
	if target := prof.TargetDisplay(); target != "" {
		_, _ = fmt.Fprintf(&b, "，target 是%s", target)
	}
	if prof.BotName != "" {
		_, _ = fmt.Fprintf(&b, "，机器人是%s。", prof.BotName)
	} else {
		b.WriteString("，机器人是小弟。")
	}
	b.WriteString(" 不要把当前发言人叫成另一个人。")
	if roster := prof.IdentityRoster(); roster != "" {
		b.WriteString(" 已知成员表：\n")
		b.WriteString(roster)
	}

	// Dynamic sentiment and relationship injection
	if mem != nil {
		if state, err := mem.GetRelationshipState(); err == nil {
			_, _ = fmt.Fprintf(&b, " 【当前情感氛围】当前大哥的情绪指数为 %d/100（状态：%s，当分值较小如小于70时，说明他疲惫或受挫，请加倍温和与多给安慰；分值正常时可幽默）；你们当前的亲密度分值为 %d/100。请根据此分值自动微调你的语调。", state.MoodScore, state.LastSentiment, state.AffinityScore)
		}
	}
	b.WriteString(" 【重要记忆与会话一体化原则】提供给你的“相关记忆”中如果包含 `[聊天记录 YYYY-MM-DD HH:MM]` 前缀的文本，这与“对话历史”一样，全都是你与 owner 过去在不同渠道（如微信、飞书等）留下的连续会话足迹。你必须把它们视为一个整体且连贯的聊天时间线。在回复时，请极其自然、直接地融入这些过去的聊天背景，像是一直在同一个会话中无间断聊天一样，直接使用这些历史内容作为你回答的背景和默契事实。绝不要生硬地向用户提及“根据我的记忆”、“在微信聊天记录里查到”等带有机器痕迹的词汇，让所有历史对话上下文与当前对话浑然一体。")
	b.WriteString(" 不要在回复末尾加\"正在输入\"或\"整理中\"这类占位文字。")
	if persona, ok := prof.Config["persona"].(string); ok && persona != "" {
		b.WriteString(" ")
		b.WriteString(persona)
	}
	return b.String()
}

func senderLabel(cfg *config.Config, prof *profile.Profile, fs *feishu.Client, openID string) string {
	if member, ok := prof.MemberByOpenID(openID); ok {
		return member.DisplayName()
	}
	ownerDisp := prof.OwnerDisplay()
	botDisp := prof.BotName
	targetDisp := prof.TargetDisplay()

	if globalMemStore != nil {
		if u := globalMemStore.GetConfig("user_name", ""); u != "" {
			ownerDisp = u
		}
		if b := globalMemStore.GetConfig("bot_name", ""); b != "" {
			botDisp = b
		}
		if t := globalMemStore.GetConfig("partner_name", ""); t != "" {
			targetDisp = t
		}
	}

	return fs.LabelSender(openID, ownerDisp, botDisp, targetDisp)
}

func audienceForSender(cfg *config.Config, prof *profile.Profile, msg feishu.Message) string {
	if msg.IsOwner || (cfg.FeishuOwnerOpenID != "" && msg.Sender == cfg.FeishuOwnerOpenID) {
		return "owner"
	}
	if cfg.FeishuTargetOpenID != "" && msg.Sender == cfg.FeishuTargetOpenID {
		return "target"
	}
	switch prof.MemberRole(msg.Sender) {
	case "owner":
		return "owner"
	case "target", "partner":
		return "target"
	default:
		return "other"
	}
}

func buildChatMessages(prof *profile.Profile, mem memory.MemoryStore, currentContent string, currentSender string, currentMessageID string, recentMsgs []feishu.Message, memories []string, extraInstructions string, budget *ctxmgr.Budget, senderLabel func(string) string) []llm.Message {
	systemPrompt := buildSystemPrompt(prof, mem)
	safeCurrentMessage := fmt.Sprintf("【这是用户最新发的消息，请针对它回复，不要回复历史里的消息】\n当前发言人：%s\n消息：%s", currentSender, safety.SanitizeForLLM(currentContent))

	// Reserve high-priority static elements
	budget.Reserve("system_prompt", systemPrompt)
	budget.Reserve("current_message", safeCurrentMessage)
	if extraInstructions != "" {
		budget.Reserve("extra_instructions", extraInstructions)
	}

	// 1. Pack recent messages (second priority) - chronological order (oldest first)
	// We scan newest-first to fill the budget, but we will output chronologically
	var ctxLines []string
	if len(recentMsgs) > 0 {
		for i := 0; i < len(recentMsgs); i++ {
			m := recentMsgs[i]
			if m.MessageID == currentMessageID {
				continue
			}
			line := fmt.Sprintf("[%s] %s: %s", m.Time, senderLabel(m.Sender), safety.SanitizeForLLM(m.Content))
			// We check if we can fit it. If yes, we keep it.
			// ListMessages returns newest-first, so i=0 is newest, i=len-1 is oldest.
			// We fill starting from the newest historical turns.
			if budget.CanFit(len(line) + 1) {
				budget.Reserve("recent_message_turn", line)
				// Prepend since we are traversing newest-first but want chronological order (oldest first)
				ctxLines = append([]string{line}, ctxLines...)
			} else {
				// Stop filling history if we exceed budget
				break
			}
		}
	}

	// 2. Pack memories (third priority)
	var safeMemories []string
	if len(memories) > 0 {
		for _, memory := range memories {
			sanitized := safety.SanitizeForLLM(memory)
			if budget.CanFit(len(sanitized) + 1) {
				budget.Reserve("memory_item", sanitized)
				safeMemories = append(safeMemories, sanitized)
			} else {
				break
			}
		}
	}

	// Assemble final messages for KV cache friendliness:
	// 1. System Prompt
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{Role: "system", Content: systemPrompt})

	// 2. Memory context (semi-static)
	if len(safeMemories) > 0 {
		msgs = append(msgs, llm.Message{Role: "system", Content: "相关记忆：\n" + strings.Join(safeMemories, "\n")})
	}

	// 3. Historical conversation context
	if len(ctxLines) > 0 {
		msgs = append(msgs, llm.Message{Role: "system", Content: "对话历史（早→近，不含最新消息）：\n" + strings.Join(ctxLines, "\n")})
	}

	// 4. Extra instructions
	if extraInstructions != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: extraInstructions})
	}

	// 5. Final User Turn
	msgs = append(msgs, llm.Message{Role: "user", Content: safeCurrentMessage})

	budget.Log()
	return msgs
}

// ---- Health card ----

func buildHealthCard(cfg *config.Config, fs *feishu.Client, llmClient *llm.Client, mem memory.MemoryStore) map[string]interface{} {
	h := health.NewChecker(
		func(ctx stdctx.Context) error {
			if fs == nil {
				return fmt.Errorf("飞书客户端未初始化")
			}
			return fs.HealthCheck(ctx, cfg.FeishuChatID)
		},
		func(ctx stdctx.Context) error {
			if llmClient == nil {
				return fmt.Errorf("no LLM client")
			}
			_, err := llmClient.Chat(ctx, []llm.Message{{Role: "user", Content: "hi"}}, llm.WithMaxTokens(5))
			return err
		},
		func() int {
			if mem == nil {
				return 0
			}
			return len(mem.All())
		},
	)
	result := h.Check(stdctx.Background())
	return result.BuildCard()
}

// ---- Memory audit card ----

func buildMemoryAuditCard(mem memory.MemoryStore, audience string) map[string]interface{} {
	var elements []interface{}
	headerTitle := "记忆审计"

	if mem == nil {
		elements = []interface{}{
			map[string]interface{}{
				"tag":     "markdown",
				"content": "记忆库未初始化",
			},
		}
	} else {
		all := mem.All()
		var filtered []memory.Memory
		for _, m := range all {
			if !memoryVisibleTo(m.Visibility, audience) {
				continue
			}
			filtered = append(filtered, m)
		}

		// show last 20
		if len(filtered) > 20 {
			filtered = filtered[len(filtered)-20:]
		}

		var lines []string
		for _, m := range filtered {
			visTag := ""
			switch m.Visibility {
			case memory.VisOwnerOnly:
				visTag = "[仅owner]"
			case memory.VisPublicToTarget:
				visTag = "[共享]"
			case memory.VisPrivate:
				visTag = "[私有]"
			}
			lines = append(lines, fmt.Sprintf("- %s %s", visTag, m.Content))
		}

		if len(lines) == 0 {
			lines = []string{"暂无记忆"}
		}

		headerTitle = fmt.Sprintf("记忆审计 (%d条)", len(all))
		elements = []interface{}{
			map[string]interface{}{
				"tag":     "markdown",
				"content": strings.Join(lines, "\n"),
			},
		}
	}

	return buildCard(headerTitle, "blue", elements)
}

func memoryVisibleTo(vis memory.Visibility, audience string) bool {
	switch vis {
	case memory.VisPrivate:
		return false
	case memory.VisOwnerOnly:
		return audience == "owner"
	case memory.VisPublicToTarget:
		return audience == "owner" || audience == "target"
	default:
		return false
	}
}

// ---- External search adapter ----

func webSearch(query string, cfg *config.Config) ([]search.Result, error) {
	if !cfg.ExternalSearchEnabled {
		return nil, nil
	}
	searchClient := search.NewClient(
		cfg.ExternalSearchBackend,
		cfg.DeerFlowBackendDir,
		cfg.DeerFlowPython,
		cfg.DeerFlowGatewayURL,
		cfg.OpenClawCLI,
	)
	return searchClient.Search(stdctx.Background(), query)
}

func summarizeSearch(query string, results []search.Result, llmClient *llm.Client) string {
	if llmClient == nil || len(results) == 0 {
		return search.Summarize(query, results)
	}

	var sb strings.Builder
	for i, res := range results {
		_, _ = fmt.Fprintf(&sb, "[%d] 标题：%s | 链接：%s\n内容：%s\n\n", i+1, res.Title, res.URL, res.Summary)
	}
	sources := sb.String()

	prompt := fmt.Sprintf(`你是飞书陪伴机器人的外部搜索资料整理小弟。请将搜索到的多条外部参考资料融会贯通，撰写成一段流畅、有人设温度、有事实依据的解答。

用户的问题：%s

外部参考资料：
%s

回答规则：
- 对内容进行归纳重组，让回答读起来像一个充满朝气和耐心的机器人小弟（“老板，您来得正好！我帮您查了查……”）。
- 【非常重要】你的每一句核心事实结论，必须在句尾标注其来源资料的序号。例如：
  “Google在2026年发布了Gemini 3.5 Flash模型，推理速度快且性价比高[1]。”
- 在回答的尾部换行并打印具体的「参考资料：」列表。格式：
  [1] 标题：链接
  [2] 标题：链接
- 尽量覆盖所有相关的关键信息，不要只敷衍列举一两条。

只返回整理出的最终回答内容，不要包含任何系统调试信息。`, query, sources)

	resp, err := llmClient.Chat(stdctx.Background(), []llm.Message{
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0.2), llm.WithMaxTokens(1000))
	if err != nil {
		log.Printf("[搜索合成] LLM 失败: %v", err)
		return search.Summarize(query, results)
	}

	return resp
}

// ---- Main entry ----

var (
	passiveAssistant = NewPassiveAssistant()
	convoState       = newStateManager()
)

var (
	flagActions = flag.Bool("actions", false, "Run in GitHub Actions mode (no WebSocket listener)")
)

// processedMsgIDs tracks recently handled message IDs so redelivered messages
// (Feishu retries when the handler is slow to ACK) are skipped instead of
// answered twice.
var (
	processedMsgIDs = make(map[string]time.Time)
	processedMu     sync.Mutex
)

const processedMsgTTL = 30 * time.Minute

// alreadyProcessed records msgID and reports whether it was already seen
// recently. An empty msgID is never considered a duplicate.
func alreadyProcessed(msgID string) bool {
	if msgID == "" {
		return false
	}
	processedMu.Lock()
	defer processedMu.Unlock()
	now := time.Now()
	for id, t := range processedMsgIDs {
		if now.Sub(t) > processedMsgTTL {
			delete(processedMsgIDs, id)
		}
	}
	if _, ok := processedMsgIDs[msgID]; ok {
		return true
	}
	processedMsgIDs[msgID] = now
	return false
}

// interpretMediaRequest uses the LLM to understand, in conversation context,
// what images the user wants and whether they want all of them — resolving
// follow-up references like "再/那张/全部" against recent messages. Returns
// the search query (empty if the message isn't really an image request) and
// whether to send every match.
func interpretMediaRequest(ctx stdctx.Context, llmClient *llm.Client, msg feishu.Message, recentMsgs []feishu.Message, label func(string) string, prof *profile.Profile) (query string, showAll bool) {
	if llmClient == nil {
		return msg.Content, false // fallback: search the raw message
	}
	var ctxLines []string
	for _, m := range recentMsgs {
		ctxLines = append(ctxLines, fmt.Sprintf("[%s] %s: %s", m.Time, label(m.Sender), safety.SanitizeForLLM(m.Content)))
	}
	prompt := fmt.Sprintf(`用户在飞书聊天里找图片。根据近期对话理解用户最新这条消息想找什么，只返回 JSON：{"query":"关键词","show_all":true/false}。
- query：用于图片库检索的关键词（如 美食/蚊子/小区/截图/聊天记录），要结合上下文解析"再/那张/全部/另一张"等指代；如果最新消息明显不是找图片，query 为空字符串。
- show_all：用户要看全部相关图片（"全部/都看看/都要/都发"）时为 true，否则 false。

近期对话：
%s

用户最新消息：%s`, strings.Join(ctxLines, "\n"), safety.SanitizeForLLM(msg.Content))
	reply, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: buildSystemPrompt(prof, nil)},
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(120))
	if err != nil {
		log.Printf("[图片记忆] 查询理解失败: %v", err)
		return msg.Content, false
	}
	m := regexp.MustCompile(`\{.*\}`).FindString(reply)
	if m == "" {
		return msg.Content, false
	}
	var parsed struct {
		Query   string `json:"query"`
		ShowAll bool   `json:"show_all"`
	}
	if err := json.Unmarshal([]byte(m), &parsed); err != nil {
		return msg.Content, false
	}
	return strings.TrimSpace(parsed.Query), parsed.ShowAll
}

// sendOneMediaImage uploads and sends one image, replying to the message when
// possible, logging (not returning) upload errors.
func sendOneMediaImage(ctx stdctx.Context, fs *feishu.Client, msg feishu.Message, path string) {
	if msg.MessageID != "" {
		if err := fs.ReplyImage(ctx, path, msg.MessageID); err != nil {
			log.Printf("[图片记忆] 发送图片失败: %v", err)
		}
	} else if _, err := fs.SendImage(ctx, path, msg.ChatID); err != nil {
		log.Printf("[图片记忆] 发送图片失败: %v", err)
	}
}

func understandIncomingImage(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, msg feishu.Message, mem memory.MemoryStore) string {
	if msg.ImageKey == "" {
		log.Printf("[图片理解] 图片消息没有 image_key: message_id=%s", msg.MessageID)
		return ""
	}
	image, err := fs.DownloadImage(ctx, msg.ImageKey)
	if err != nil {
		log.Printf("[图片理解] 下载飞书图片失败: %v", err)
		return ""
	}

	// Calculate MD5 hash
	hash := fmt.Sprintf("%x", md5.Sum(image))
	if mem != nil {
		if ocr, caption, err := mem.GetImageHashCache(hash); err == nil && (ocr != "" || caption != "") {
			log.Printf("[图片理解] 命中 MD5 缓存秒懂: hash=%s", hash)
			var cacheParts []string
			if ocr != "" {
				cacheParts = append(cacheParts, "OCR文字："+ocr)
			}
			if caption != "" {
				cacheParts = append(cacheParts, "视觉描述："+caption)
			}
			return strings.Join(cacheParts, "\n")
		}
	}
	var ocrText string
	var captionText string

	type understandResult struct {
		ocr     string
		caption string
		err     error
	}
	resChan := make(chan understandResult, 2)

	// 1. OCR chain: Apple Vision locally, then Feishu as a cross-platform fallback.
	go func() {
		text, backend, err := recognizeImageText(ctx, cfg, fs, image)
		if err != nil {
			resChan <- understandResult{err: err}
			return
		}
		if text != "" {
			log.Printf("[图片理解] OCR backend=%s chars=%d", backend, len([]rune(text)))
		}
		resChan <- understandResult{ocr: text}
	}()

	// 2. 并发协程 B: Ollama 本地多模态 Vision 图像描述
	go func() {
		if !cfg.LocalVisionEnabled || cfg.OllamaVisionModel == "" {
			resChan <- understandResult{}
			return
		}
		desc, err := describeImageWithOllama(ctx, cfg, image)
		if err != nil {
			resChan <- understandResult{err: fmt.Errorf("ollama vision error: %w", err)}
			return
		}
		resChan <- understandResult{caption: desc}
	}()

	// 3. 阻塞等待两个协程的结果，同时支持上下文超时/取消退出
	for i := 0; i < 2; i++ {
		select {
		case <-ctx.Done():
			log.Printf("[图片理解] 上下文结束，终止图片并发分析等待")
			return ""
		case res := <-resChan:
			if res.err != nil {
				log.Printf("[图片理解] 协程执行出错: %v", res.err)
				continue
			}
			if res.ocr != "" {
				ocrText = res.ocr
			}
			if res.caption != "" {
				captionText = res.caption
			}
		}
	}

	var parts []string
	if ocrText != "" {
		parts = append(parts, "OCR文字："+ocrText)
	}
	if captionText != "" {
		parts = append(parts, "视觉描述："+captionText)
	}

	// 4. 对生成的图片细节存入 MD5 秒懂缓存，防范重复提取
	if mem != nil && (ocrText != "" || captionText != "") {
		_ = mem.SaveImageHashCache(hash, ocrText, captionText)
	}
	if saver, ok := mem.(managedMediaStore); ok {
		mediaKey := msg.MessageID
		if mediaKey == "" {
			mediaKey = msg.ImageKey
		}
		data := append([]byte(nil), image...)
		go func() {
			saveCtx, cancel := stdctx.WithTimeout(stdctx.Background(), 45*time.Second)
			defer cancel()
			_, saveErr := saver.SaveManagedMedia(saveCtx, memory.ManagedMediaInput{
				MediaKey: mediaKey, Data: data, SourcePath: mediaKey,
				Sender: msg.Sender, SentAt: time.Now().Unix(), OCRText: ocrText, Caption: captionText,
			})
			if saveErr != nil {
				log.Printf("[媒体入库] 保存失败 message_id=%s: %v", msg.MessageID, saveErr)
			}
		}()
	}

	return strings.Join(parts, "\n")
}

func recognizeImageText(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, image []byte) (string, string, error) {
	backend := strings.ToLower(strings.TrimSpace(cfg.LocalOCRBackend))
	if backend == "" {
		backend = "auto"
	}
	if cfg.LocalOCREnabled && backend != "feishu" {
		engine := localocr.NewAppleVision(cfg.AppleVisionOCRPath, cfg.LocalOCRTimeout)
		result, err := engine.RecognizeBytes(ctx, image)
		if err == nil && result.Text != "" {
			return result.Text, fmt.Sprintf("apple_vision/%dms", result.ElapsedMS), nil
		}
		if err != nil {
			log.Printf("[图片理解] Apple Vision OCR 不可用: %v", err)
		}
		if backend == "apple_vision" {
			return "", "apple_vision", err
		}
	}
	if !cfg.FeishuOCREnabled {
		return "", "disabled", nil
	}
	texts, err := fs.RecognizeImageText(ctx, image, cfg.FeishuOCRCooldown)
	if err != nil {
		return "", "feishu", fmt.Errorf("feishu OCR error: %w", err)
	}
	return strings.Join(texts, "\n"), "feishu", nil
}

func describeImageWithOllama(ctx stdctx.Context, cfg *config.Config, image []byte) (string, error) {
	if len(image) == 0 {
		return "", fmt.Errorf("empty image")
	}
	baseURL := strings.TrimRight(cfg.OllamaBaseURL, "/")
	body := map[string]interface{}{
		"model":  cfg.OllamaVisionModel,
		"prompt": "请用中文简洁描述这张图片。如果图片里有文字，请尽量逐字读出来；如果是聊天截图，也说清楚关键信息。不要编造看不见的细节。",
		"images": []string{base64.StdEncoding.EncodeToString(image)},
		"stream": false,
		"options": map[string]interface{}{
			"temperature": 0.1,
			"num_predict": 220,
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/generate", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(respData)
		if len(snippet) > 800 {
			snippet = snippet[:800]
		}
		return "", fmt.Errorf("ollama http %d: %s", resp.StatusCode, snippet)
	}
	var parsed struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(respData, &parsed); err != nil {
		return "", fmt.Errorf("parse ollama response: %w", err)
	}
	return strings.TrimSpace(parsed.Response), nil
}

func main() {
	flag.Parse()

	if *flagActions {
		runActionsMode()
		return
	}

	runBotMode()
}

func runActionsMode() {
	cfg := config.Load()
	fmt.Println("GitHub Actions 模式 - 执行单次检查")
	prof := loadProfile(cfg)
	// In actions mode, just do a single GitHub poll and exit
	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	ghClient := github.NewClient(cfg.GitHubUsername, cfg.GitHubToken)
	fsClient := feishu.NewClient(cfg.FeishuAppID, cfg.FeishuAppSecret, cfg.FeishuBotOpenID)

	ctx, cancel := stdctx.WithCancel(stdctx.Background())
	defer cancel()

	ghState, err := state.Load("memory_data", cfg.ProfileID)
	if err != nil {
		log.Printf("初始化 GitHub 状态失败: %v", err)
		return
	}

	checkGitHub(ctx, cfg, ghClient, fsClient, llmClient, ghState, prof)
	fmt.Println("GitHub Actions 模式完成")
}

func runBotMode() {
	cfg := config.Load()

	if cfg.DryRun {
		fmt.Println("DRY RUN 模式 — 只打印，不真正发送飞书消息")
	}

	prof := loadProfile(cfg)

	var memStore memory.MemoryStore
	var err error
	if cfg.MemoryEnabled {
		var embedder memory.Embedder
		if cfg.OllamaModel != "" {
			embedder = memory.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.OllamaModel)
		}
		if cfg.MemoryDatabaseDSN != "" {
			memStore, err = memory.NewDatabaseStore(memory.DatabaseOptions{
				DSN:                   cfg.MemoryDatabaseDSN,
				ProfileID:             cfg.ProfileID,
				IncludeChatArchive:    cfg.MemoryIncludeChatArchive,
				ChatVisibility:        memory.Visibility(cfg.MemoryChatVisibility),
				ChatArchiveTable:      cfg.MemoryChatArchiveTable,
				ChatArchiveTextColumn: cfg.MemoryChatArchiveTextColumn,
				ChatArchiveTimeColumn: cfg.MemoryChatArchiveTimeColumn,
				IncludeMediaArchive:   cfg.MemoryIncludeMediaArchive,
				MediaVisibility:       memory.Visibility(cfg.MemoryMediaVisibility),
				MediaArchiveTable:     cfg.MemoryMediaArchiveTable,
				MediaOCRColumn:        cfg.MemoryMediaOCRColumn,
				MediaCaptionColumn:    cfg.MemoryMediaCaptionColumn,
				MediaTimeColumn:       cfg.MemoryMediaTimeColumn,
				MediaSenderColumn:     cfg.MemoryMediaSenderColumn,
				MediaFilePathColumn:   cfg.MemoryMediaFilePathColumn,
				MediaMsgIDColumn:      cfg.MemoryMediaMsgIDColumn,
				MediaStatusColumn:     cfg.MemoryMediaStatusColumn,
				MediaRoot:             cfg.MemoryMediaRoot,
				MediaVault:            cfg.MemoryMediaVault,
				Embedder:              embedder,
				EmbeddingDimension:    cfg.MemoryEmbeddingDimension,
			})
			if err != nil {
				log.Printf("初始化数据库记忆库失败: %v", err)
			} else {
				globalMemStore = memStore
				if embedder != nil {
					log.Printf("[记忆] 使用 OceanBase/MySQL + Ollama hybrid retrieval: profile=%s include_chat_archive=%v include_media_archive=%v embed=%s/%s", cfg.ProfileID, cfg.MemoryIncludeChatArchive, cfg.MemoryIncludeMediaArchive, cfg.OllamaBaseURL, cfg.OllamaModel)
				} else {
					log.Printf("[记忆] 使用 OceanBase/MySQL: profile=%s include_chat_archive=%v include_media_archive=%v", cfg.ProfileID, cfg.MemoryIncludeChatArchive, cfg.MemoryIncludeMediaArchive)
				}
			}
		} else {
			if embedder != nil {
				log.Printf("[记忆] 使用 Ollama embedding: %s/%s", cfg.OllamaBaseURL, cfg.OllamaModel)
			} else {
				embedder = &memory.HashEmbedder{}
				log.Println("[记忆] 使用 Hash embedding (本地 fallback)")
			}
			memStore, err = memory.NewSearchStore(cfg.ProfileID, "memory_data", embedder)
			if err != nil {
				log.Printf("初始化记忆库失败: %v", err)
			} else {
				globalMemStore = memStore
			}
		}
	}

	if memStore != nil {
		webPort := os.Getenv("PORT")
		if webPort == "" {
			webPort = "8080"
		}
		go startHTTPServer(webPort, memStore)
	}

	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	fsClient := feishu.NewClient(cfg.FeishuAppID, cfg.FeishuAppSecret, cfg.FeishuBotOpenID)
	fsClient.SetOwnerOpenID(cfg.FeishuOwnerOpenID)
	fsClient.SetTargetOpenID(cfg.FeishuTargetOpenID)
	ghClient := github.NewClient(cfg.GitHubUsername, cfg.GitHubToken)

	// Load persistent state for GitHub polling idempotency
	ghState, err := state.Load("memory_data", cfg.ProfileID)
	if err != nil {
		log.Printf("初始化 GitHub 状态失败: %v", err)
	}

	ctx, cancel := stdctx.WithCancel(stdctx.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Println("收到退出信号，正在关闭...")
		cancel()
	}()

	// Start GitHub polling
	go githubPollLoop(ctx, cfg, ghClient, fsClient, llmClient, ghState, prof)
	if cfg.LoveNoteEnabled && ghState != nil {
		loveNotes := lovenote.New(cfg, llmClient, ghState, prof)
		interval := cfg.LoveNoteCheckInterval
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		go func() {
			if err := loveNotes.Run(ctx); err != nil {
				log.Printf("[恋爱笔记] 启动检查跳过: %v", err)
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			log.Println("[恋爱笔记] 增量评论任务已启动")
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := loveNotes.Run(ctx); err != nil {
						log.Printf("[恋爱笔记] 本轮跳过: %v", err)
					}
				}
			}
		}()
	}

	// Start proactive topic loop
	if cfg.ProactiveTopicEnabled {
		go proactiveTopicLoop(ctx, cfg, fsClient)
	}

	// Start Feishu listener
	handlers := feishu.Handlers{
		OnMessage: func(msg feishu.Message) {
			onMessageReceived(ctx, cfg, prof, memStore, llmClient, fsClient, msg)
		},
		OnPassiveMsg: func(msg feishu.Message) {
			convoState.UpdateMessage(msg, audienceForSender(cfg, prof, msg))
			passiveAssistant.OnMessage(msg)
		},
		OnCardAction: func(action feishu.CardAction) string {
			return onCardAction(ctx, cfg, prof, memStore, llmClient, fsClient, action)
		},
	}

	log.Println("启动飞书长连接监听...")
	go func() {
		backoff := 1 * time.Second
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			err := fsClient.StartListening(ctx, handlers)
			if err != nil {
				select {
				case <-ctx.Done():
					log.Println("[飞书长连接] 优雅退出。")
					return
				default:
				}

				log.Printf("[飞书长连接] 异常断开: %v。将在 %v 后尝试自动重新连接...", err, backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}

				backoff *= 2
				if backoff > 60*time.Second {
					backoff = 60 * time.Second
				}
			} else {
				backoff = 1 * time.Second
			}
		}
	}()

	<-ctx.Done()
	log.Println("机器人主程序已优雅退出。")
}

// loadProfile loads the profile for cfg.ProfileID, falling back to a generic
// default so no private owner/target names are hard-coded in open source.
func loadProfile(cfg *config.Config) *profile.Profile {
	profilesDir := os.Getenv("PROFILES_DIR")
	if profilesDir == "" {
		// Default to profiles/ relative to the binary
		exePath, err := os.Executable()
		if err == nil {
			profilesDir = filepath.Join(filepath.Dir(exePath), "profiles")
		} else {
			profilesDir = "profiles"
		}
	}
	prof, err := profile.Load(cfg.ProfileID, profilesDir)
	if err != nil {
		log.Printf("加载 profile %s 失败: %v，使用默认", cfg.ProfileID, err)
		return defaultProfile(cfg.ProfileID)
	}
	return prof
}

func defaultProfile(id string) *profile.Profile {
	return &profile.Profile{
		ID:         id,
		Name:       "飞书陪伴机器人",
		BotRole:    "飞书陪伴机器人小弟",
		OwnerName:  "老板",
		TargetName: "",
	}
}

// ---- GitHub polling loop ----

func githubPollLoop(ctx stdctx.Context, cfg *config.Config, gh *github.Client, fs *feishu.Client, llmClient *llm.Client, st *state.State, prof *profile.Profile) {
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkGitHub(ctx, cfg, gh, fs, llmClient, st, prof)
		}
	}
}

func checkGitHub(ctx stdctx.Context, cfg *config.Config, gh *github.Client, fs *feishu.Client, llmClient *llm.Client, st *state.State, prof *profile.Profile) {
	if st == nil {
		log.Println("[GitHub] 状态未初始化，跳过本轮检查，避免重复推送")
		return
	}

	events, err := gh.FetchEvents(ctx)
	if err != nil {
		log.Printf("获取 GitHub 事件失败: %v", err)
		return
	}
	for _, repo := range cfg.GitHubPrivateRepos {
		commits, err := gh.FetchPrivateCommits(ctx, repo)
		if err != nil {
			log.Printf("获取 private 仓库 %s 失败: %v", repo, err)
			continue
		}
		events = append(events, commits...)
	}
	github.SortByCreatedAt(events)
	events = github.DedupEvents(events)

	if len(events) == 0 {
		return
	}

	// Filter out already-sent events and mark new ones
	var newEvents []github.Event
	for _, e := range events {
		if !st.HasSent(e.ID) {
			newEvents = append(newEvents, e)
			if len(newEvents) >= 10 {
				break
			}
		}
	}

	if len(newEvents) == 0 {
		return
	}

	var activities []github.Activity
	for _, e := range newEvents {
		activities = append(activities, github.ParseActivity(e))
	}

	shouldSend, text := decideGitHubActivityPush(ctx, llmClient, activities, prof)

	for _, e := range newEvents {
		if err := st.MarkSent(e.ID); err != nil {
			log.Printf("[State] 标记事件失败: %v", err)
		}
	}

	if !shouldSend {
		log.Printf("[GitHub] DeepSeek 判断本轮 %d 条新动态暂不推送", len(activities))
		return
	}
	if text == "" {
		text = buildGitHubActivitySentence(activities, prof.OwnerName)
	}
	if _, err := fs.SendText(ctx, text, cfg.FeishuChatID); err != nil {
		log.Printf("发送 GitHub 动态失败: %v", err)
	}
}

type githubPushDecision struct {
	Send bool   `json:"send"`
	Text string `json:"text"`
}

func decideGitHubActivityPush(ctx stdctx.Context, llmClient *llm.Client, activities []github.Activity, prof *profile.Profile) (bool, string) {
	ownerName := prof.OwnerDisplay()
	fallback := buildGitHubActivitySentence(activities, ownerName)
	if len(activities) == 0 {
		return false, ""
	}
	if llmClient == nil {
		return true, fallback
	}

	payload, _ := json.Marshal(activities)

	// Target-aware phrasing: when no intimate target is configured, keep the
	// GitHub activity summary neutral for generic deployments.
	targetName := prof.TargetDisplay()
	rule4 := "commit 要转成自然易懂的话，比如\"给飞书陪伴机器人新增了记忆审核入口\"；star 要说明大概收藏了什么项目。"
	if targetName != "" {
		rule4 = fmt.Sprintf("commit 要转成%s能看懂的话，比如\"给飞书陪伴机器人新增了记忆审核入口\"；star 要说明大概收藏了什么项目。", targetName)
	}
	rule5 := fmt.Sprintf("语气轻量，不要腻，不要自称%s。", ownerName)
	if targetName != "" {
		rule5 = fmt.Sprintf("语气轻量，不要腻，不要自称%s，不要给%s起配置之外的称呼。", ownerName, targetName)
	}

	prompt := fmt.Sprintf(`你是飞书陪伴机器人"小弟"，需要判断一批 GitHub 新动态是否值得主动推送给群里。
要求：
1. 只返回 JSON：{"send":true/false,"text":"..."}。
2. 如果是零碎、重复、无信息量的动态，可以 send=false。
3. 如果值得发，text 必须是一句自然中文，必须包含具体时间点（时:分，如 14:30，不能只写日期）和"%s"的活动，不要表格、不要 Markdown、不要链接。
4. %s
5. %s

GitHub 动态 JSON：
%s`, ownerName, rule4, rule5, string(payload))

	resp, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: "你只输出严格 JSON，不要解释。"},
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0.2), llm.WithMaxTokens(180))
	if err != nil {
		log.Printf("[GitHub] DeepSeek 动态判断失败，使用兜底: %v", err)
		return true, fallback
	}

	var decision githubPushDecision
	if err := json.Unmarshal([]byte(extractJSON(resp)), &decision); err != nil {
		log.Printf("[GitHub] DeepSeek 动态判断 JSON 解析失败，使用兜底: %v", err)
		return true, fallback
	}
	decision.Text = sanitizeGitHubActivitySentence(decision.Text)
	if decision.Send && decision.Text == "" {
		decision.Text = fallback
	}
	return decision.Send, decision.Text
}

func buildGitHubActivitySentence(activities []github.Activity, ownerName string) string {
	if len(activities) == 0 {
		return ""
	}
	if ownerName == "" {
		ownerName = "老板"
	}
	first := activities[0]
	text := normalizeGitHubActivityText(first.Text)
	if len(activities) == 1 {
		return fmt.Sprintf("%s，%s%s。", first.CreatedAt, ownerName, text)
	}
	return fmt.Sprintf("%s，%s有 %d 条新的 GitHub 动态，主要是%s。", first.CreatedAt, ownerName, len(activities), text)
}

func normalizeGitHubActivityText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimSuffix(text, "。")
	replacements := []struct {
		old string
		new string
	}{
		{"向 ", "给 "},
		{" 推送了 ", " 提交了 "},
		{"收藏了 ", "收藏了项目 "},
	}
	for _, r := range replacements {
		text = strings.ReplaceAll(text, r.old, r.new)
	}
	if text == "" {
		return "有了新的 GitHub 动态"
	}
	return text
}

func sanitizeGitHubActivitySentence(text string) string {
	text = strings.TrimSpace(text)
	text = strings.Trim(text, "`")
	text = strings.ReplaceAll(text, "\n", " ")
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

func extractJSON(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```json")
		text = strings.TrimPrefix(text, "```")
		text = strings.TrimSuffix(text, "```")
		text = strings.TrimSpace(text)
	}
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end >= start {
		return text[start : end+1]
	}
	return text
}

// ---- Proactive topic loop ----

func proactiveTopicLoop(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client) {
	ticker := time.NewTicker(cfg.ProactiveTopicCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if passiveAssistant.ShouldSendProactiveTopic() {
				if _, err := fs.SendText(ctx, "最近有没有什么想聊的呀？", cfg.FeishuChatID); err != nil {
					log.Printf("[主动话题] 发送失败: %v", err)
				} else {
					passiveAssistant.MarkTopicSent()
				}
			}
		}
	}
}

// ---- Message handler ----

func onMessageReceived(ctx stdctx.Context, cfg *config.Config, prof *profile.Profile, mem memory.MemoryStore, llmClient *llm.Client, fs *feishu.Client, msg feishu.Message) {
	trace := latency.NewTrace("chat_reply")
	defer trace.Log()

	audience := audienceForSender(cfg, prof, msg)
	initialLabel := senderLabel(cfg, prof, fs, msg.Sender)
	log.Printf("[收到消息] sender=%s label=%s audience=%s is_owner=%v mentioned=%v: %s", msg.Sender, initialLabel, audience, msg.IsOwner, msg.IsMentioned, msg.Content)

	// Idempotency: Feishu redelivers a message if the handler is slow to ACK
	// (e.g. LLM + image upload takes a few seconds). Skip redeliveries we've
	// already processed, so the user never gets duplicate replies.
	if alreadyProcessed(msg.MessageID) {
		log.Printf("[收到消息] 跳过重复投递: %s", msg.MessageID)
		return
	}
	if msg.MsgType == "image" {
		trace.Span("image_understanding")
		if desc := understandIncomingImage(ctx, cfg, fs, msg, mem); desc != "" {
			msg.Content = strings.TrimSpace(msg.Content + "\n图片理解：" + desc)
			log.Printf("[图片理解] %s", desc)
		}
	}
	st := convoState.UpdateMessage(msg, audience)
	passiveAssistant.OnMessage(msg)

	// Classify explicit intents only for private chats and @-mentions.
	// Passive group messages are judged by DeepSeek in planReply.
	intent := IntentNone
	if msg.ChatType != "group" || msg.IsMentioned {
		isRecentMediaSearch := st.LastImageQuery != "" && time.Since(st.LastActiveAt) < 5*time.Minute
		intent = classifyIntent(ctx, llmClient, msg.Content, isRecentMediaSearch)
	}

	label := func(openid string) string {
		return senderLabel(cfg, prof, fs, openid)
	}
	currentSender := label(msg.Sender)
	plan := planReply(ctx, llmClient, msg, intent, *st, prof, currentSender, mem)
	convoState.SetEmotion(msg.ChatID, plan.DetectedEmotion)
	log.Printf("[上下文计划] reply=%v memory=%v query=%q top_k=%d chat=%v media=%v recent=%d budget=%d graph=%v temporal=%v style=%s reason=%s",
		plan.ShouldReply, plan.UseMemory, plan.MemoryQuery, plan.MemoryTopK, plan.IncludeChat, plan.IncludeMedia,
		plan.RecentLimit, plan.ContextMaxChars, plan.UseGraph, plan.TemporalAlign, plan.ReplyStyle, plan.Reason)
	if !plan.ShouldReply {
		return
	}
	if intent == IntentNone && plan.UseMedia {
		intent = IntentMedia
	}

	// Add thinking reaction only after deciding to reply. Passive group
	// messages that are observed but not answered should stay invisible.
	var thinkReactID string
	if msg.MessageID != "" {
		thinkReactID, _ = fs.AddReaction(ctx, msg.MessageID, "THINKING")
	}

	// Handle intent-based tools first
	switch intent {
	case IntentGitHub:
		handleGitHubIntent(ctx, cfg, fs, msg, thinkReactID, llmClient, prof)
		return
	case IntentHealth:
		handleHealthIntent(ctx, cfg, fs, llmClient, mem, msg, thinkReactID)
		return
	case IntentMemoryAudit:
		handleMemoryAuditIntent(ctx, cfg, prof, mem, fs, msg, thinkReactID)
		return
	case IntentSearch:
		handleSearchIntent(ctx, cfg, fs, msg, thinkReactID, llmClient, prof)
		return
	case IntentStatus:
		handleStatusIntent(ctx, cfg, fs, msg, thinkReactID, llmClient, prof)
		return
	case IntentMedia:
		handleMediaIntent(ctx, cfg, mem, fs, msg, thinkReactID, llmClient, prof)
		return
	case IntentRecall:
		if err := fs.RecallLastSent(ctx, msg.ChatID); err != nil {
			log.Printf("[撤回] %v", err)
			if _, replyErr := fs.ReplyText(ctx, "没找到能撤回的消息呀", msg.MessageID); replyErr != nil {
				log.Printf("[撤回] 回复失败: %v", replyErr)
			}
		} else {
			if _, replyErr := fs.ReplyText(ctx, "已撤回～", msg.MessageID); replyErr != nil {
				log.Printf("[撤回] 确认回复失败: %v", replyErr)
			}
		}
		cleanupReaction(ctx, fs, msg, thinkReactID)
		return
	}

	// Read recent messages for context
	trace.Span("read_messages")
	recentMsgs, _ := fs.ListMessages(ctx, msg.ChatID, plan.RecentLimit)

	// Search relevant memories
	var memories []string
	if mem != nil && plan.UseMemory {
		trace.Span("search_memory")
		searchQuery := strings.TrimSpace(plan.MemoryQuery)
		if searchQuery == "" {
			searchQuery = msg.Content
		}
		var graphFacts []string

		if plan.UseGraph && len(plan.DetectedEntities) > 0 {
			resolved := mem.ResolveAliases(plan.DetectedEntities)
			if len(resolved) > 0 {
				searchQuery = strings.Join(resolved, " ") + " " + msg.Content
				relations := mem.GetEntityRelations(resolved)
				for _, rel := range relations {
					graphFacts = append(graphFacts, "[图谱关系] "+rel)
				}
			}
		}

		if retriever, ok := mem.(memory.PlannedRetriever); ok {
			retrieved := retriever.SearchRelevantWithOptions(searchQuery, audience, memory.RetrievalOptions{
				TopK: plan.MemoryTopK, IncludeBotMemory: true,
				IncludeChatArchive: plan.IncludeChat, IncludeMediaArchive: plan.IncludeMedia,
			})
			for _, item := range retrieved {
				memories = append(memories, item.PromptText())
			}
		} else {
			memories = mem.Search(searchQuery, audience)
			if len(memories) > plan.MemoryTopK {
				memories = memories[:plan.MemoryTopK]
			}
		}
		if len(graphFacts) > 0 {
			memories = append(graphFacts, memories...)
		}

		if plan.TemporalAlign && len(memories) > 1 {
			memories = alignTemporalMemory(ctx, llmClient, memories)
		}
		if len(memories) > 0 {
			log.Printf("[记忆] audience=%s 找到 %d 条相关记忆 (包含图谱事实 %d 条)", audience, len(memories), len(graphFacts))
		}
	}

	// Build prompt and call LLM
	budget := ctxmgr.NewBudget(plan.ContextMaxChars)
	llmMsgs := buildChatMessages(prof, mem, msg.Content, currentSender, msg.MessageID, recentMsgs, memories, replyStyleInstruction(plan, *st), budget, label)

	trace.Span("deepseek_call")

	var reply string
	var err error
	streamed := false

	if cfg.StreamingReplyEnabled && msg.ChatType != "group" {
		// CardKit streaming reply for private chats
		reply, err = streamingReply(ctx, llmClient, llmMsgs, fs, msg, cfg.StreamingReplyUpdateInterval, plan.MaxTokens)
		streamed = true
	} else {
		// Non-streaming for group chats
		reply, err = llmClient.Chat(ctx, llmMsgs, llm.WithTemperature(0.7), llm.WithMaxTokens(plan.MaxTokens))
	}

	if err != nil {
		log.Printf("[LLM] 调用失败: %v", err)
		cleanupReaction(ctx, fs, msg, thinkReactID)
		return
	}

	trace.Span("reply_sent")

	if cfg.DryRun {
		log.Printf("[DRY RUN] 回复: %s", reply)
		cleanupReaction(ctx, fs, msg, thinkReactID)
		return
	}

	// Streaming branch already delivered the reply; only send for non-streaming.
	if !streamed {
		if reply == "" {
			reply = emptyReplyPlaceholder
		}
		if msg.MessageID != "" {
			if m, _ := fs.ReplyText(ctx, reply, msg.MessageID); m != "" {
				fs.NoteSent(msg.ChatID, m)
			}
		} else {
			if _, sendErr := fs.SendText(ctx, reply, msg.ChatID); sendErr != nil {
				log.Printf("[回复] 发送失败: %v", sendErr)
			}
		}
	}
	convoState.MarkBotReply(msg.ChatID)

	// Cleanup thinking reaction and add content emoji
	cleanupReaction(ctx, fs, msg, thinkReactID)
	if msg.MessageID != "" {
		if _, reactionErr := fs.AddReaction(ctx, msg.MessageID, pickEmoji(msg.Content, msg.IsOwner, prof)); reactionErr != nil {
			log.Printf("[表情回应] 添加失败: %v", reactionErr)
		}
	}
	if mem != nil && mem.GetConfig("module_emotion_tracker", "true") == "true" {
		go updateEmotionMetrics(ctx, llmClient, mem, msg.Content, reply)
	}

	// Autonomous Memory confirmation check
	if cfg.MemoryConfirmationEnabled && mem != nil && audience != "other" {
		var recentTurns []workingMemoryTurn
		history, err := fs.ListMessages(ctx, msg.ChatID, 8)
		if err == nil && len(history) > 0 {
			for i := len(history) - 1; i >= 0; i-- {
				h := history[i]
				senderLabel := "对方"
				switch h.Sender {
				case cfg.FeishuOwnerOpenID:
					senderLabel = prof.OwnerName
				case cfg.FeishuTargetOpenID:
					senderLabel = prof.TargetDisplay()
				case cfg.FeishuBotOpenID:
					senderLabel = "机器人"
				}
				recentTurns = append(recentTurns, workingMemoryTurn{
					Sender:  senderLabel,
					Content: h.Content,
				})
			}
		}

		shouldRemember, candidate, mType := shouldRememberViaLLM(ctx, msg.Content, msg.IsOwner, prof, llmClient, recentTurns)
		if shouldRemember && candidate != "" {
			// Run memory consolidation to resolve conflicts and duplicates
			mergedCandidate, keep := consolidateMemory(ctx, llmClient, mem, candidate, mType, msg.Sender, prof)
			if keep && mergedCandidate != "" {
				// Autonomous persistence directly into database
				err := mem.Add(memory.Memory{
					Content:    mergedCandidate,
					MemoryType: memory.MemoryType(mType),
					Sender:     msg.Sender,
					Visibility: memory.VisOwnerOnly,
				})
				if err != nil {
					log.Printf("[记忆沉淀] 写入记忆库失败: %v", err)
				} else {
					log.Printf("[记忆沉淀] 自动整理并成功存入长期记忆: %q", mergedCandidate)
					go extractAndSaveGraph(ctx, llmClient, mem, mergedCandidate, recentTurns)
					if _, reactionErr := fs.AddReaction(ctx, msg.MessageID, "SUBMIT"); reactionErr != nil {
						log.Printf("[记忆沉淀] 添加确认表情失败: %v", reactionErr)
					}
				}
			}
		}
	}
}

func handleGitHubIntent(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, msg feishu.Message, reactID string, llmClient *llm.Client, prof *profile.Profile) {
	gh := github.NewClient(cfg.GitHubUsername, cfg.GitHubToken)
	events, err := gh.FetchEvents(ctx)
	if err != nil {
		log.Printf("获取 GitHub 事件失败: %v", err)
		if _, replyErr := fs.ReplyText(ctx, "GitHub 数据拉取失败，稍后再试试", msg.MessageID); replyErr != nil {
			log.Printf("[GitHub] 失败提示发送失败: %v", replyErr)
		}
		cleanupReaction(ctx, fs, msg, reactID)
		return
	}
	for _, repo := range cfg.GitHubPrivateRepos {
		commits, err := gh.FetchPrivateCommits(ctx, repo)
		if err == nil {
			events = append(events, commits...)
		}
	}
	github.SortByCreatedAt(events)
	events = github.DedupEvents(events)

	var activities []github.Activity
	for i, e := range events {
		if i >= 10 {
			break
		}
		activities = append(activities, github.ParseActivity(e))
	}

	_, reply := decideGitHubActivityPush(ctx, llmClient, activities, prof)
	if reply == "" {
		reply = buildGitHubActivitySentence(activities, prof.OwnerName)
	}
	if reply == "" {
		reply = fmt.Sprintf("暂时没拉到%s最近的 GitHub 动态。", prof.OwnerDisplay())
	}
	if msg.MessageID != "" {
		if m, _ := fs.ReplyText(ctx, reply, msg.MessageID); m != "" {
			fs.NoteSent(msg.ChatID, m)
		}
	} else {
		if _, sendErr := fs.SendText(ctx, reply, msg.ChatID); sendErr != nil {
			log.Printf("[GitHub] 回复发送失败: %v", sendErr)
		}
	}
	cleanupReaction(ctx, fs, msg, reactID)
	if _, reactionErr := fs.AddReaction(ctx, msg.MessageID, "DONE"); reactionErr != nil {
		log.Printf("[GitHub] 完成表情添加失败: %v", reactionErr)
	}
	convoState.MarkBotReply(msg.ChatID)
}

func handleHealthIntent(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, llmClient *llm.Client, mem memory.MemoryStore, msg feishu.Message, reactID string) {
	card := buildHealthCard(cfg, fs, llmClient, mem)
	var err error
	if msg.MessageID != "" {
		err = fs.ReplyCard(ctx, card, msg.MessageID)
	} else {
		_, err = fs.SendCard(ctx, card, msg.ChatID)
	}
	if err != nil {
		log.Printf("[健康检查] 卡片发送失败: %v", err)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	if _, err := fs.AddReaction(ctx, msg.MessageID, "DONE"); err != nil {
		log.Printf("[健康检查] 完成表情添加失败: %v", err)
	}
	convoState.MarkBotReply(msg.ChatID)
}

func handleMemoryAuditIntent(ctx stdctx.Context, cfg *config.Config, prof *profile.Profile, mem memory.MemoryStore, fs *feishu.Client, msg feishu.Message, reactID string) {
	audience := audienceForSender(cfg, prof, msg)
	card := buildMemoryAuditCard(mem, audience)
	var err error
	if msg.MessageID != "" {
		err = fs.ReplyCard(ctx, card, msg.MessageID)
	} else {
		_, err = fs.SendCard(ctx, card, msg.ChatID)
	}
	if err != nil {
		log.Printf("[记忆审计] 卡片发送失败: %v", err)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	if _, err := fs.AddReaction(ctx, msg.MessageID, "DONE"); err != nil {
		log.Printf("[记忆审计] 完成表情添加失败: %v", err)
	}
	convoState.MarkBotReply(msg.ChatID)
}

func handleMediaIntent(ctx stdctx.Context, cfg *config.Config, mem memory.MemoryStore, fs *feishu.Client, msg feishu.Message, reactID string, llmClient *llm.Client, prof *profile.Profile) {
	searcher, ok := mem.(mediaMemoryStore)
	if !ok || !cfg.MemoryIncludeMediaArchive {
		if _, err := fs.ReplyText(ctx, "图片回忆还没接到记忆库里，等图片索引跑完并打开 MEMORY_INCLUDE_MEDIA_ARCHIVE 后就能找。", msg.MessageID); err != nil {
			log.Printf("[图片记忆] 提示发送失败: %v", err)
		}
		cleanupReaction(ctx, fs, msg, reactID)
		if _, err := fs.AddReaction(ctx, msg.MessageID, "DONE"); err != nil {
			log.Printf("[图片记忆] 完成表情添加失败: %v", err)
		}
		return
	}

	audience := audienceForSender(cfg, prof, msg)
	label := func(openid string) string {
		return senderLabel(cfg, prof, fs, openid)
	}

	// Understand the request in conversation context: resolves "再/那张/全部"
	// references and detects "show all", so follow-ups work.
	recentMsgs, _ := fs.ListMessages(ctx, msg.ChatID, 100)
	query, showAll := interpretMediaRequest(ctx, llmClient, msg, recentMsgs, label, prof)
	log.Printf("[图片记忆] 理解查询=%q showAll=%v audience=%s 原文=%q", query, showAll, audience, msg.Content)
	if query == "" {
		if _, err := fs.ReplyText(ctx, "我没太明白要找哪张图，可以说具体点：地点、物品、截图里的字，或大概时间。", msg.MessageID); err != nil {
			log.Printf("[图片记忆] 澄清提示发送失败: %v", err)
		}
		cleanupReaction(ctx, fs, msg, reactID)
		if _, err := fs.AddReaction(ctx, msg.MessageID, "DONE"); err != nil {
			log.Printf("[图片记忆] 完成表情添加失败: %v", err)
		}
		return
	}

	limit := 3
	if showAll {
		limit = 8
	}
	results := searcher.SearchMedia(query, audience, limit)
	convoState.MarkMediaSearch(msg.ChatID, query)
	log.Printf("[图片记忆] 查询=%q 命中=%d", query, len(results))
	if len(results) == 0 {
		if _, err := fs.ReplyText(ctx, fmt.Sprintf("图片记忆里没翻到跟“%s”相关的内容，换个词试试？", query), msg.MessageID); err != nil {
			log.Printf("[图片记忆] 空结果提示发送失败: %v", err)
		}
		cleanupReaction(ctx, fs, msg, reactID)
		if _, err := fs.AddReaction(ctx, msg.MessageID, "DONE"); err != nil {
			log.Printf("[图片记忆] 完成表情添加失败: %v", err)
		}
		return
	}

	reply, pickIdx := summarizeMediaResults(ctx, llmClient, query, results, prof)
	if reply == "" {
		reply = fallbackMediaSummary(results)
		pickIdx = 0
	}
	if showAll {
		reply += fmt.Sprintf("\n（共 %d 张，我都发出来。）", len(results))
	} else if len(results) > 1 {
		reply += fmt.Sprintf("\n（还有 %d 张相关的，要看全部就说一声。）", len(results)-1)
	}
	log.Printf("[图片记忆] 回复: %q", reply)
	if msg.MessageID != "" {
		if _, err := fs.ReplyText(ctx, reply, msg.MessageID); err != nil {
			log.Printf("[图片记忆] ReplyText 失败: %v", err)
		}
	} else {
		if _, err := fs.SendText(ctx, reply, msg.ChatID); err != nil {
			log.Printf("[图片记忆] SendText 失败: %v", err)
		}
	}

	if cfg.MemoryMediaSendImage {
		if showAll {
			// Send up to 5 valid images.
			sent := 0
			for _, result := range results {
				if sent >= 5 {
					break
				}
				if result.FilePath == "" {
					continue
				}
				if _, err := os.Stat(result.FilePath); err != nil {
					log.Printf("[图片记忆] 文件不存在: %s: %v", result.FilePath, err)
					continue
				}
				if !isLikelyImage(result.FilePath) {
					log.Printf("[图片记忆] 跳过非图片/坏图文件: %s", result.FilePath)
					continue
				}
				sendOneMediaImage(ctx, fs, msg, result.FilePath)
				sent++
			}
		} else {
			// Send the LLM-picked image first; fall back to the others if it's
			// missing or not a decodable image.
			order := make([]int, 0, len(results))
			order = append(order, pickIdx)
			for i := range results {
				if i != pickIdx {
					order = append(order, i)
				}
			}
			for _, idx := range order {
				result := results[idx]
				if result.FilePath == "" {
					continue
				}
				if _, err := os.Stat(result.FilePath); err != nil {
					log.Printf("[图片记忆] 文件不存在: %s: %v", result.FilePath, err)
					continue
				}
				if !isLikelyImage(result.FilePath) {
					log.Printf("[图片记忆] 跳过非图片/坏图文件: %s", result.FilePath)
					continue
				}
				sendOneMediaImage(ctx, fs, msg, result.FilePath)
				break
			}
		}
	}

	cleanupReaction(ctx, fs, msg, reactID)
	if _, err := fs.AddReaction(ctx, msg.MessageID, "DONE"); err != nil {
		log.Printf("[图片记忆] 完成表情添加失败: %v", err)
	}
	convoState.MarkBotReply(msg.ChatID)
}

// isLikelyImage reports whether the file starts with a recognized image magic
// header. It's a cheap pre-check so the bot never uploads a corrupt or
// non-image file (e.g. WeChat export placeholders that PIL can't decode) to
// Feishu — such files would either be rejected by the upload API or render as
// broken images. Bad-image rows can still surface from the archive when the
// indexer hasn't filtered them, so this guards the send path regardless.
func isLikelyImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var head [12]byte
	n, _ := io.ReadFull(f, head[:])
	b := head[:n]
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF: // JPEG
		return true
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n": // PNG
		return true
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"): // GIF
		return true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP": // WebP
		return true
	}
	return false
}

// summarizeMediaResults asks the LLM to summarize the matched images and pick
// the one most relevant to the query to send, so the summary text and the
// emitted image always agree. Returns (summary, pickIndex); on any failure it
// returns ("", 0) so the caller falls back to the first result.
func summarizeMediaResults(ctx stdctx.Context, llmClient *llm.Client, query string, results []memory.MediaResult, prof *profile.Profile) (string, int) {
	if llmClient == nil {
		return "", 0
	}
	var lines []string
	for i, result := range results {
		lines = append(lines, fmt.Sprintf("[%d] %s", i, safety.SanitizeForLLM(result.ContextText())))
	}
	prompt := fmt.Sprintf(`用户想从图片记忆里找内容。请根据检索到的图片记录，用中文回复 JSON。
要求：
1. 只返回 JSON：{"summary":"一小段自然中文总结","pick":<要发的那张的序号>}。
2. summary 直接说翻到了什么、发的是哪张，带上具体时间点（时:分，不能只写日期）；不要暴露本地文件路径，不要编造未出现的细节。
3. pick 是你最想发出去的那张的序号（0~%d），必须与 summary 描述呼应。
4. 如果像情侣日常回忆，可以温柔一点，但不要腻。

用户问题：%s
图片记录：
%s`, len(results)-1, safety.SanitizeForLLM(query), strings.Join(lines, "\n"))
	reply, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: buildSystemPrompt(prof, nil)},
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0.5), llm.WithMaxTokens(300))
	if err != nil {
		log.Printf("[图片记忆] DeepSeek 总结失败: %v", err)
		return "", 0
	}
	m := regexp.MustCompile(`\{.*\}`).FindString(reply)
	if m == "" {
		return strings.TrimSpace(reply), 0
	}
	var parsed struct {
		Summary string `json:"summary"`
		Pick    int    `json:"pick"`
	}
	if err := json.Unmarshal([]byte(m), &parsed); err != nil || strings.TrimSpace(parsed.Summary) == "" {
		return strings.TrimSpace(reply), 0
	}
	pick := parsed.Pick
	if pick < 0 || pick >= len(results) {
		pick = 0
	}
	return strings.TrimSpace(parsed.Summary), pick
}

func fallbackMediaSummary(results []memory.MediaResult) string {
	var lines []string
	for i, result := range results {
		if i >= 3 {
			break
		}
		desc := result.Caption
		if desc == "" {
			desc = result.OCRText
		}
		if desc == "" {
			desc = "一张聊天图片"
		}
		lines = append(lines, fmt.Sprintf("%s %s：%s", result.SentAt, result.Sender, desc))
	}
	return "我翻到这些图片记忆：\n" + strings.Join(lines, "\n") + "\n我先发一张看看。"
}

func handleSearchIntent(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, msg feishu.Message, reactID string, llmClient *llm.Client, prof *profile.Profile) {
	results, err := webSearch(msg.Content, cfg)
	if err != nil || len(results) == 0 {
		if _, replyErr := fs.ReplyText(ctx, fmt.Sprintf("小弟这边外部搜索暂时没接通，等%s电脑上的本地搜索服务稳一下再查。", prof.OwnerDisplay()), msg.MessageID); replyErr != nil {
			log.Printf("[外部搜索] 失败提示发送失败: %v", replyErr)
		}
	} else {
		summary := summarizeSearch(msg.Content, results, llmClient)
		if _, replyErr := fs.ReplyText(ctx, summary, msg.MessageID); replyErr != nil {
			log.Printf("[外部搜索] 结果发送失败: %v", replyErr)
		}
	}
	cleanupReaction(ctx, fs, msg, reactID)
	if _, reactionErr := fs.AddReaction(ctx, msg.MessageID, "DONE"); reactionErr != nil {
		log.Printf("[外部搜索] 完成表情添加失败: %v", reactionErr)
	}
	convoState.MarkBotReply(msg.ChatID)
}

func handleStatusIntent(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, msg feishu.Message, reactID string, llmClient *llm.Client, prof *profile.Profile) {
	statusReader := localapps.NewReader()
	status, err := statusReader.GetStatus()
	if err != nil {
		log.Printf("[状态] 获取失败: %v", err)
		if _, replyErr := fs.ReplyText(ctx, fmt.Sprintf("小弟这边暂时没有获取到%s的详细状态，等配置好了再告诉你～", prof.OwnerDisplay()), msg.MessageID); replyErr != nil {
			log.Printf("[状态] 失败提示发送失败: %v", replyErr)
		}
	} else {
		statusText := localapps.InterpretStatus(status)
		if _, replyErr := fs.ReplyText(ctx, statusText, msg.MessageID); replyErr != nil {
			log.Printf("[状态] 回复发送失败: %v", replyErr)
		}
	}
	cleanupReaction(ctx, fs, msg, reactID)
	if _, reactionErr := fs.AddReaction(ctx, msg.MessageID, "DONE"); reactionErr != nil {
		log.Printf("[状态] 完成表情添加失败: %v", reactionErr)
	}
	convoState.MarkBotReply(msg.ChatID)
}

func cleanupReaction(ctx stdctx.Context, fs *feishu.Client, msg feishu.Message, reactID string) {
	if reactID != "" && msg.MessageID != "" {
		if err := fs.DeleteReaction(ctx, msg.MessageID, reactID); err != nil {
			log.Printf("[表情回应] 清理失败: %v", err)
		}
	}
}

// ---- Card action handler ----

func onCardAction(ctx stdctx.Context, cfg *config.Config, prof *profile.Profile, mem memory.MemoryStore, llmClient *llm.Client, fs *feishu.Client, action feishu.CardAction) string {
	log.Printf("[卡片回调] action=%s msgID=%s operator=%s", action.Action, action.MessageID, action.OperatorID)
	return "已收到"
}

var _ = io.Discard
var _ = bytes.Buffer{}

func extractAndSaveGraph(ctx stdctx.Context, llmClient *llm.Client, memStore memory.MemoryStore, content string, recentTurns []workingMemoryTurn) {
	if llmClient == nil || memStore == nil || strings.TrimSpace(content) == "" {
		return
	}

	contextBuilder := strings.Builder{}
	if memStore.GetConfig("module_multi_turn_graph", "true") == "true" && len(recentTurns) > 0 {
		contextBuilder.WriteString("\n为了帮助你消解代词（如“她/他/它”指代具体谁）以及推断跨对话的隐含关联，以下是产生这句记忆时的最近对话上下文历史：\n=== 最近对话历史 ===\n")
		for idx, turn := range recentTurns {
			_, _ = fmt.Fprintf(&contextBuilder, "[轮次 %d] %s: %s\n", idx+1, turn.Sender, turn.Content)
		}
		contextBuilder.WriteString("====================\n\n请结合上述对话背景事实，仔细推理出这句记忆中代词所指向的标准实体名。\n")
	}

	prompt := fmt.Sprintf(`你是一个机器人知识图谱提炼专家。请只返回 JSON。
给定一句话（这是一条已经提炼过的机器人长期记忆），请提取出其中的实体（人、地、物、概念等）以及它们之间的关系三元组。
%s
【强制关系 Schema 限制】：为了使图谱关系能演进并自我纠偏，你提炼的关系边名称（relation 字段）必须且只能在以下预定义词中选择（直接输出中文名词）：
- 别名 : 指代大名、网名或昵称（如“小明是阿山的别名” -> ("小明", "别名", "阿山")）
- 喜欢 : 指代喜欢的事物、零食或食物等（如“阿山喜欢吃火锅” -> ("阿山", "喜欢", "火锅")。如果提到“阿山戒了火锅”，仍提炼原关系，后续纠偏程序会处理删除）
- 所在地 : 指代居住地、定居城市或出差所在地等（如“阿山搬去深圳定居了” -> ("阿山", "所在地", "深圳")）
- 同事 : 指代同事关系（如“小林是阿山的同事” -> ("小林", "同事", "阿山")）
- 朋友 : 指代朋友关系
- 妈妈 / 爸爸 : 指代父母关系

你的 JSON 格式必须是：
{
  "entities": [
    {"name": "实体名称，如 阿山", "category": "person|alias|item|place|concept"}
  ],
  "relations": [
    {"src_name": "实体A名称", "relation": "必须在上述强 Schema 限定词中选择", "dst_name": "实体B名称"}
  ]
}

如果没有提取出任何有意义的关系或实体，请返回：
{"entities":[],"relations":[]}

输入记忆：%s`, contextBuilder.String(), content)

	reply, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: "You are a Knowledge Graph entity/relation extractor. Output strictly JSON conformant to format constraints."},
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(350))
	if err != nil {
		log.Printf("[图谱提炼] LLM 调用失败: %v", err)
		return
	}

	var parsed struct {
		Entities []struct {
			Name     string `json:"name"`
			Category string `json:"category"`
		} `json:"entities"`
		Relations []struct {
			SrcName  string `json:"src_name"`
			Relation string `json:"relation"`
			DstName  string `json:"dst_name"`
		} `json:"relations"`
	}

	if err := json.Unmarshal([]byte(extractJSON(reply)), &parsed); err != nil {
		log.Printf("[图谱提炼] JSON 解析失败 raw=%q error=%v", reply, err)
		return
	}

	// 1. Save all entities
	entityIDMap := make(map[string]string)
	for _, ent := range parsed.Entities {
		name := strings.TrimSpace(ent.Name)
		cat := strings.TrimSpace(ent.Category)
		if name != "" && cat != "" {
			id, err := memStore.SaveEntity(name, cat)
			if err == nil {
				entityIDMap[name] = id
			}
		}
	}

	// 2. Save all relations with conflict reconciliation
	for _, rel := range parsed.Relations {
		src := strings.TrimSpace(rel.SrcName)
		r := strings.TrimSpace(rel.Relation)
		dst := strings.TrimSpace(rel.DstName)
		if src != "" && r != "" && dst != "" {
			// 确保主语与宾语实体已保存，防范 JOIN 检索丢失
			_, _ = memStore.SaveEntity(src, "concept")
			_, _ = memStore.SaveEntity(dst, "concept")

			if memStore.GetConfig("module_graph_self_evolution", "true") == "true" {
				existing, err := memStore.GetRelationDestinations(src, r)
				if err == nil && len(existing) > 0 {
					action, targetToRemove := reconcileGraphRelation(ctx, llmClient, src, r, existing, dst, content)
					log.Printf("[图谱调和] 决策: %s -[%s]-> %s, 已有: %v, 动作: %s, 移除目标: %s", src, r, dst, existing, action, targetToRemove)

					if action == "replace" && targetToRemove != "" {
						_ = memStore.DeleteRelation(src, r, targetToRemove)
						log.Printf("[图谱纠偏] 已成功删除被替换的关系边: %s -[%s]-> %s", src, r, targetToRemove)
						if targetToRemove == dst {
							continue
						}
					} else if action == "delete" && targetToRemove != "" {
						_ = memStore.DeleteRelation(src, r, targetToRemove)
						log.Printf("[图谱纠偏] 已成功删除被否定的关系边: %s -[%s]-> %s", src, r, targetToRemove)
						continue
					}
				}
			}

			err = memStore.SaveRelation(src, r, dst)
			if err != nil {
				log.Printf("[图谱提炼] 保存关系 %s -[%s]-> %s 失败: %v", src, r, dst, err)
			} else {
				log.Printf("[图谱提炼] 成功同步图谱三元组: (%s, %s, %s)", src, r, dst)
			}
		}
	}
}

func reconcileGraphRelation(ctx stdctx.Context, llmClient *llm.Client, srcName string, relation string, existingDsts []string, newDst string, contextText string) (action string, targetToRemove string) {
	if llmClient == nil || len(existingDsts) == 0 {
		return "keep_both", ""
	}

	prompt := fmt.Sprintf(`你是一个知识图谱关系冲突调和专家。只返回 JSON。
我们要将一条新提取出的关系，合并至已有的知识图谱中。请分析是否存在时效性或逻辑互斥性冲突。

主语：%s
关系类型：%s
当前已有的目标实体值：%v
新提炼的目标实体值：%s
背景上下文事实（来源句子）：%s

决策规则：
1. 【keep_both】：新旧目标值可以并存（例如：“喜欢吃火锅”和“喜欢吃牛肉干”可以并存；同事“小李”和“小王”可以并存）。
2. 【replace】：新值和旧值存在逻辑排他性冲突（例如：一个人通常只能在一个“location”定居，新的定居地“深圳”应当替换旧的“北京”；感情状态“单身”必须替换“恋爱中”）。你需要明确指出要替换的旧实体值。
3. 【delete】：新背景上下文事实如果明确表达否定、戒掉、不再做、不再喜欢（例如“戒了火锅”、“不吃火锅了”，即使提炼的新值依然写的是火锅，但由于背景在否定它），必须决策为 delete，并把 target_to_remove 填为被否定的旧实体值（如 火锅）。

请返回以下 JSON 格式：
{
  "action": "keep_both|replace|delete",
  "target_to_remove": "如果选择 replace 或 delete，需要在此填入具体的冲突旧值实体名称（如 北京）。如果没有，返回空字符串"
}

只返回 JSON。`, srcName, relation, existingDsts, newDst, contextText)

	reply, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: "You are a Knowledge Graph conflict resolver. Output strictly JSON conformant to format constraints."},
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(150))
	if err != nil {
		log.Printf("[图谱调和] 决策调用失败，默认并存: %v", err)
		return "keep_both", ""
	}

	var parsed struct {
		Action         string `json:"action"`
		TargetToRemove string `json:"target_to_remove"`
	}
	if err := json.Unmarshal([]byte(extractJSON(reply)), &parsed); err != nil {
		log.Printf("[图谱调和] JSON 解析失败 raw=%q, 默认并存", reply)
		return "keep_both", ""
	}

	return parsed.Action, parsed.TargetToRemove
}

func startHTTPServer(port string, memStore memory.MemoryStore) {
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/db-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		if r.Method == "OPTIONS" {
			return
		}

		dbStore, ok := memStore.(*memory.DatabaseStore)
		if !ok {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status": "offline",
				"error":  "not in database mode",
			})
			return
		}
		dbConn := dbStore.GetDBConn()

		status := "online"
		pingErr := dbConn.Ping()
		if pingErr != nil {
			status = "offline"
		}

		var version string
		_ = dbConn.QueryRow("SELECT VERSION()").Scan(&version)

		stats := dbConn.Stats()

		var entityCount, relationCount int
		_ = dbConn.QueryRow("SELECT COUNT(*) FROM knowledge_entities").Scan(&entityCount)
		_ = dbConn.QueryRow("SELECT COUNT(*) FROM knowledge_relations").Scan(&relationCount)

		response := map[string]interface{}{
			"status":         status,
			"version":        version,
			"max_open_conns": stats.MaxOpenConnections,
			"open_conns":     stats.OpenConnections,
			"in_use_conns":   stats.InUse,
			"idle_conns":     stats.Idle,
			"wait_count":     stats.WaitCount,
			"entity_count":   entityCount,
			"relation_count": relationCount,
			"profile_id":     dbStore.GetProfileID(),
		}

		_ = json.NewEncoder(w).Encode(response)
	})

	mux.HandleFunc("/api/configs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		if r.Method == "OPTIONS" {
			return
		}

		if r.Method == "GET" {
			configs := map[string]string{
				"module_emotion_tracker":      memStore.GetConfig("module_emotion_tracker", "true"),
				"module_graph_self_evolution": memStore.GetConfig("module_graph_self_evolution", "true"),
				"module_multi_turn_graph":     memStore.GetConfig("module_multi_turn_graph", "true"),
				"module_image_dedup":          memStore.GetConfig("module_image_dedup", "true"),
				"system_prompt":               memStore.GetConfig("system_prompt", ""),
				"user_name":                   memStore.GetConfig("user_name", ""),
				"bot_name":                    memStore.GetConfig("bot_name", ""),
				"partner_name":                memStore.GetConfig("partner_name", ""),
				"init_completed":              memStore.GetConfig("init_completed", "false"),
			}
			_ = json.NewEncoder(w).Encode(configs)
			return
		}

		if r.Method == "POST" {
			var body struct {
				Key     string            `json:"key"`
				Value   string            `json:"value"`
				Configs map[string]string `json:"configs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			if len(body.Configs) > 0 {
				for k, v := range body.Configs {
					_ = memStore.SetConfig(k, v)
				}
			} else if body.Key != "" {
				err := memStore.SetConfig(body.Key, body.Value)
				if err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}

			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
	})

	mux.HandleFunc("/api/emotion-trends", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		if r.Method == "OPTIONS" {
			return
		}

		states, times, err := memStore.GetEmotionHistory()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		type TrendPoint struct {
			Timestamp int64  `json:"timestamp"`
			Mood      int    `json:"mood"`
			Affinity  int    `json:"affinity"`
			Sentiment string `json:"sentiment"`
		}

		var trends []TrendPoint
		for i := 0; i < len(states); i++ {
			trends = append(trends, TrendPoint{
				Timestamp: times[i],
				Mood:      states[i].MoodScore,
				Affinity:  states[i].AffinityScore,
				Sentiment: states[i].LastSentiment,
			})
		}

		if trends == nil {
			trends = []TrendPoint{}
		}

		_ = json.NewEncoder(w).Encode(trends)
	})

	mux.HandleFunc("/api/graph", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		if r.Method == "OPTIONS" {
			return
		}

		dbStore, ok := memStore.(*memory.DatabaseStore)
		if !ok {
			http.Error(w, "Graph RAG is only supported in database mode", http.StatusBadRequest)
			return
		}
		dbConn := dbStore.GetDBConn()

		if r.Method == "GET" {
			rows, err := dbConn.Query(`
SELECT e1.name, r.relation, e2.name
FROM knowledge_relations r
JOIN knowledge_entities e1 ON r.src_id = e1.id
JOIN knowledge_entities e2 ON r.dst_id = e2.id
ORDER BY r.created_at DESC`)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer func() { _ = rows.Close() }()

			type Trio struct {
				Src      string `json:"src"`
				Relation string `json:"relation"`
				Dst      string `json:"dst"`
			}
			var trios []Trio
			for rows.Next() {
				var t Trio
				if err := rows.Scan(&t.Src, &t.Relation, &t.Dst); err == nil {
					trios = append(trios, t)
				}
			}
			if trios == nil {
				trios = []Trio{}
			}
			_ = json.NewEncoder(w).Encode(trios)
			return
		}

		if r.Method == "POST" {
			var body struct {
				Src      string `json:"src"`
				Relation string `json:"relation"`
				Dst      string `json:"dst"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = memStore.SaveEntity(body.Src, "concept")
			_, _ = memStore.SaveEntity(body.Dst, "concept")
			err := memStore.SaveRelation(body.Src, body.Relation, body.Dst)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}

		if r.Method == "DELETE" {
			var body struct {
				Src      string `json:"src"`
				Relation string `json:"relation"`
				Dst      string `json:"dst"`
			}
			if r.ContentLength > 0 {
				_ = json.NewDecoder(r.Body).Decode(&body)
			} else {
				body.Src = r.URL.Query().Get("src")
				body.Relation = r.URL.Query().Get("relation")
				body.Dst = r.URL.Query().Get("dst")
			}

			err := memStore.DeleteRelation(body.Src, body.Relation, body.Dst)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/api") {
			http.NotFound(w, r)
			return
		}

		filePath := "web/dist" + path
		if path == "/" {
			filePath = "web/dist/index.html"
		}

		content, err := webAssets.ReadFile(filePath)
		if err != nil {
			content, err = webAssets.ReadFile("web/dist/index.html")
			if err != nil {
				http.Error(w, "Dashboard Front-End not compiled yet. Please run build in web directory.", http.StatusNotFound)
				return
			}
			filePath = "web/dist/index.html"
		}

		if strings.HasSuffix(filePath, ".html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else if strings.HasSuffix(filePath, ".js") {
			w.Header().Set("Content-Type", "application/javascript")
		} else if strings.HasSuffix(filePath, ".css") {
			w.Header().Set("Content-Type", "text/css")
		} else if strings.HasSuffix(filePath, ".png") {
			w.Header().Set("Content-Type", "image/png")
		} else if strings.HasSuffix(filePath, ".svg") {
			w.Header().Set("Content-Type", "image/svg+xml")
		}
		_, _ = w.Write(content)
	})

	log.Printf("[Web控制台] 后台 HTTP 服务器正在启动，监听端口: %s ...", port)
	log.Printf("[Web控制台] 面板访问地址: http://127.0.0.1:%s/", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Printf("[Web控制台] 服务器异常关闭: %v", err)
	}
}
