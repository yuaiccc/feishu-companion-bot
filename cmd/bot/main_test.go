package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	stdctx "context"
	"feishu-companion-bot/internal/config"
	ctxmgr "feishu-companion-bot/internal/context"
	"feishu-companion-bot/internal/llm"
	"feishu-companion-bot/internal/memory"
	"feishu-companion-bot/internal/profile"
	"feishu-companion-bot/internal/search"
)

type mockMemoryStore struct {
	items []memory.Memory
}

func (m *mockMemoryStore) All() []memory.Memory {
	return m.items
}

func (m *mockMemoryStore) Add(mem memory.Memory) error {
	m.items = append(m.items, mem)
	return nil
}

func (m *mockMemoryStore) Delete(id string) error {
	for i, item := range m.items {
		if item.ID == id {
			m.items = append(m.items[:i], m.items[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *mockMemoryStore) Search(query string, audience string) []string {
	return nil
}

func (m *mockMemoryStore) SearchRelevant(query string, audience string) []memory.RetrievedMemory {
	var out []memory.RetrievedMemory
	for _, item := range m.items {
		out = append(out, memory.RetrievedMemory{
			ID:         item.ID,
			Text:       item.Content,
			MemoryType: item.MemoryType,
		})
	}
	return out
}

func (m *mockMemoryStore) GetRelationshipState() (memory.RelationshipState, error) {
	return memory.RelationshipState{MoodScore: 80, AffinityScore: 80, LastSentiment: "neutral"}, nil
}

func (m *mockMemoryStore) UpdateRelationshipState(state memory.RelationshipState) error {
	return nil
}

func (m *mockMemoryStore) GetImageHashCache(hash string) (ocr string, caption string, err error) {
	return "", "", fmt.Errorf("not implemented in mock")
}

func (m *mockMemoryStore) SaveImageHashCache(hash string, ocr string, caption string) error {
	return nil
}

func init() {
	dir := "."
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
			os.Chdir(dir)
			break
		}
		dir = filepath.Join(dir, "..")
	}
}

func TestLLMClassifyIntent(t *testing.T) {
	cfg := config.Load()
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("Skipping test: DEEPSEEK_API_KEY not configured in .env")
	}

	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	ctx := context.Background()

	tests := []struct {
		input               string
		isRecentMediaSearch bool
		expected            Intent
	}{
		{"查下 GitHub 最近的 commit", false, IntentGitHub},
		{"帮我自检一下机器人的服务状态", false, IntentHealth},
		{"查看一下我的记忆审计面板", false, IntentMemoryAudit},
		{"上网搜一下2026年流行的AI框架", false, IntentSearch},
		{"撤回刚才发错的消息", false, IntentRecall},
		{"你好啊小弟，今天天气真不错", false, IntentNone},
		{"换一张", true, IntentMedia},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := classifyIntent(ctx, llmClient, tc.input, tc.isRecentMediaSearch)
			if result != tc.expected {
				t.Errorf("For input '%s' (isRecentMediaSearch=%t), expected intent '%s', got '%s'", tc.input, tc.isRecentMediaSearch, tc.expected, result)
			} else {
				t.Logf("Passed: input '%s' -> intent '%s'", tc.input, result)
			}
		})
	}
}

func TestSearchSynthesis(t *testing.T) {
	cfg := config.Load()
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("Skipping test: DEEPSEEK_API_KEY not configured in .env")
	}

	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)

	results := []search.Result{
		{
			Title:   "Gemini 3.5 Flash 发布",
			URL:     "https://google.com/gemini-flash",
			Summary: "Google 2026年发布了 Gemini 3.5 Flash 模型，拥有极高的性价比和推理速度。",
		},
		{
			Title:   "火山引擎接通 DeepSeek V3",
			URL:     "https://volcengine.com/deepseek",
			Summary: "火山引擎方舟平台上线了 DeepSeek V3，大幅降低了企业推理成本并提升了效率。",
		},
	}

	query := "最近有什么最新的大模型和AI消息"
	summary := summarizeSearch(query, results, llmClient)

	t.Logf("Generated search synthesis:\n%s", summary)

	// Verify synthesis quality
	if !strings.Contains(summary, "Gemini") && !strings.Contains(summary, "DeepSeek") {
		t.Errorf("Synthesis failed to capture key entities. Got: %s", summary)
	}

	if !strings.Contains(summary, "[1]") && !strings.Contains(summary, "[2]") {
		t.Errorf("Synthesis failed to include search source references. Got: %s", summary)
	}
}

func TestMemoryConsolidation(t *testing.T) {
	cfg := config.Load()
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("Skipping test: DEEPSEEK_API_KEY not configured in .env")
	}

	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	ctx := context.Background()
	prof := &profile.Profile{OwnerName: "owner"}

	memStore := &mockMemoryStore{
		items: []memory.Memory{
			{
				ID:         "old_mem_1",
				Content:    "用户喜欢喝可乐，经常买可口可乐。",
				MemoryType: memory.MemoryTypeSemantic,
			},
			{
				ID:         "old_mem_2",
				Content:    "用户计划下周一去北京出差开会。",
				MemoryType: memory.MemoryTypeEpisodic,
			},
		},
	}

	// 1. 测试冲突：地点/计划变更
	newContentConflict := "下周一去北京开会的计划取消了，改去上海参展。"
	consolidateMemory(ctx, llmClient, memStore, newContentConflict, "episodic", "owner", prof)

	// Verify that the old memory is either deleted or its content has changed from the original.
	foundOldBeijing := false
	for _, item := range memStore.items {
		if item.ID == "old_mem_2" && item.Content == "用户计划下周一去北京出差开会。" {
			foundOldBeijing = true
		}
	}
	if foundOldBeijing {
		t.Errorf("Conflict resolution failed: 'old_mem_2' (去北京) was not deleted or updated.")
	} else {
		t.Log("Passed: Conflicting old memory '去北京' was successfully removed/consolidated.")
	}

	// 2. 测试冗余：完全一致或子集
	initialCount := len(memStore.items)
	newContentRedundant := "我平时比较爱喝可乐。"
	consolidateMemory(ctx, llmClient, memStore, newContentRedundant, "semantic", "owner", prof)

	// 期待旧的“喜欢喝可乐”由于冗余而被消解（删除或标记）
	foundCoke := false
	for _, item := range memStore.items {
		if strings.Contains(item.Content, "喝可乐") {
			foundCoke = true
		}
	}
	t.Logf("Memory count after redundant consolidation: %d (initial: %d), Coke still exists: %t", len(memStore.items), initialCount, foundCoke)
}

func TestContextAwareMemoryExtraction(t *testing.T) {
	cfg := config.Load()
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("Skipping test: DEEPSEEK_API_KEY not configured in .env")
	}

	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	ctx := context.Background()

	prof := &profile.Profile{
		OwnerName: "owner",
	}

	// 1. Mock recent turns context: 
	// The conversation was discussing plans to go to Beijing next Monday.
	recentTurns := []workingMemoryTurn{
		{
			Sender:  "机器人",
			Content: "老板，您之前提到下周一有出差安排，是去北京开会吗？",
		},
	}

	// The current message is a simple confirmation, which would be rejected 
	// as meaningless without context.
	currentMessage := "对的，就是这个安排。"

	shouldRemember, candidate, memoryType := shouldRememberViaLLM(ctx, currentMessage, true, prof, llmClient, recentTurns)

	t.Logf("Context-aware memory extraction result: remember=%t, candidate='%s', type='%s'", shouldRemember, candidate, memoryType)

	// Verify that LLM was able to extract the fact using the context
	if !shouldRemember {
		t.Errorf("Context-aware memory extraction failed: expected remember=true, got false.")
	}
	if !strings.Contains(candidate, "北京") {
		t.Errorf("Context-aware memory extraction failed: extracted memory '%s' does not contain '北京'.", candidate)
	}
}

func TestSmokeQueryShushuFood(t *testing.T) {
	cfg := config.Load()
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("Skipping smoke query: DEEPSEEK_API_KEY not configured")
	}
	if cfg.MemoryDatabaseDSN == "" {
		t.Skip("Skipping smoke query: MEMORY_DATABASE_DSN not configured")
	}

	dsn := cfg.MemoryDatabaseDSN
	const jdbcPrefix = "jdbc:mysql://"
	if strings.HasPrefix(dsn, jdbcPrefix) {
		cleaned := strings.TrimPrefix(dsn, jdbcPrefix)
		parts := strings.SplitN(cleaned, "/", 2)
		addr := parts[0]
		dbName := ""
		if len(parts) > 1 {
			dbName = parts[1]
		}
		dsn = fmt.Sprintf("root@tcp(%s)/%s?parseTime=true", addr, dbName)
	}

	// 1. Initialize DB Store
	var embedder memory.Embedder
	if cfg.OllamaModel != "" {
		embedder = memory.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.OllamaModel)
	}

	memStore, err := memory.NewDatabaseStore(memory.DatabaseOptions{
		DSN:                dsn,
		ProfileID:          cfg.ProfileID,
		IncludeChatArchive: true,
		ChatVisibility:        memory.Visibility(cfg.MemoryChatVisibility),
		ChatArchiveTable:      cfg.MemoryChatArchiveTable,
		IncludeMediaArchive:   true,
		MediaVisibility:       memory.Visibility(cfg.MemoryMediaVisibility),
		MediaArchiveTable:     cfg.MemoryMediaArchiveTable,
		Embedder:              embedder,
	})
	if err != nil {
		t.Fatalf("Failed to connect to OceanBase: %v", err)
	}

	// 2. Query Memory
	query := "舒舒喜欢吃什么"
	t.Logf("🔍 Smoke query: %q", query)
	results := memStore.SearchRelevant(query, "owner")
	t.Logf("📊 Found %d matching records in OceanBase:", len(results))
	
	var memoryTexts []string
	for idx, item := range results {
		t.Logf("  [%d] (Type: %s, Source: %s): %s", idx+1, item.MemoryType, item.SourceType, item.Text)
		memoryTexts = append(memoryTexts, item.Text)
	}

	// 3. Ask DeepSeek
	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	prof := &profile.Profile{
		OwnerName: "三哥",
		BotName: "小弟",
		Config: map[string]interface{}{
			"persona": "你是三哥的小助手小弟，语气轻松自然，有理有据。你和舒舒（即owner，三哥）在微信有大量的历史聊天记录。当检索出来的记忆标记有 [聊天记录] 时，这代表你们的历史微信会话足迹，你必须把它们和你的飞书对话当作统一连贯的时间线，浑然一体地融入你的回答背景，千万不要说出‘在微信记录里看到’这种机器化套话。",
		},
	}
	
	messages := buildChatMessages(
		prof,
		nil,
		query,
		"三哥",
		"msg_smoke",
		nil,
		memoryTexts,
		"【回复原则】请使用 detailed 风格回复，字数限制 600，做到详尽、充实、有理有据。",
		ctxmgr.NewBudget(64000),
		func(id string) string { return "三哥" },
	)

	t.Log("🧠 Asking DeepSeek...")
	reply, err := llmClient.Chat(stdctx.Background(), messages, llm.WithTemperature(0.3), llm.WithMaxTokens(600))
	if err != nil {
		t.Fatalf("LLM call failed: %v", err)
	}

	t.Logf("\n💬 --- ROBOT REPLY ---\n%s\n----------------------", reply)
}

func TestTemporalMemoryAlignment(t *testing.T) {
	cfg := config.Load()
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("DEEPSEEK_API_KEY not configured")
	}
	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)

	memories := []string{
		"[聊天记录 2023-06-22] 对方: 我觉得鸡爪煲特别好吃，舒舒也很喜欢吃鸡爪煲。",
		"[语义记忆] 舒舒最近在减肥，说以后再也不吃油腻的鸡爪煲了，只吃清蒸沙拉。",
	}

	aligned := alignTemporalMemory(stdctx.Background(), llmClient, memories)
	if len(aligned) == 0 {
		t.Fatalf("Aligned memories should not be empty")
	}

	hasCokeArchive := false
	hasNewFact := false
	for _, m := range aligned {
		if strings.Contains(m, "鸡爪煲特别好吃") {
			hasCokeArchive = true
		}
		if strings.Contains(m, "再也不吃油腻的鸡爪煲") {
			hasNewFact = true
		}
	}

	// Because alignment is run, the outdated WeChat record should be filtered out
	if hasCokeArchive {
		t.Errorf("Temporal memory alignment failed: outdated WeChat record was not filtered out")
	}
	if !hasNewFact {
		t.Errorf("Temporal memory alignment failed: new semantic memory was lost")
	}
}

type mock1024Embedder struct{}

func (e *mock1024Embedder) Embed(text string) ([]float32, error) {
	vec := make([]float32, 1024)
	vec[0] = 0.5
	return vec, nil
}

func TestAsyncEmbeddingAndHashCache(t *testing.T) {
	cfg := config.Load()
	if cfg.MemoryDatabaseDSN == "" {
		t.Skip("MEMORY_DATABASE_DSN not configured in .env, skipping OceanBase integration tests")
	}

	opts := memory.DatabaseOptions{
		DSN:       cfg.MemoryDatabaseDSN,
		ProfileID: "test_async_opt",
		Embedder:  &mock1024Embedder{},
	}
	store, err := memory.NewDatabaseStore(opts)
	if err != nil {
		t.Fatalf("Failed to connect to OceanBase: %v", err)
	}
	defer store.Close()

	// 1. Test image hash cache
	testHash := "img_md5_test_hash_val"
	ocrExpected := "测试图片文字内容"
	captionExpected := "一只可爱的测试猫咪"

	err = store.SaveImageHashCache(testHash, ocrExpected, captionExpected)
	if err != nil {
		t.Fatalf("Failed to save image hash cache: %v", err)
	}

	ocrRet, captionRet, err := store.GetImageHashCache(testHash)
	if err != nil {
		t.Fatalf("Failed to get image hash cache: %v", err)
	}
	if ocrRet != ocrExpected || captionRet != captionExpected {
		t.Errorf("Image hash cache value mismatch: got ocr=%q, cap=%q, want ocr=%q, cap=%q", ocrRet, captionRet, ocrExpected, captionExpected)
	}

	// 2. Test async embedding calculation and eventual consistency
	testMem := memory.Memory{
		ID:         fmt.Sprintf("mem_async_t_%d", time.Now().UnixNano()),
		Content:    "大哥今天很开心，说要带我们去吃火锅。",
		MemoryType: memory.MemoryTypeSemantic,
		Visibility: memory.VisOwnerOnly,
	}

	// Sync write should complete instantly
	err = store.Add(testMem)
	if err != nil {
		t.Fatalf("Failed to Add memory: %v", err)
	}

	// Poll database for up to 2 seconds to check if background embedding worker finished writing the vector
	var hasVector bool
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		var vecRaw []byte
		err = store.GetDBConn().QueryRow(`SELECT embedding FROM bot_memories WHERE id=?`, testMem.ID).Scan(&vecRaw)
		if err == nil && len(vecRaw) > 0 {
			hasVector = true
			break
		}
	}

	if !hasVector {
		t.Errorf("Async embedding worker failed to compute and update vector within 2 seconds")
	}

	// 3. Test relationship metrics tracker
	relState := memory.RelationshipState{
		MoodScore:     95,
		AffinityScore: 98,
		LastSentiment: "happy",
	}
	err = store.UpdateRelationshipState(relState)
	if err != nil {
		t.Fatalf("Failed to update relationship state: %v", err)
	}

	relRet, err := store.GetRelationshipState()
	if err != nil {
		t.Fatalf("Failed to get relationship state: %v", err)
	}
	if relRet.MoodScore != relState.MoodScore || relRet.AffinityScore != relState.AffinityScore || relRet.LastSentiment != relState.LastSentiment {
		t.Errorf("Relationship state mismatch: got %+v, want %+v", relRet, relState)
	}
}


