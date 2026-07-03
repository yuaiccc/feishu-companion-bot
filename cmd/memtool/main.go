package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"

	"feishu-companion-bot/internal/config"
	"feishu-companion-bot/internal/memory"
)

func main() {
	dryRun := flag.Bool("dry-run", true, "预览模式，不实际删除")
	list := flag.Bool("list", false, "列出所有记忆")
	clean := flag.Bool("clean", false, "清洗重复/过时记忆")
	search := flag.String("search", "", "搜索记忆关键词")
	showVis := flag.Bool("show-vis", false, "显示可见性标签")
	migrateJSON := flag.Bool("migrate-json", false, "将当前 profile 的 JSON 记忆迁移到数据库记忆库")
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

	fmt.Println("用法: memtool -list | -search <关键词> | -clean [-dry-run=false] | -migrate-json")
}

func openMemoryStore(cfg *config.Config) (memory.MemoryStore, error) {
	if cfg.MemoryDatabaseDSN != "" {
		return memory.NewDatabaseStore(memory.DatabaseOptions{
			DSN:                cfg.MemoryDatabaseDSN,
			ProfileID:          cfg.ProfileID,
			IncludeChatArchive: cfg.MemoryIncludeChatArchive,
			ChatVisibility:     memory.Visibility(cfg.MemoryChatVisibility),
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
			fmt.Printf("[%s] %s\n    来源: %s | 创建: %d\n\n", vis, m.Content, m.SourceType, m.CreatedAt)
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
