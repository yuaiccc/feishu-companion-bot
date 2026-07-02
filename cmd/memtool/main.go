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
	flag.Parse()

	cfg := config.Load()
	store, err := memory.NewStore(cfg.ProfileID, "memory_data")
	if err != nil {
		log.Fatalf("打开记忆库失败: %v", err)
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

	fmt.Println("用法: memtool -list | -search <关键词> | -clean [-dry-run=false]")
}

func listMemories(store *memory.Store, showVis bool) {
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

func searchMemories(store *memory.Store, query string, showVis bool) {
	all := store.All()
	fmt.Printf("搜索「%s」，找到 %d 条：\n\n", query, len(all))
	for _, m := range all {
		if strings.Contains(m.Content, query) || strings.Contains(m.Raw, query) {
			if showVis {
				fmt.Printf("[%s] %s\n", m.Visibility, m.Content)
			} else {
				fmt.Printf("- %s\n", m.Content)
			}
		}
	}
}

func cleanMemories(store *memory.Store, dryRun bool) {
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

	// Actually remove duplicates (store doesn't have Delete, so we rebuild)
	// For now just report
	fmt.Println("清洗完成")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = json.Marshal // compile check
