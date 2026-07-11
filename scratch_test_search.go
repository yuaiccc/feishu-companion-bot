package main

import (
	"context"
	"fmt"
	"os"
	"feishu-companion-bot/internal/config"
	"feishu-companion-bot/internal/search"
)

func main() {
	cfg := config.Load()
	fmt.Printf("[测试] 配置状态: Enabled=%v, Backend=%q, GatewayURL=%q, PythonDir=%q\n",
		cfg.ExternalSearchEnabled, cfg.ExternalSearchBackend, cfg.DeerFlowGatewayURL, cfg.DeerFlowBackendDir)

	client := search.NewClient(
		cfg.ExternalSearchBackend,
		cfg.DeerFlowBackendDir,
		cfg.DeerFlowPython,
		cfg.DeerFlowGatewayURL,
		cfg.OpenClawCLI,
	)

	query := "DeepSeek V3"
	fmt.Printf("[测试] 发起检索: %q ...\n", query)
	results, err := client.Search(context.Background(), query)
	if err != nil {
		fmt.Printf("[测试] ❌ 检索失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("[测试] 正常返回，共计 %d 条搜索线索:\n", len(results))
	for i, r := range results {
		fmt.Printf("  [%d] 标题: %q\n      链接: %s\n      摘要: %s\n", i+1, r.Title, r.URL, r.Summary)
	}
}
