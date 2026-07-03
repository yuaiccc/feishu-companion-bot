package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
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
) (string, error) {
	fullText, sent, err := streamingCardReply(ctx, llmClient, msgs, fsClient, msg, updateInterval)
	if err == nil {
		return fullText, nil
	}
	if sent {
		// Card already delivered (possibly partial) — don't double-send.
		return fullText, err
	}
	log.Printf("[流式] CardKit 流式失败，回退文本编辑: %v", err)
	return streamingTextReply(ctx, llmClient, msgs, fsClient, msg, updateInterval)
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
	})
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
	})
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
	b.WriteString(" 不要在回复末尾加\"正在输入\"或\"整理中\"这类占位文字。")
	return b.String()
}

func buildChatMessages(prof *profile.Profile, recentMsgs []feishu.Message, memories []string, extraInstructions string, budget *ctxmgr.Budget) []llm.Message {
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

	// recent context (last 10 messages)
	if len(recentMsgs) > 0 {
		var ctxLines []string
		for _, m := range recentMsgs {
			ctxLines = append(ctxLines, fmt.Sprintf("[%s] %s: %s", m.Time, m.Sender, safety.SanitizeForLLM(m.Content)))
		}
		ctxText := budget.Add("recent_messages", strings.Join(ctxLines, "\n"))
		if ctxText != "" {
			msgs = append(msgs, llm.Message{Role: "system", Content: "近期对话：\n" + ctxText})
		}
	}

	if extraInstructions != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: extraInstructions})
	}

	currentMessage := ""
	if len(recentMsgs) > 0 {
		currentMessage = recentMsgs[len(recentMsgs)-1].Content
	}
	safeCurrentMessage := safety.SanitizeForLLM(currentMessage)
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
			if audience == "owner" && m.Visibility == memory.VisPrivate {
				continue
			}
			if audience == "target" && m.Visibility == memory.VisOwnerOnly {
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
)

var (
	flagActions = flag.Bool("actions", false, "Run in GitHub Actions mode (no WebSocket listener)")
)

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
			})
			if err != nil {
				log.Printf("初始化数据库记忆库失败: %v", err)
			} else {
				log.Printf("[记忆] 使用 OceanBase/MySQL: profile=%s include_chat_archive=%v include_media_archive=%v", cfg.ProfileID, cfg.MemoryIncludeChatArchive, cfg.MemoryIncludeMediaArchive)
			}
		} else {
			var embedder memory.Embedder
			if cfg.OllamaModel != "" {
				embedder = memory.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.OllamaModel)
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
3. 如果值得发，text 必须是一句自然中文，必须包含时间和"%s"的活动，不要表格、不要 Markdown、不要链接。
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

	log.Printf("[收到消息] %s: %s", msg.Sender, msg.Content)

	// Add thinking reaction
	var thinkReactID string
	if msg.MessageID != "" {
		thinkReactID, _ = fs.AddReaction(ctx, msg.MessageID, "THINKING")
	}

	// Classify intent
	intent := classifyIntent(msg.Content)

	// Handle intent-based tools first
	switch intent {
	case IntentGitHub:
		handleGitHubIntent(ctx, cfg, fs, msg, thinkReactID, llmClient, prof)
		return
	case IntentHealth:
		handleHealthIntent(ctx, cfg, fs, msg, thinkReactID)
		return
	case IntentMemoryAudit:
		handleMemoryAuditIntent(ctx, cfg, mem, fs, msg, thinkReactID)
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
	recentMsgs, _ := fs.ListMessages(ctx, msg.ChatID, 20)

	// Search relevant memories
	var memories []string
	if mem != nil {
		trace.Span("search_memory")
		audience := audienceForMessage(msg)
		memories = mem.Search(msg.Content, audience)
		if len(memories) > 0 {
			log.Printf("[记忆] 找到 %d 条相关记忆", len(memories))
		}
	}

	// Build prompt and call LLM
	budget := ctxmgr.NewBudget(4000)
	llmMsgs := buildChatMessages(prof, recentMsgs, memories, "", budget)

	trace.Span("deepseek_call")

	var reply string
	var err error
	streamed := false

	if cfg.StreamingReplyEnabled && msg.ChatType != "group" {
		// CardKit streaming reply for private chats
		reply, err = streamingReply(ctx, llmClient, llmMsgs, fs, msg, cfg.StreamingReplyUpdateInterval)
		streamed = true
	} else {
		// Non-streaming for group chats
		reply, err = llmClient.Chat(ctx, llmMsgs, llm.WithTemperature(0.7), llm.WithMaxTokens(500))
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
			fs.ReplyText(ctx, reply, msg.MessageID)
		} else {
			fs.SendText(ctx, reply, msg.ChatID)
		}
	}

	// Cleanup thinking reaction and add content emoji
	cleanupReaction(ctx, fs, msg, thinkReactID)
	if msg.MessageID != "" {
		fs.AddReaction(ctx, msg.MessageID, pickEmoji(msg.Content, msg.IsOwner, prof))
	}

	// Memory confirmation check
	if cfg.MemoryConfirmationEnabled && mem != nil {
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
		fs.ReplyText(ctx, reply, msg.MessageID)
	} else {
		fs.SendText(ctx, reply, msg.ChatID)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	fs.AddReaction(ctx, msg.MessageID, "DONE")
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
}

func handleMemoryAuditIntent(ctx stdctx.Context, cfg *config.Config, mem memory.MemoryStore, fs *feishu.Client, msg feishu.Message, reactID string) {
	audience := "owner"
	if msg.IsOwner {
		audience = "target"
	}
	card := buildMemoryAuditCard(mem, audience)
	if msg.MessageID != "" {
		fs.ReplyCard(ctx, card, msg.MessageID)
	} else {
		fs.SendCard(ctx, card, msg.ChatID)
	}
	cleanupReaction(ctx, fs, msg, reactID)
	fs.AddReaction(ctx, msg.MessageID, "DONE")
}

func handleMediaIntent(ctx stdctx.Context, cfg *config.Config, mem memory.MemoryStore, fs *feishu.Client, msg feishu.Message, reactID string, llmClient *llm.Client, prof *profile.Profile) {
	searcher, ok := mem.(mediaMemoryStore)
	if !ok || !cfg.MemoryIncludeMediaArchive {
		fs.ReplyText(ctx, "图片回忆还没接到记忆库里，等图片索引跑完并打开 MEMORY_INCLUDE_MEDIA_ARCHIVE 后就能找。", msg.MessageID)
		cleanupReaction(ctx, fs, msg, reactID)
		fs.AddReaction(ctx, msg.MessageID, "DONE")
		return
	}

	results := searcher.SearchMedia(msg.Content, audienceForMessage(msg), 3)
	if len(results) == 0 {
		fs.ReplyText(ctx, "我在图片记忆里还没翻到相关内容。可以换个更具体的词，比如地点、截图里的文字、物品或大概时间。", msg.MessageID)
		cleanupReaction(ctx, fs, msg, reactID)
		fs.AddReaction(ctx, msg.MessageID, "DONE")
		return
	}

	reply, pickIdx := summarizeMediaResults(ctx, llmClient, msg.Content, results, prof)
	if reply == "" {
		reply = fallbackMediaSummary(results)
		pickIdx = 0
	}
	if len(results) > 1 {
		reply += fmt.Sprintf("\n（还有 %d 张相关的，要看全部就说一声。）", len(results)-1)
	}
	if msg.MessageID != "" {
		fs.ReplyText(ctx, reply, msg.MessageID)
	} else {
		fs.SendText(ctx, reply, msg.ChatID)
	}

	if cfg.MemoryMediaSendImage {
		// Try the LLM-picked image first; fall back to the others if it's
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
			if msg.MessageID != "" {
				if err := fs.ReplyImage(ctx, result.FilePath, msg.MessageID); err != nil {
					log.Printf("[图片记忆] 发送图片失败: %v", err)
				}
			} else if _, err := fs.SendImage(ctx, result.FilePath, msg.ChatID); err != nil {
				log.Printf("[图片记忆] 发送图片失败: %v", err)
			}
			break
		}
	}

	cleanupReaction(ctx, fs, msg, reactID)
	fs.AddReaction(ctx, msg.MessageID, "DONE")
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
2. summary 直接说翻到了什么、发的是哪张，带上大概时间；不要暴露本地文件路径，不要编造未出现的细节。
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
}

func cleanupReaction(ctx stdctx.Context, fs *feishu.Client, msg feishu.Message, reactID string) {
	if reactID != "" && msg.MessageID != "" {
		fs.DeleteReaction(ctx, msg.MessageID, reactID)
	}
}

func audienceForMessage(msg feishu.Message) string {
	if msg.IsOwner {
		return "owner"
	}
	return "target"
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
