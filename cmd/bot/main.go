package main

import (
	"bytes"
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
	"feishu-companion-bot/internal/memory"
	"feishu-companion-bot/internal/profile"
	"feishu-companion-bot/internal/safety"
	"feishu-companion-bot/internal/search"
	"feishu-companion-bot/internal/state"
)

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
)

type mediaMemoryStore interface {
	SearchMedia(query string, audience string, limit int) []memory.MediaResult
}

type replyPlan struct {
	ShouldReply bool
	UseMemory   bool
	UseMedia    bool
	ReplyStyle  string
	MaxTokens   int
	Reason      string
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
	if emotion := inferEmotion(msg.Content); emotion != "" {
		st.LastEmotion = emotion
	}
	return st.Clone()
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

var (
	statusKeywords = []string{
		"在干嘛", "在干啥", "干嘛", "干啥", "忙什么", "忙啥",
		"在做什么", "在搞什么", "最近怎么样", "最近忙不忙",
		"最近活动", "最近在", "最近进度", "电脑活动", "窗口",
	}
	githubKeywords = []string{
		"github", "commit", "提交", "代码", "项目进度", "最近提交",
		"仓库", "push", "pr", "issue",
	}
	searchKeywords = []string{
		"搜索", "搜一下", "查一下", "网上", "外部", "最新", "热门",
		"排行", "排行榜", "新闻", "b站", "B站", "bilibili", "新番", "动漫",
	}
	mediaKeywords = []string{
		"图片", "照片", "截图", "那张图", "找图", "发图", "相册",
		"回忆", "我们做了什么", "之前做了什么", "以前做了什么",
		"票据", "地图", "聊天截图",
	}
	healthKeywords = []string{
		"健康检查", "服务状态", "自检", "机器人状态", "状态面板",
	}
	memoryAuditKeywords = []string{
		"记忆审计", "记忆面板", "记忆状态", "记忆检查", "审计记忆",
	}
)

func classifyIntent(content string) Intent {
	if len(content) <= 2 {
		return IntentNone
	}
	lower := strings.ToLower(content)
	for _, kw := range memoryAuditKeywords {
		if strings.Contains(content, kw) {
			return IntentMemoryAudit
		}
	}
	for _, kw := range healthKeywords {
		if strings.Contains(content, kw) || strings.Contains(lower, strings.ToLower(kw)) {
			return IntentHealth
		}
	}
	for _, kw := range githubKeywords {
		if strings.Contains(lower, kw) {
			return IntentGitHub
		}
	}
	for _, kw := range mediaKeywords {
		if strings.Contains(content, kw) || strings.Contains(lower, strings.ToLower(kw)) {
			return IntentMedia
		}
	}
	for _, kw := range searchKeywords {
		if strings.Contains(content, kw) || strings.Contains(lower, strings.ToLower(kw)) {
			if !strings.Contains(content, "最近活动") && !strings.Contains(content, "电脑活动") {
				return IntentSearch
			}
		}
	}
	for _, kw := range statusKeywords {
		if strings.Contains(content, kw) {
			return IntentStatus
		}
	}
	return IntentNone
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

func inferEmotion(content string) string {
	switch {
	case strings.ContainsAny(content, "难过哭委屈烦累崩溃疼痛"):
		return "低落或需要安慰"
	case strings.ContainsAny(content, "生气气死讨厌"):
		return "生气"
	case strings.ContainsAny(content, "开心哈哈嘻喜欢爱亲抱"):
		return "开心或亲近"
	case strings.ContainsAny(content, "困睡晚安"):
		return "困倦"
	default:
		return ""
	}
}

func planReply(ctx stdctx.Context, llmClient *llm.Client, msg feishu.Message, intent Intent, st conversationState, prof *profile.Profile, senderLabel string) replyPlan {
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
	prompt := fmt.Sprintf(`你是飞书陪伴机器人的回复前决策器。只返回 JSON：
{"should_reply":true/false,"use_memory":true/false,"use_media":true/false,"reply_style":"short|normal|comfort|detailed","max_tokens":120,"reason":"极短原因"}

判断规则：
- 群聊中被 @ 时通常要回复；私聊通常要回复。
- 群聊未 @ 时必须由你理解语义后决定是否插话，不能按关键词机械匹配；只有你确实能帮上忙、接住情绪、回答问题、补充图片/记忆时才插话。
- 群聊未 @ 且机器人刚回复过时，要更克制，除非用户明显在接着问你或需要帮助。
- 当前发言人是 %s。不要把当前发言人和被提到的人混淆；不要把当前发言人叫成另一个成员。
- 如果用户只是寒暄、确认、短句，short。
- 如果用户表达低落/生气/累，comfort，并查记忆。
- 如果问题涉及过去、回忆、偏好、关系、图片、截图，查记忆；涉及图片再 use_media。
- 不要为了所有消息都查记忆。

会话状态：
%s

用户消息：%s`, senderLabel, stateText, safety.SanitizeForLLM(msg.Content))
	reply, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: buildSystemPrompt(prof)},
		{Role: "user", Content: prompt},
	}, llm.WithTemperature(0), llm.WithMaxTokens(160))
	if err != nil {
		log.Printf("[回复决策] LLM 失败，使用默认计划: %v", err)
		if passiveGroup {
			plan.ShouldReply = false
			plan.Reason = "passive_llm_error"
		}
		return plan
	}
	var parsed struct {
		ShouldReply bool   `json:"should_reply"`
		UseMemory   bool   `json:"use_memory"`
		UseMedia    bool   `json:"use_media"`
		ReplyStyle  string `json:"reply_style"`
		MaxTokens   int    `json:"max_tokens"`
		Reason      string `json:"reason"`
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
	return replyPlan{
		ShouldReply: parsed.ShouldReply,
		UseMemory:   parsed.UseMemory,
		UseMedia:    parsed.UseMedia,
		ReplyStyle:  parsed.ReplyStyle,
		MaxTokens:   parsed.MaxTokens,
		Reason:      parsed.Reason,
	}
}

func defaultReplyPlan(msg feishu.Message, intent Intent, st conversationState) replyPlan {
	plan := replyPlan{ShouldReply: true, UseMemory: false, UseMedia: false, ReplyStyle: "normal", MaxTokens: 350, Reason: "default"}
	if msg.ChatType == "group" && !msg.IsMentioned {
		plan.ShouldReply = false
		plan.Reason = "passive_wait_llm"
		return plan
	}
	if intent != IntentNone {
		plan.UseMemory = intent == IntentMedia || intent == IntentStatus
		plan.UseMedia = intent == IntentMedia
		plan.MaxTokens = 300
		plan.Reason = "intent"
		return plan
	}
	content := msg.Content
	if len([]rune(content)) <= 8 {
		plan.ReplyStyle = "short"
		plan.MaxTokens = 160
	}
	if inferEmotion(content) != "" || st.LastEmotion == "低落或需要安慰" || st.LastEmotion == "生气" {
		plan.UseMemory = true
		plan.ReplyStyle = "comfort"
		plan.MaxTokens = 320
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
		parts = append(parts, "可以稍微展开，但保持自然，不要写成报告。")
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

func shouldRememberViaLLM(ctx stdctx.Context, content string, fromOwner bool, prof *profile.Profile, llmClient *llm.Client) (bool, string) {
	if llmClient == nil || content == "" || len(content) < 3 || len(content) > 500 {
		return false, ""
	}
	// Label the sender from the profile. fromOwner maps to the owner name;
	// otherwise the target name, or a generic "对方" when no target is set.
	sender := prof.OwnerDisplay()
	if !fromOwner {
		if t := prof.TargetDisplay(); t != "" {
			sender = t
		} else {
			sender = "对方"
		}
	}
	systemPrompt := `你是飞书陪伴机器人的记忆管家。判断一条聊天消息是否值得进入长期记忆候选。
只返回 JSON，不要解释。格式：
{"remember": true/false, "memory": "一句自然中文记忆", "reason": "极短原因"}

判断标准：
- 记住稳定偏好、重要事实、长期习惯、关系边界、称呼方式、明确承诺、重要计划。
- 不记普通寒暄、即时情绪、重复废话、临时闲聊、表情语气、已经过时的细枝末节。
- 不要编造消息里没有的信息。
- memory 要适合给 owner 私聊确认，简短、克制、可长期复用。`

	resp, err := llmClient.Chat(ctx, []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: fmt.Sprintf("发送者：%s\n消息：%s", sender, content)},
	}, llm.WithTemperature(0), llm.WithMaxTokens(160))
	if err != nil {
		log.Printf("[记忆判断] LLM 失败: %v", err)
		return false, ""
	}

	// parse JSON from response
	re := regexp.MustCompile(`\{.*\}`)
	m := re.FindString(resp)
	if m == "" {
		return false, ""
	}
	var result struct {
		Remember bool   `json:"remember"`
		Memory   string `json:"memory"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(m), &result); err != nil {
		return false, ""
	}
	return result.Remember, result.Memory
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
	seq := 0
	streamErr := llmClient.ChatStream(ctx, msgs, func(chunk string) {
		fullText += chunk
		now := time.Now()
		if now.Sub(lastUpdate) >= updateInterval || endsWithPunct(chunk) {
			seq++
			fsClient.StreamUpdateCardText(ctx, cardID, feishu.StreamingCardElementID, fullText, seq)
			lastUpdate = now
		}
	}, llm.WithMaxTokens(maxTokens))
	// Final flush + close streaming regardless of stream error.
	if fullText == "" {
		fullText = emptyReplyPlaceholder
	}
	seq++
	fsClient.StreamUpdateCardText(ctx, cardID, feishu.StreamingCardElementID, fullText, seq)
	fsClient.CloseStreamingCard(ctx, cardID, seq+1)
	if streamErr != nil {
		return fullText, sent, streamErr
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

	err = llmClient.ChatStream(ctx, msgs, func(chunk string) {
		fullText += chunk
		now := time.Now()
		if now.Sub(lastUpdate) >= updateInterval || endsWithPunct(chunk) {
			fsClient.UpdateTextMessage(ctx, initialID, fullText)
			lastUpdate = now
		}
	}, llm.WithMaxTokens(maxTokens))
	if err != nil {
		return fullText, err
	}
	if fullText == "" {
		fullText = emptyReplyPlaceholder
	}
	fsClient.UpdateTextMessage(ctx, initialID, fullText)
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

func buildSystemPrompt(prof *profile.Profile) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("你是%s，%s的小助手。", prof.BotRoleText(), prof.OwnerName))
	if prof.TargetName != "" {
		b.WriteString(fmt.Sprintf(" 和%s关系亲密。", prof.TargetName))
	}
	b.WriteString(" 你不是owner本人，只是小弟/助手。语气轻松自然，克制不腻。")
	b.WriteString(fmt.Sprintf(" 必须分清发言人身份：owner 是%s", prof.OwnerDisplay()))
	if target := prof.TargetDisplay(); target != "" {
		b.WriteString(fmt.Sprintf("，target 是%s", target))
	}
	if prof.BotName != "" {
		b.WriteString(fmt.Sprintf("，机器人是%s。", prof.BotName))
	} else {
		b.WriteString("，机器人是小弟。")
	}
	b.WriteString(" 不要把当前发言人叫成另一个人。")
	if roster := prof.IdentityRoster(); roster != "" {
		b.WriteString(" 已知成员表：\n")
		b.WriteString(roster)
	}
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
	return fs.LabelSender(openID, prof.OwnerDisplay(), prof.BotName, prof.TargetDisplay())
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

func buildChatMessages(prof *profile.Profile, currentContent string, currentSender string, currentMessageID string, recentMsgs []feishu.Message, memories []string, extraInstructions string, budget *ctxmgr.Budget, senderLabel func(string) string) []llm.Message {
	var msgs []llm.Message
	msgs = append(msgs, llm.Message{Role: "system", Content: buildSystemPrompt(prof)})

	if len(memories) > 0 {
		safeMemories := make([]string, 0, len(memories))
		for _, memory := range memories {
			safeMemories = append(safeMemories, safety.SanitizeForLLM(memory))
		}
		memText := budget.Add("memories", strings.Join(safeMemories, "\n"))
		if memText != "" {
			msgs = append(msgs, llm.Message{Role: "system", Content: "相关记忆：\n" + memText})
		}
	}

	// Recent context — the conversation BEFORE the current message, in
	// chronological order (oldest first). The current message is excluded so it
	// isn't duplicated; it's passed separately as the final user message, which
	// is the one the model must reply to. (ListMessages returns newest-first,
	// so we iterate in reverse.)
	if len(recentMsgs) > 0 {
		var ctxLines []string
		for i := len(recentMsgs) - 1; i >= 0; i-- {
			m := recentMsgs[i]
			if m.MessageID == currentMessageID {
				continue
			}
			ctxLines = append(ctxLines, fmt.Sprintf("[%s] %s: %s", m.Time, senderLabel(m.Sender), safety.SanitizeForLLM(m.Content)))
		}
		ctxText := budget.Add("recent_messages", strings.Join(ctxLines, "\n"))
		if ctxText != "" {
			msgs = append(msgs, llm.Message{Role: "system", Content: "对话历史（早→近，不含最新消息）：\n" + ctxText})
		}
	}

	if extraInstructions != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: extraInstructions})
	}

	// The final user message is the latest one — explicitly mark it as the
	// message to reply to, so the model doesn't latch onto an earlier turn.
	safeCurrentMessage := fmt.Sprintf("【这是用户最新发的消息，请针对它回复，不要回复历史里的消息】\n当前发言人：%s\n消息：%s", currentSender, safety.SanitizeForLLM(currentContent))
	budget.Add("current_message", safeCurrentMessage)
	msgs = append(msgs, llm.Message{Role: "user", Content: safeCurrentMessage})

	budget.Log()
	return msgs
}

// ---- Health card ----

func buildHealthCard(cfg *config.Config, llmClient *llm.Client, mem memory.MemoryStore) map[string]interface{} {
	h := health.NewChecker(
		func(ctx stdctx.Context) error {
			// Feishu health: try token refresh
			return nil
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
		cfg.OpenClawCLI,
	)
	return searchClient.Search(stdctx.Background(), query)
}

func summarizeSearch(query string, results []search.Result, llmClient *llm.Client) string {
	return search.Summarize(query, results)
}

// ---- Main entry ----

var (
	passiveAssistant = NewPassiveAssistant()
	streamingContext = make(map[string]map[string]interface{}) // messageID -> context
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

// lastMediaAt tracks, per chat, when the last image search happened so
// follow-ups can be resolved while that search is still the active topic.
var (
	lastMediaAt = make(map[string]time.Time)
	lastMediaMu sync.Mutex
)

const lastMediaTTL = 10 * time.Minute

func noteMediaSearch(chatID string) {
	lastMediaMu.Lock()
	defer lastMediaMu.Unlock()
	lastMediaAt[chatID] = time.Now()
}

func recentMediaSearch(chatID string) bool {
	lastMediaMu.Lock()
	defer lastMediaMu.Unlock()
	t, ok := lastMediaAt[chatID]
	return ok && time.Since(t) <= lastMediaTTL
}

var mediaFollowupWords = []string{"全部", "都看", "都要", "都发", "那张", "另一", "换一张", "再来", "再看", "再发", "别的图", "还有图", "都发来"}

func hasMediaFollowupWord(s string) bool {
	for _, w := range mediaFollowupWords {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// isRecallCommand reports whether the message asks the bot to recall its last
// reply. Kept to short messages to avoid matching "撤回" inside normal sentences.
func isRecallCommand(s string) bool {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 10 {
		return false
	}
	lower := strings.ToLower(s)
	return strings.Contains(s, "撤回") || strings.Contains(s, "发错了") || strings.Contains(s, "收回") || strings.Contains(lower, "recall")
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
		{Role: "system", Content: buildSystemPrompt(prof)},
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

func understandIncomingImage(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, msg feishu.Message) string {
	if msg.ImageKey == "" {
		log.Printf("[图片理解] 图片消息没有 image_key: message_id=%s", msg.MessageID)
		return ""
	}
	image, err := fs.DownloadImage(ctx, msg.ImageKey)
	if err != nil {
		log.Printf("[图片理解] 下载飞书图片失败: %v", err)
		return ""
	}
	var parts []string
	if cfg.FeishuOCREnabled {
		texts, err := fs.RecognizeImageText(ctx, image, cfg.FeishuOCRCooldown)
		if err != nil {
			if feishu.IsRateLimitError(err) {
				log.Printf("[图片理解] 飞书 OCR 触发限流，进入冷却并改用本地视觉模型: %v", err)
			} else {
				log.Printf("[图片理解] 飞书 OCR 失败，改用本地视觉模型: %v", err)
			}
		} else if len(texts) > 0 {
			parts = append(parts, "OCR文字："+strings.Join(texts, " / "))
		}
	}
	if cfg.LocalVisionEnabled && cfg.OllamaVisionModel != "" {
		if desc, err := describeImageWithOllama(ctx, cfg, image); err != nil {
			log.Printf("[图片理解] 本地视觉模型失败: %v", err)
		} else if desc != "" {
			parts = append(parts, "视觉描述："+desc)
		}
	}
	return strings.Join(parts, "\n")
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
	defer resp.Body.Close()
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
				Embedder:              embedder,
			})
			if err != nil {
				log.Printf("初始化数据库记忆库失败: %v", err)
			} else {
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
			}
		}
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
	if err := fsClient.StartListening(ctx, handlers); err != nil {
		log.Fatalf("长连接退出: %v", err)
	}
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
				fs.SendText(ctx, "最近有没有什么想聊的呀？", cfg.FeishuChatID)
				passiveAssistant.MarkTopicSent()
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
		if desc := understandIncomingImage(ctx, cfg, fs, msg); desc != "" {
			msg.Content = strings.TrimSpace(msg.Content + "\n图片理解：" + desc)
			log.Printf("[图片理解] %s", desc)
		}
	}
	if isRecallCommand(msg.Content) {
		if err := fs.RecallLastSent(ctx, msg.ChatID); err != nil {
			log.Printf("[撤回] %v", err)
			fs.ReplyText(ctx, "没找到能撤回的消息呀", msg.MessageID)
		} else {
			fs.ReplyText(ctx, "已撤回～", msg.MessageID)
		}
		return
	}
	st := convoState.UpdateMessage(msg, audience)
	passiveAssistant.OnMessage(msg)

	// Classify explicit intents only for private chats and @-mentions.
	// Passive group messages are judged by DeepSeek in planReply.
	intent := IntentNone
	if msg.ChatType != "group" || msg.IsMentioned {
		intent = classifyIntent(msg.Content)
	}

	// @/private follow-ups after an image search can still route to the media
	// handler, which reads conversation context to resolve the reference.
	if (msg.ChatType != "group" || msg.IsMentioned) && intent == IntentNone && recentMediaSearch(msg.ChatID) && hasMediaFollowupWord(msg.Content) {
		intent = IntentMedia
	}

	label := func(openid string) string {
		return senderLabel(cfg, prof, fs, openid)
	}
	currentSender := label(msg.Sender)
	plan := planReply(ctx, llmClient, msg, intent, *st, prof, currentSender)
	log.Printf("[回复决策] reply=%v memory=%v media=%v style=%s max_tokens=%d reason=%s", plan.ShouldReply, plan.UseMemory, plan.UseMedia, plan.ReplyStyle, plan.MaxTokens, plan.Reason)
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
		handleHealthIntent(ctx, cfg, fs, msg, thinkReactID)
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
	}

	// Read recent messages for context
	trace.Span("read_messages")
	recentMsgs, _ := fs.ListMessages(ctx, msg.ChatID, 100)

	// Search relevant memories
	var memories []string
	if mem != nil && plan.UseMemory {
		trace.Span("search_memory")
		memories = mem.Search(msg.Content, audience)
		if len(memories) > 0 {
			log.Printf("[记忆] audience=%s 找到 %d 条相关记忆", audience, len(memories))
		}
	}

	// Build prompt and call LLM
	budget := ctxmgr.NewBudget(16000)
	llmMsgs := buildChatMessages(prof, msg.Content, currentSender, msg.MessageID, recentMsgs, memories, replyStyleInstruction(plan, *st), budget, label)

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
			fs.SendText(ctx, reply, msg.ChatID)
		}
	}
	convoState.MarkBotReply(msg.ChatID)

	// Cleanup thinking reaction and add content emoji
	cleanupReaction(ctx, fs, msg, thinkReactID)
	if msg.MessageID != "" {
		fs.AddReaction(ctx, msg.MessageID, pickEmoji(msg.Content, msg.IsOwner, prof))
	}

	// Memory confirmation check
	if cfg.MemoryConfirmationEnabled && mem != nil && audience != "other" {
		shouldRemember, candidate := shouldRememberViaLLM(ctx, msg.Content, msg.IsOwner, prof, llmClient)
		if shouldRemember && candidate != "" {
			saveMemoryCandidate(ctx, cfg, fs, mem, candidate, msg.MessageID)
		}
	}
}

func handleGitHubIntent(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, msg feishu.Message, reactID string, llmClient *llm.Client, prof *profile.Profile) {
	gh := github.NewClient(cfg.GitHubUsername, cfg.GitHubToken)
	events, err := gh.FetchEvents(ctx)
	if err != nil {
		log.Printf("获取 GitHub 事件失败: %v", err)
		fs.ReplyText(ctx, "GitHub 数据拉取失败，稍后再试试", msg.MessageID)
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
		fs.SendText(ctx, reply, msg.ChatID)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	fs.AddReaction(ctx, msg.MessageID, "DONE")
	convoState.MarkBotReply(msg.ChatID)
}

func handleHealthIntent(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, msg feishu.Message, reactID string) {
	card := buildHealthCard(cfg, nil, nil)
	if msg.MessageID != "" {
		fs.ReplyCard(ctx, card, msg.MessageID)
	} else {
		fs.SendCard(ctx, card, msg.ChatID)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	fs.AddReaction(ctx, msg.MessageID, "DONE")
	convoState.MarkBotReply(msg.ChatID)
}

func handleMemoryAuditIntent(ctx stdctx.Context, cfg *config.Config, prof *profile.Profile, mem memory.MemoryStore, fs *feishu.Client, msg feishu.Message, reactID string) {
	audience := audienceForSender(cfg, prof, msg)
	card := buildMemoryAuditCard(mem, audience)
	if msg.MessageID != "" {
		fs.ReplyCard(ctx, card, msg.MessageID)
	} else {
		fs.SendCard(ctx, card, msg.ChatID)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	fs.AddReaction(ctx, msg.MessageID, "DONE")
	convoState.MarkBotReply(msg.ChatID)
}

func handleMediaIntent(ctx stdctx.Context, cfg *config.Config, mem memory.MemoryStore, fs *feishu.Client, msg feishu.Message, reactID string, llmClient *llm.Client, prof *profile.Profile) {
	searcher, ok := mem.(mediaMemoryStore)
	if !ok || !cfg.MemoryIncludeMediaArchive {
		fs.ReplyText(ctx, "图片回忆还没接到记忆库里，等图片索引跑完并打开 MEMORY_INCLUDE_MEDIA_ARCHIVE 后就能找。", msg.MessageID)
		cleanupReaction(ctx, fs, msg, reactID)
		fs.AddReaction(ctx, msg.MessageID, "DONE")
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
		fs.ReplyText(ctx, "我没太明白要找哪张图，可以说具体点：地点、物品、截图里的字，或大概时间。", msg.MessageID)
		cleanupReaction(ctx, fs, msg, reactID)
		fs.AddReaction(ctx, msg.MessageID, "DONE")
		return
	}

	limit := 3
	if showAll {
		limit = 8
	}
	results := searcher.SearchMedia(query, audience, limit)
	noteMediaSearch(msg.ChatID)
	convoState.MarkMediaSearch(msg.ChatID, query)
	log.Printf("[图片记忆] 查询=%q 命中=%d", query, len(results))
	if len(results) == 0 {
		fs.ReplyText(ctx, fmt.Sprintf("图片记忆里没翻到跟“%s”相关的内容，换个词试试？", query), msg.MessageID)
		cleanupReaction(ctx, fs, msg, reactID)
		fs.AddReaction(ctx, msg.MessageID, "DONE")
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
	fs.AddReaction(ctx, msg.MessageID, "DONE")
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
	defer f.Close()
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
		{Role: "system", Content: buildSystemPrompt(prof)},
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
		fs.ReplyText(ctx, fmt.Sprintf("小弟这边外部搜索暂时没接通，等%s电脑上的本地搜索服务稳一下再查。", prof.OwnerDisplay()), msg.MessageID)
	} else {
		summary := summarizeSearch(msg.Content, results, llmClient)
		fs.ReplyText(ctx, summary, msg.MessageID)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	fs.AddReaction(ctx, msg.MessageID, "DONE")
	convoState.MarkBotReply(msg.ChatID)
}

func handleStatusIntent(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, msg feishu.Message, reactID string, llmClient *llm.Client, prof *profile.Profile) {
	statusReader := localapps.NewReader()
	status, err := statusReader.GetStatus()
	if err != nil {
		log.Printf("[状态] 获取失败: %v", err)
		fs.ReplyText(ctx, fmt.Sprintf("小弟这边暂时没有获取到%s的详细状态，等配置好了再告诉你～", prof.OwnerDisplay()), msg.MessageID)
	} else {
		statusText := localapps.InterpretStatus(status)
		fs.ReplyText(ctx, statusText, msg.MessageID)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	fs.AddReaction(ctx, msg.MessageID, "DONE")
	convoState.MarkBotReply(msg.ChatID)
}

func cleanupReaction(ctx stdctx.Context, fs *feishu.Client, msg feishu.Message, reactID string) {
	if reactID != "" && msg.MessageID != "" {
		fs.DeleteReaction(ctx, msg.MessageID, reactID)
	}
}

func saveMemoryCandidate(ctx stdctx.Context, cfg *config.Config, fs *feishu.Client, mem memory.MemoryStore, content, replyToMsgID string) {
	elements := []interface{}{
		map[string]interface{}{
			"tag":     "markdown",
			"content": fmt.Sprintf("这条可能值得长期记住：\n\n%s", content),
		},
		map[string]interface{}{
			"tag":   "button",
			"text":  map[string]string{"tag": "plain_text", "content": "记住"},
			"type":  "primary",
			"name":  "memory_confirm_remember",
			"value": map[string]string{"action": "remember_candidate", "content": content},
		},
		map[string]interface{}{
			"tag":   "button",
			"text":  map[string]string{"tag": "plain_text", "content": "不要记"},
			"type":  "danger",
			"name":  "memory_confirm_dismiss",
			"value": map[string]string{"action": "dismiss_candidate", "content": content},
		},
	}

	card := buildCard("候选记忆", "turquoise", elements)

	// Send to owner's private chat via open_id
	if cfg.FeishuOwnerOpenID != "" {
		fs.SendCardToOpenID(ctx, card, cfg.FeishuOwnerOpenID)
	}
}

// ---- Card action handler ----

func onCardAction(ctx stdctx.Context, cfg *config.Config, prof *profile.Profile, mem memory.MemoryStore, llmClient *llm.Client, fs *feishu.Client, action feishu.CardAction) string {
	log.Printf("[卡片回调] action=%s msgID=%s operator=%s", action.Action, action.MessageID, action.OperatorID)

	switch action.Action {
	case "remember_candidate":
		// Content is passed in ActionValue, save now
		if mem != nil && action.ActionValue != nil {
			if content, ok := action.ActionValue["content"].(string); ok && content != "" {
				m := memory.Memory{
					ID:         fmt.Sprintf("cand_%d", time.Now().UnixNano()),
					Content:    content,
					Visibility: memory.VisOwnerOnly,
					SourceType: "llm_candidate",
					CreatedAt:  time.Now().Unix(),
				}
				mem.Add(m)
				return "已写入记忆"
			}
		}
		return "内容为空，未写入"
	case "dismiss_candidate":
		// Just acknowledge, don't save
		return "好，这条不记"
	}
	return "已收到"
}

var _ = io.Discard
var _ = bytes.Buffer{}
