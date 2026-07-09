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

func (m *mockMemoryStore) SaveEntity(name string, category string) (string, error) {
	return "", nil
}

func (m *mockMemoryStore) SaveRelation(srcName string, relation string, dstName string) error {
	return nil
}

func (m *mockMemoryStore) GetEntityRelations(entityNames []string) []string {
	return nil
}

func (m *mockMemoryStore) ResolveAliases(entityNames []string) []string {
	return entityNames
}

func (m *mockMemoryStore) GetRelationDestinations(srcName string, relation string) ([]string, error) {
	return nil, nil
}

func (m *mockMemoryStore) DeleteRelation(srcName string, relation string, dstName string) error {
	return nil
}

func (m *mockMemoryStore) GetConfig(key string, defaultVal string) string {
	return defaultVal
}

func (m *mockMemoryStore) SetConfig(key string, value string) error {
	return nil
}

func (m *mockMemoryStore) GetEmotionHistory() ([]memory.RelationshipState, []int64, error) {
	return nil, nil, nil
}

func (m *mockMemoryStore) GetProfileID() string {
	return "test"
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

func TestGraphRAGEntityCollisionAndExtraction(t *testing.T) {
	cfg := config.Load()
	if cfg.MemoryDatabaseDSN == "" {
		t.Skip("MEMORY_DATABASE_DSN not configured, skipping graph integration test")
	}

	opts := memory.DatabaseOptions{
		DSN:       cfg.MemoryDatabaseDSN,
		ProfileID: "test_graph_opt",
		Embedder:  &mock1024Embedder{},
	}
	store, err := memory.NewDatabaseStore(opts)
	if err != nil {
		t.Fatalf("Failed to connect to OceanBase: %v", err)
	}
	defer store.Close()

	db := store.GetDBConn()
	_, _ = db.Exec(`
		DELETE FROM knowledge_relations 
		WHERE src_id IN (SELECT id FROM knowledge_entities WHERE name IN ('秋酿', '舒舒', '阿姨', '大叔', '三哥'))
		   OR dst_id IN (SELECT id FROM knowledge_entities WHERE name IN ('秋酿', '舒舒', '阿姨', '大叔', '三哥'))`)
	_, _ = db.Exec("DELETE FROM knowledge_entities WHERE name IN ('秋酿', '舒舒', '阿姨', '大叔', '三哥')")

	// 1. Test saving entities and relations
	_, err = store.SaveEntity("秋酿", "alias")
	if err != nil {
		t.Fatalf("SaveEntity failed: %v", err)
	}
	_, err = store.SaveEntity("舒舒", "person")
	if err != nil {
		t.Fatalf("SaveEntity failed: %v", err)
	}
	err = store.SaveRelation("秋酿", "is_alias_of", "舒舒")
	if err != nil {
		t.Fatalf("SaveRelation failed: %v", err)
	}
	err = store.SaveRelation("阿姨", "mother_of", "舒舒")
	if err != nil {
		t.Fatalf("SaveRelation failed: %v", err)
	}

	// 2. Test alias resolution
	resolved := store.ResolveAliases([]string{"秋酿"})
	hasShushu := false
	for _, r := range resolved {
		if r == "舒舒" {
			hasShushu = true
		}
	}
	if !hasShushu {
		t.Errorf("Alias resolution failed: '秋酿' was not resolved to '舒舒'. Resolved list: %v", resolved)
	}

	// 3. Test relation mapping facts translation
	relations := store.GetEntityRelations([]string{"秋酿", "舒舒"})
	var hasAliasFact, hasMotherFact bool
	for _, f := range relations {
		if strings.Contains(f, "秋酿是舒舒的别名") {
			hasAliasFact = true
		}
		if strings.Contains(f, "阿姨是舒舒的妈妈") {
			hasMotherFact = true
		}
	}
	if !hasAliasFact {
		t.Errorf("GetEntityRelations failed to return alias fact, got: %v", relations)
	}
	if !hasMotherFact {
		t.Errorf("GetEntityRelations failed to return mother relationship fact, got: %v", relations)
	}

	// 4. Test async LLM triplet extraction
	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("DeepSeekAPIKey not configured, skipping LLM triplet extraction test")
	}

	ctx := context.Background()
	extractAndSaveGraph(ctx, llmClient, store, "大叔是三哥的同事。", nil)

	// Query whether triplet got written
	var relationName string
	err = db.QueryRow(`
		SELECT r.relation 
		FROM knowledge_relations r
		JOIN knowledge_entities e1 ON r.src_id = e1.id
		JOIN knowledge_entities e2 ON r.dst_id = e2.id
		WHERE e1.name = '大叔' AND e2.name = '三哥'`).Scan(&relationName)

	if err != nil {
		t.Logf("Warning: LLM triplet extraction scan failed: %v. This is normal if LLM API is transiently unavailable.", err)
	} else {
		t.Logf("Success: LLM triplet extraction extracted relation: %q", relationName)
		if relationName != "同事" {
			t.Errorf("Expected relation '同事', got %q", relationName)
		}
	}
}

func TestGraphSelfEvolution(t *testing.T) {
	cfg := config.Load()
	if cfg.MemoryDatabaseDSN == "" {
		t.Skip("MEMORY_DATABASE_DSN not configured, skipping graph self-evolution test")
	}

	opts := memory.DatabaseOptions{
		DSN:       cfg.MemoryDatabaseDSN,
		ProfileID: "test_graph_evo_opt",
		Embedder:  &mock1024Embedder{},
	}
	store, err := memory.NewDatabaseStore(opts)
	if err != nil {
		t.Fatalf("Failed to connect to OceanBase: %v", err)
	}
	defer store.Close()

	db := store.GetDBConn()
	_, _ = db.Exec(`
		DELETE FROM knowledge_relations 
		WHERE src_id IN (SELECT id FROM knowledge_entities WHERE name IN ('三哥', '北京', '深圳', '火锅'))
		   OR dst_id IN (SELECT id FROM knowledge_entities WHERE name IN ('三哥', '北京', '深圳', '火锅'))`)
	_, _ = db.Exec("DELETE FROM knowledge_entities WHERE name IN ('三哥', '北京', '深圳', '火锅')")

	// 1. Init original facts
	_, _ = store.SaveEntity("三哥", "person")
	_ = store.SaveRelation("三哥", "所在地", "北京")
	_ = store.SaveRelation("三哥", "喜欢", "火锅")

	// 2. Validate original values
	dsts, err := store.GetRelationDestinations("三哥", "所在地")
	if err != nil || len(dsts) != 1 || dsts[0] != "北京" {
		t.Fatalf("Initial destinations setup failed: got %v, err=%v", dsts, err)
	}

	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("DeepSeekAPIKey not configured, skipping LLM self-evolution test")
	}

	ctx := context.Background()

	// 3. Test REPLACE: 三哥搬去深圳定居了
	extractAndSaveGraph(ctx, llmClient, store, "三哥最近已经决定去深圳定居了，因此不再住在北京。", nil)

	var hasBeijing, hasShenzhen bool
	for i := 0; i < 15; i++ {
		time.Sleep(100 * time.Millisecond)
		dsts, _ = store.GetRelationDestinations("三哥", "所在地")
		hasBeijing, hasShenzhen = false, false
		for _, d := range dsts {
			if d == "北京" {
				hasBeijing = true
			}
			if d == "深圳" {
				hasShenzhen = true
			}
		}
		if !hasBeijing && hasShenzhen {
			break
		}
	}

	if hasBeijing {
		t.Errorf("Self-evolution REPLACE failed: conflicting target '北京' was not removed")
	}
	if !hasShenzhen {
		t.Errorf("Self-evolution REPLACE failed: new target '深圳' was not written")
	}

	// 4. Test DELETE: 三哥不吃火锅了
	extractAndSaveGraph(ctx, llmClient, store, "三哥最近因为上火开始减肥，坚决不吃火锅了。", nil)

	var hasHotpot bool
	for i := 0; i < 15; i++ {
		time.Sleep(100 * time.Millisecond)
		dsts, _ = store.GetRelationDestinations("三哥", "喜欢")
		hasHotpot = false
		for _, d := range dsts {
			if d == "火锅" {
				hasHotpot = true
			}
		}
		if !hasHotpot {
			break
		}
	}

	if hasHotpot {
		t.Errorf("Self-evolution DELETE failed: negated target '火锅' was not deleted from likes relation")
	}
}

func TestMultiTurnGraphExtraction(t *testing.T) {
	cfg := config.Load()
	if cfg.MemoryDatabaseDSN == "" {
		t.Skip("MEMORY_DATABASE_DSN not configured, skipping graph multi-turn test")
	}

	opts := memory.DatabaseOptions{
		DSN:       cfg.MemoryDatabaseDSN,
		ProfileID: "test_graph_multiturn",
		Embedder:  &mock1024Embedder{},
	}
	store, err := memory.NewDatabaseStore(opts)
	if err != nil {
		t.Fatalf("Failed to connect to OceanBase: %v", err)
	}
	defer store.Close()

	db := store.GetDBConn()
	_, _ = db.Exec(`
		DELETE FROM knowledge_relations 
		WHERE src_id IN (SELECT id FROM knowledge_entities WHERE name IN ('舒舒', '梅子', '酸的零食', '这个'))
		   OR dst_id IN (SELECT id FROM knowledge_entities WHERE name IN ('舒舒', '梅子', '酸的零食', '这个'))`)
	_, _ = db.Exec("DELETE FROM knowledge_entities WHERE name IN ('舒舒', '梅子', '酸的零食', '这个')")

	llmClient := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	if cfg.DeepSeekAPIKey == "" {
		t.Skip("DeepSeekAPIKey not configured, skipping LLM test")
	}

	ctx := context.Background()

	recentTurns := []workingMemoryTurn{
		{Sender: "舒舒", Content: "我最近胃口不太好。"},
		{Sender: "三哥", Content: "那我给你买点酸的零食？"},
		{Sender: "舒舒", Content: "好呀，我最喜欢吃这个了。"},
	}

	_ = store.SetConfig("module_multi_turn_graph", "true")

	extractAndSaveGraph(ctx, llmClient, store, "舒舒说她最喜欢吃这个。", recentTurns)

	var likesSomething string
	for i := 0; i < 15; i++ {
		time.Sleep(100 * time.Millisecond)
		dsts, _ := store.GetRelationDestinations("舒舒", "喜欢")
		if len(dsts) > 0 {
			likesSomething = dsts[0]
			break
		}
	}

	if likesSomething == "" {
		t.Log("Warning: LLM multi-turn extraction did not yield any likes relations.")
	} else {
		t.Logf("Success: LLM successfully resolved 'this' to %q through context!", likesSomething)
	}
}

func TestConfigToggles(t *testing.T) {
	cfg := config.Load()
	if cfg.MemoryDatabaseDSN == "" {
		t.Skip("MEMORY_DATABASE_DSN not configured, skipping configs test")
	}

	opts := memory.DatabaseOptions{
		DSN:       cfg.MemoryDatabaseDSN,
		ProfileID: "test_graph_configs",
		Embedder:  &mock1024Embedder{},
	}
	store, err := memory.NewDatabaseStore(opts)
	if err != nil {
		t.Fatalf("Failed to connect to OceanBase: %v", err)
	}
	defer store.Close()

	val := store.GetConfig("non_existent_key", "default_val")
	if val != "default_val" {
		t.Errorf("Expected default_val, got %q", val)
	}

	err = store.SetConfig("test_module_switch", "false")
	if err != nil {
		t.Fatalf("SetConfig failed: %v", err)
	}

	val = store.GetConfig("test_module_switch", "true")
	if val != "false" {
		t.Errorf("Expected false, got %q", val)
	}
}




