package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"feishu-companion-bot/internal/config"
	"feishu-companion-bot/internal/memory"
)

func main() {
	sources := flag.String("sources", "", "用于恢复旧媒体的目录，多个目录用逗号分隔")
	apply := flag.Bool("apply", false, "实际复制并写入数据库；默认只预览")
	flag.Parse()

	cfg := config.Load()
	if cfg.MemoryDatabaseDSN == "" {
		log.Fatal("需要配置 MEMORY_DATABASE_DSN")
	}
	var embedder memory.Embedder
	if cfg.OllamaModel != "" {
		embedder = memory.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.OllamaModel)
	}
	store, err := memory.NewDatabaseStore(memory.DatabaseOptions{
		DSN: cfg.MemoryDatabaseDSN, ProfileID: cfg.ProfileID,
		IncludeMediaArchive: cfg.MemoryIncludeMediaArchive,
		MediaVisibility:     memory.Visibility(cfg.MemoryMediaVisibility),
		MediaArchiveTable:   cfg.MemoryMediaArchiveTable, MediaOCRColumn: cfg.MemoryMediaOCRColumn,
		MediaCaptionColumn: cfg.MemoryMediaCaptionColumn, MediaTimeColumn: cfg.MemoryMediaTimeColumn,
		MediaSenderColumn: cfg.MemoryMediaSenderColumn, MediaFilePathColumn: cfg.MemoryMediaFilePathColumn,
		MediaMsgIDColumn: cfg.MemoryMediaMsgIDColumn, MediaStatusColumn: cfg.MemoryMediaStatusColumn,
		MediaRoot: cfg.MemoryMediaRoot, MediaVault: cfg.MemoryMediaVault,
		Embedder: embedder, EmbeddingDimension: cfg.MemoryEmbeddingDimension,
	})
	if err != nil {
		log.Fatalf("打开媒体库失败: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("关闭媒体库失败: %v", err)
		}
	}()

	roots := splitNonEmpty(*sources)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()
	started := time.Now()
	stats, err := store.ReingestLegacyMedia(ctx, roots, *apply, func(s memory.MediaReingestStats) {
		fmt.Printf("\r处理 %d 条 | 匹配 %d | 入库 %d | 已存在 %d | 缺失 %d | 歧义 %d | 错误 %d | %.1fs",
			s.Rows, s.Matched, s.Imported, s.AlreadyOK, s.Missing, s.Ambiguous, s.Errors, time.Since(started).Seconds())
	})
	fmt.Println()
	if err != nil {
		log.Fatal(err)
	}
	mode := "预览"
	if *apply {
		mode = "实际入库"
	}
	fmt.Printf("%s完成：%+v\n", mode, stats)
}

func splitNonEmpty(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
