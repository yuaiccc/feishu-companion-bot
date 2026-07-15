package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"feishu-companion-bot/internal/config"
	"feishu-companion-bot/internal/memory"
	"feishu-companion-bot/internal/rageval"
)

func main() {
	input := flag.String("input", "eval/sample.jsonl", "JSONL 评测集")
	output := flag.String("output", "", "JSON 报告路径")
	flag.Parse()
	cfg := config.Load()
	if cfg.MemoryDatabaseDSN == "" {
		log.Fatal("检索评测需要配置 MEMORY_DATABASE_DSN")
	}
	cases, err := readCases(*input)
	if err != nil {
		log.Fatal(err)
	}
	embedder := memory.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.OllamaModel)
	store, err := memory.NewDatabaseStore(memory.DatabaseOptions{
		DSN: cfg.MemoryDatabaseDSN, ProfileID: cfg.ProfileID,
		IncludeChatArchive: cfg.MemoryIncludeChatArchive, ChatVisibility: memory.Visibility(cfg.MemoryChatVisibility),
		ChatArchiveTable: cfg.MemoryChatArchiveTable, ChatArchiveTextColumn: cfg.MemoryChatArchiveTextColumn, ChatArchiveTimeColumn: cfg.MemoryChatArchiveTimeColumn,
		IncludeMediaArchive: cfg.MemoryIncludeMediaArchive, MediaVisibility: memory.Visibility(cfg.MemoryMediaVisibility),
		MediaArchiveTable: cfg.MemoryMediaArchiveTable, MediaOCRColumn: cfg.MemoryMediaOCRColumn, MediaCaptionColumn: cfg.MemoryMediaCaptionColumn,
		MediaTimeColumn: cfg.MemoryMediaTimeColumn, MediaSenderColumn: cfg.MemoryMediaSenderColumn, MediaFilePathColumn: cfg.MemoryMediaFilePathColumn,
		MediaMsgIDColumn: cfg.MemoryMediaMsgIDColumn, MediaStatusColumn: cfg.MemoryMediaStatusColumn, MediaRoot: cfg.MemoryMediaRoot, MediaVault: cfg.MemoryMediaVault,
		Embedder: embedder, EmbeddingDimension: cfg.MemoryEmbeddingDimension,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	report := rageval.Run(context.Background(), store, cases)
	data, _ := json.MarshalIndent(report, "", "  ")
	if *output == "" {
		*output = filepath.Join("memory_data", cfg.ProfileID, "audits", "retrieval-eval.json")
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0700); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*output, data, 0600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("cases=%d pass=%.1f%% hit@k=%.1f%% mrr=%.3f source_hit=%.1f%% privacy_violations=%d p50=%dms p95=%dms\n报告：%s\n",
		report.Cases, report.PassRate*100, report.HitRate*100, report.MRR, report.SourceHitRate*100,
		report.PrivacyViolations, report.LatencyP50MS, report.LatencyP95MS, *output)
}

func readCases(path string) ([]rageval.Case, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var cases []rageval.Case
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var item rageval.Case
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, err
		}
		cases = append(cases, item)
	}
	return cases, scanner.Err()
}
