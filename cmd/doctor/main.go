package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"feishu-companion-bot/internal/config"
	"feishu-companion-bot/internal/feishu"
	"feishu-companion-bot/internal/memory"
	"feishu-companion-bot/internal/notes"
	localocr "feishu-companion-bot/internal/ocr"
)

type check struct {
	name     string
	critical bool
	err      error
	detail   string
}

func main() {
	online := flag.Bool("online", false, "同时验证飞书鉴权和群访问")
	flag.Parse()
	cfg := config.Load()
	checks := []check{
		{name: "运行环境", detail: runtime.GOOS + "/" + runtime.GOARCH + " go=" + runtime.Version()},
		{name: "飞书凭证", critical: true, err: require(cfg.FeishuAppID != "" && cfg.FeishuAppSecret != "", "FEISHU_APP_ID/SECRET 未配置")},
		{name: "DeepSeek", critical: true, err: require(cfg.DeepSeekAPIKey != "", "DEEPSEEK_API_KEY 未配置"), detail: cfg.DeepSeekModel},
	}
	engine := localocr.NewAppleVision(cfg.AppleVisionOCRPath, cfg.LocalOCRTimeout)
	checks = append(checks, check{name: "Apple Vision OCR", err: engine.Available(), detail: cfg.AppleVisionOCRPath})

	started := time.Now()
	vec, embedErr := memory.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.OllamaModel).Embed("飞书陪伴机器人健康检查")
	checks = append(checks, check{name: "Ollama embedding", critical: cfg.MemoryDatabaseDSN != "", err: embedErr, detail: fmt.Sprintf("model=%s dim=%d latency=%s", cfg.OllamaModel, len(vec), time.Since(started).Round(time.Millisecond))})

	if cfg.MemoryDatabaseDSN != "" {
		store, err := memory.NewDatabaseStore(memory.DatabaseOptions{
			DSN: cfg.MemoryDatabaseDSN, ProfileID: cfg.ProfileID,
			IncludeChatArchive: cfg.MemoryIncludeChatArchive, ChatVisibility: memory.Visibility(cfg.MemoryChatVisibility),
			ChatArchiveTable: cfg.MemoryChatArchiveTable, ChatArchiveTextColumn: cfg.MemoryChatArchiveTextColumn, ChatArchiveTimeColumn: cfg.MemoryChatArchiveTimeColumn,
			IncludeMediaArchive: cfg.MemoryIncludeMediaArchive, MediaVisibility: memory.Visibility(cfg.MemoryMediaVisibility),
			MediaArchiveTable: cfg.MemoryMediaArchiveTable, MediaOCRColumn: cfg.MemoryMediaOCRColumn, MediaCaptionColumn: cfg.MemoryMediaCaptionColumn,
			MediaTimeColumn: cfg.MemoryMediaTimeColumn, MediaSenderColumn: cfg.MemoryMediaSenderColumn, MediaFilePathColumn: cfg.MemoryMediaFilePathColumn,
			MediaMsgIDColumn: cfg.MemoryMediaMsgIDColumn, MediaStatusColumn: cfg.MemoryMediaStatusColumn,
			MediaRoot: cfg.MemoryMediaRoot, MediaVault: cfg.MemoryMediaVault,
			Embedder: memory.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.OllamaModel), EmbeddingDimension: cfg.MemoryEmbeddingDimension,
		})
		detail := ""
		if err == nil {
			d := store.Diagnostics()
			parts := make([]string, 0, len(d.Tables))
			for _, table := range d.Tables {
				if table.Exists {
					parts = append(parts, fmt.Sprintf("%s=%d/%d", table.Table, table.EmbeddedRows, table.Rows))
				}
			}
			detail = strings.Join(parts, " ")
			store.Close()
		}
		checks = append(checks, check{name: "OceanBase 记忆库", critical: true, err: err, detail: detail})
	} else {
		checks = append(checks, check{name: "OceanBase 记忆库", err: fmt.Errorf("MEMORY_DATABASE_DSN 未配置，将使用 JSON")})
	}

	if *online {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		client := feishu.NewClient(cfg.FeishuAppID, cfg.FeishuAppSecret, cfg.FeishuBotOpenID)
		err := client.HealthCheck(ctx, cfg.FeishuChatID)
		checks = append(checks, check{name: "飞书 OpenAPI", critical: true, err: err, detail: "真实鉴权与群访问"})
		if cfg.LoveNoteEnabled && (cfg.LoveNoteDocToken != "" || cfg.LoveNoteWikiToken != "") {
			noteClient := notes.NewClient(cfg.FeishuAppID, cfg.FeishuAppSecret)
			docToken := cfg.LoveNoteDocToken
			var noteErr error
			if docToken == "" {
				docToken, noteErr = noteClient.ResolveWikiDocToken(ctx, cfg.LoveNoteWikiToken)
			}
			blocks := 0
			if noteErr == nil {
				var items []notes.Block
				items, noteErr = noteClient.GetBlocks(ctx, docToken)
				blocks = len(items)
			}
			checks = append(checks, check{name: "恋爱笔记只读链路", err: noteErr, detail: fmt.Sprintf("blocks=%d", blocks)})
		}
	}

	failed := false
	for _, item := range checks {
		status := "OK"
		if item.err != nil {
			status = "WARN"
			if item.critical {
				status = "FAIL"
				failed = true
			}
		}
		fmt.Printf("%-5s %-22s %s", status, item.name, item.detail)
		if item.err != nil {
			fmt.Printf(" (%v)", item.err)
		}
		fmt.Println()
	}
	if failed {
		os.Exit(1)
	}
}

func require(ok bool, message string) error {
	if !ok {
		return fmt.Errorf("%s", message)
	}
	return nil
}
