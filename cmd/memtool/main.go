package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"feishu-companion-bot/internal/config"
	"feishu-companion-bot/internal/llm"
	"feishu-companion-bot/internal/memory"
	"feishu-companion-bot/internal/memoryquality"
)

func main() {
	dryRun := flag.Bool("dry-run", true, "预览模式，不实际删除")
	list := flag.Bool("list", false, "列出所有记忆")
	clean := flag.Bool("clean", false, "清洗重复/过时记忆")
	search := flag.String("search", "", "搜索记忆关键词")
	showVis := flag.Bool("show-vis", false, "显示可见性标签")
	migrateJSON := flag.Bool("migrate-json", false, "将当前 profile 的 JSON 记忆迁移到数据库记忆库")
	diagnose := flag.Bool("diagnose", false, "检查数据库版本、向量覆盖率和索引")
	qualityAudit := flag.Bool("quality-audit", false, "使用 DeepSeek 审计记忆质量并生成报告")
	qualityApply := flag.Bool("quality-apply", false, "执行高置信度的审计建议")
	qualityThreshold := flag.Float64("quality-threshold", 0.92, "执行删除/改写的最低置信度")
	flag.Parse()

	cfg := config.Load()
	store, err := openMemoryStore(cfg)
	if err != nil {
		log.Fatalf("打开记忆库失败: %v", err)
	}

	if *migrateJSON {
		if cfg.MemoryDatabaseDSN == "" {
			log.Fatal("需要先配置 MEMORY_DATABASE_DSN 才能迁移到数据库")
		}
		migrateJSONMemories(cfg, store)
		return
	}

	if *diagnose {
		diagnoseDatabase(store)
		return
	}

	if *qualityAudit || *qualityApply {
		runQualityAudit(cfg, store, *qualityApply, *qualityThreshold)
		return
	}

	if *list {
		listMemories(store, *showVis)
		return
	}

	if *search != "" {
		searchMemories(store, *search, *showVis)
		return
	}

	if *clean {
		cleanMemories(store, *dryRun)
		return
	}

	fmt.Println("用法: memtool -list | -search <关键词> | -clean [-dry-run=false] | -migrate-json | -diagnose | -quality-audit [-quality-apply]")
}

func runQualityAudit(cfg *config.Config, store memory.MemoryStore, apply bool, threshold float64) {
	client := llm.NewClient(cfg.DeepSeekAPIKey, cfg.DeepSeekBaseURL, cfg.DeepSeekModel)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	report, err := memoryquality.New(client).Audit(ctx, store.All())
	if err != nil {
		log.Fatalf("记忆质量审计失败: %v", err)
	}
	dir := filepath.Join("memory_data", cfg.ProfileID, "audits")
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Fatal(err)
	}
	path := filepath.Join(dir, "memory-quality-"+time.Now().Format("20060102-150405")+".json")
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("审计 %d 条记忆，报告：%s\n", report.Total, path)
	for _, d := range report.Decisions {
		if d.Action != "keep" {
			fmt.Printf("- %s confidence=%.2f id=%s: %s\n", d.Action, d.Confidence, d.ID, d.Reason)
		}
	}
	if !apply {
		fmt.Println("当前仅生成报告；确认后使用 -quality-apply 执行高置信度建议。")
		return
	}
	stats, err := memoryquality.Apply(store, report, threshold)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("执行完成：保留 %d，删除 %d，改写 %d，跳过 %d\n", stats.Kept, stats.Deleted, stats.Rewritten, stats.Skipped)
}

func diagnoseDatabase(store memory.MemoryStore) {
	dbStore, ok := store.(*memory.DatabaseStore)
	if !ok {
		fmt.Println("当前使用本地 JSON 记忆库，没有 OceanBase/SeekDB 诊断信息")
		return
	}
	diagnostics := dbStore.Diagnostics()
	fmt.Printf("数据库版本: %s\n", diagnostics.ServerVersion)
	if len(diagnostics.DiscoveredVectorTables) > 0 {
		fmt.Printf("发现向量表: %s\n", strings.Join(diagnostics.DiscoveredVectorTables, ", "))
	}
	hasUnavailable := false
	for _, table := range diagnostics.Tables {
		if !table.Exists {
			hasUnavailable = true
			fmt.Printf("\n%s: 不可用（请检查表名或 embedding 列）\n", table.Table)
			continue
		}
		coverage := 0.0
		if table.Rows > 0 {
			coverage = float64(table.EmbeddedRows) * 100 / float64(table.Rows)
		}
		fmt.Printf("\n%s: %d 行，%d 条 embedding（%.1f%%）\n", table.Table, table.Rows, table.EmbeddedRows, coverage)
		if len(table.Indexes) == 0 {
			fmt.Println("  索引: 未读取到")
			continue
		}
		for _, index := range table.Indexes {
			fmt.Printf("  - %s\n", index)
		}
	}
	if hasUnavailable && len(diagnostics.AvailableTables) > 0 {
		fmt.Printf("\n当前库中的表: %s\n", strings.Join(diagnostics.AvailableTables, ", "))
	}
	if diagnostics.MediaPaths.Checked > 0 {
		paths := diagnostics.MediaPaths
		fmt.Printf("\n媒体路径: 检查 %d，有效 %d，缺失 %d，未配置根目录 %d\n", paths.Checked, paths.Valid, paths.Missing, paths.Unresolved)
		if paths.ExampleMissing != "" {
			fmt.Printf("  缺失示例: %s\n", paths.ExampleMissing)
		}
	}
}

func openMemoryStore(cfg *config.Config) (memory.MemoryStore, error) {
	var embedder memory.Embedder
	if cfg.OllamaModel != "" {
		embedder = memory.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.OllamaModel)
	}
	if cfg.MemoryDatabaseDSN != "" {
		return memory.NewDatabaseStore(memory.DatabaseOptions{
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
	}
	return memory.NewStore(cfg.ProfileID, "memory_data")
}

func migrateJSONMemories(cfg *config.Config, target memory.MemoryStore) {
	source, err := memory.NewStore(cfg.ProfileID, "memory_data")
	if err != nil {
		log.Fatalf("打开 JSON 记忆失败: %v", err)
	}
	all := source.All()
	for _, m := range all {
		if err := target.Add(m); err != nil {
			log.Fatalf("迁移失败: %v", err)
		}
	}
	fmt.Printf("已迁移 %d 条 JSON 记忆到数据库\n", len(all))
}

func listMemories(store memory.MemoryStore, showVis bool) {
	all := store.All()
	if len(all) == 0 {
		fmt.Println("暂无记忆")
		return
	}
	fmt.Printf("共 %d 条记忆：\n\n", len(all))
	for _, m := range all {
		if showVis {
			vis := string(m.Visibility)
			fmt.Printf("[%s] %s\n    ID: %s | 来源: %s | 创建: %d\n\n", vis, m.Content, m.ID, m.SourceType, m.CreatedAt)
		} else {
			fmt.Printf("- %s\n", m.Content)
		}
	}
}

func searchMemories(store memory.MemoryStore, query string, showVis bool) {
	results := store.Search(query, "owner")
	fmt.Printf("搜索「%s」，找到 %d 条：\n\n", query, len(results))
	if len(results) > 0 {
		for _, result := range results {
			fmt.Printf("- %s\n", result)
		}
		return
	}

	if showVis {
		fmt.Println("未命中 Search，下面显示本地 All() 的可见性辅助排查：")
		for _, m := range store.All() {
			if strings.Contains(m.Content, query) || strings.Contains(m.Raw, query) {
				fmt.Printf("[%s] %s\n", m.Visibility, m.Content)
			}
		}
	}
}

func cleanMemories(store memory.MemoryStore, dryRun bool) {
	all := store.All()
	fmt.Printf("记忆清洗 (dry-run=%v)\n", dryRun)
	fmt.Printf("清洗前: %d 条\n", len(all))

	// Simple deduplication by hash
	seen := make(map[string]bool)
	var toRemove []string
	for _, m := range all {
		if seen[m.Hash] {
			toRemove = append(toRemove, m.ID)
			fmt.Printf("  重复: %s\n", m.Content[:min(50, len(m.Content))])
		}
		seen[m.Hash] = true
	}

	fmt.Printf("将移除: %d 条\n", len(toRemove))

	if dryRun {
		fmt.Println("dry-run 模式，未实际删除")
		return
	}

	for _, id := range toRemove {
		if err := store.Delete(id); err != nil {
			log.Printf("删除 %s 失败: %v", id, err)
		}
	}
	fmt.Println("清洗完成")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = json.Marshal // compile check
