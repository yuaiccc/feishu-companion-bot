// Command smoke runs real Feishu API checks against the configured tenant.
// It sends actual messages — run it manually against a test chat, never in CI.
//
//	go run ./cmd/smoke              # all checks
//	go run ./cmd/smoke -mode stream # just CardKit streaming
//	go run ./cmd/smoke -mode group  # just plain text
//	go run ./cmd/smoke -mode github # just GitHub activity push
//	go run ./cmd/smoke -mode image  # send a real image (media archive or -image)
//
// Reads secrets from .env in the repo root.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"feishu-companion-bot/internal/config"
	"feishu-companion-bot/internal/feishu"
	"feishu-companion-bot/internal/github"
	"feishu-companion-bot/internal/memory"
)

func main() {
	mode := flag.String("mode", "all", "check mode: all|stream|group|github|image")
	chat := flag.String("chat", "", "override FeishuChatID (receive_id) for this run")
	imageFlag := flag.String("image", "", "image file path to send (image mode); empty = pick latest from media archive")
	flag.Parse()

	loadDotEnv(".env")
	cfg := config.Load()

	receiveID := cfg.FeishuChatID
	if *chat != "" {
		receiveID = *chat
	}
	if cfg.FeishuAppID == "" || cfg.FeishuAppSecret == "" || receiveID == "" {
		log.Fatal("缺少 FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_CHAT_ID，请检查 .env")
	}

	fmt.Printf("⚠️  smoke 将向真实飞书会话发送消息: %s\n", receiveID)
	fmt.Println("    3 秒后开始，Ctrl+C 取消...")
	time.Sleep(3 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fs := feishu.NewClient(cfg.FeishuAppID, cfg.FeishuAppSecret, cfg.FeishuBotOpenID)

	runAll := *mode == "all"
	ok := true
	if runAll || *mode == "stream" {
		ok = testStream(ctx, fs, receiveID) && ok
	}
	if runAll || *mode == "group" {
		ok = testGroup(ctx, fs, receiveID) && ok
	}
	if runAll || *mode == "github" {
		ok = testGitHub(ctx, cfg, fs, receiveID) && ok
	}
	if *mode == "image" {
		ok = testImage(ctx, cfg, fs, receiveID, *imageFlag) && ok
	}

	fmt.Println()
	if ok {
		fmt.Println("✅ smoke 完成")
	} else {
		fmt.Println("❌ smoke 有失败项，见上方日志")
		os.Exit(1)
	}
}

// testStream exercises the full CardKit streaming flow: create card entity,
// send it, push text incrementally, close streaming.
func testStream(ctx context.Context, fs *feishu.Client, receiveID string) bool {
	fmt.Println("\n=== [stream] 私聊 CardKit 流式 ===")

	cardJSON := feishu.BuildStreamingCardJSON("")
	cardID, err := fs.CreateStreamingCard(ctx, cardJSON)
	if err != nil {
		log.Printf("CreateStreamingCard 失败: %v", err)
		return false
	}
	fmt.Println("  card_id:", cardID)

	msgID, err := fs.SendCardEntity(ctx, cardID, receiveID)
	if err != nil {
		log.Printf("SendCardEntity 失败: %v", err)
		return false
	}
	fmt.Println("  message_id:", msgID)

	// Simulate streaming by pushing the sentence in growing prefixes.
	sentence := "smoke 测试：这是一条走 CardKit 官方流式接口的回复，正在逐段上屏。"
	seq := 0
	for i := 1; i <= len(sentence); i += 6 {
		seq++
		if err := fs.StreamUpdateCardText(ctx, cardID, feishu.StreamingCardElementID, sentence[:i], seq); err != nil {
			log.Printf("StreamUpdateCardText 失败: %v", err)
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
	seq++
	if err := fs.StreamUpdateCardText(ctx, cardID, feishu.StreamingCardElementID, sentence, seq); err != nil {
		log.Printf("最终 StreamUpdateCardText 失败: %v", err)
		return false
	}
	if err := fs.CloseStreamingCard(ctx, cardID, seq+1); err != nil {
		log.Printf("CloseStreamingCard 失败: %v", err)
		return false
	}
	fmt.Println("  流式卡片已关闭，最终文本:", sentence)
	return true
}

// testGroup sends a plain text message to verify the basic send path.
func testGroup(ctx context.Context, fs *feishu.Client, receiveID string) bool {
	fmt.Println("\n=== [group] 普通文本回复 ===")
	text := "smoke 测试：普通文本回复链路正常"
	msgID, err := fs.SendText(ctx, text, receiveID)
	if err != nil {
		log.Printf("SendText 失败: %v", err)
		return false
	}
	fmt.Println("  message_id:", msgID)
	return true
}

// testGitHub fetches recent GitHub events and pushes a one-line summary,
// mirroring the Actions fallback path (without dedup state, intentionally).
func testGitHub(ctx context.Context, cfg *config.Config, fs *feishu.Client, receiveID string) bool {
	fmt.Println("\n=== [github] GitHub 动态一句话推送 ===")
	if cfg.GitHubUsername == "" {
		fmt.Println("  跳过：GH_USERNAME 未配置")
		return true
	}
	gh := github.NewClient(cfg.GitHubUsername, cfg.GitHubToken)
	events, err := gh.FetchEvents(ctx)
	if err != nil {
		log.Printf("FetchEvents 失败: %v", err)
		return false
	}
	github.SortByCreatedAt(events)
	events = github.DedupEvents(events)
	if len(events) == 0 {
		fmt.Println("  无 GitHub 事件可推送")
		return true
	}

	var acts []github.Activity
	for i, e := range events {
		if i >= 3 {
			break
		}
		acts = append(acts, github.ParseActivity(e))
	}
	first := acts[0]
	sentence := fmt.Sprintf("smoke: 最近 %d 条 GitHub 动态，最新是 %s（%s）", len(acts), first.Text, first.Repo)
	fmt.Println("  推送文案:", sentence)
	if _, err := fs.SendText(ctx, sentence, receiveID); err != nil {
		log.Printf("SendText 失败: %v", err)
		return false
	}
	return true
}

// testImage sends a real image to verify the upload + send path used by the
// media-memory feature. Image source: -image flag, else the latest real image
// from the configured media archive.
func testImage(ctx context.Context, cfg *config.Config, fs *feishu.Client, receiveID, imageFlag string) bool {
	fmt.Println("\n=== [image] 真实图片发送 ===")
	path := imageFlag
	if path == "" {
		if cfg.MemoryDatabaseDSN == "" || !cfg.MemoryIncludeMediaArchive {
			fmt.Println("  跳过：未配置 MEMORY_DATABASE_DSN / MEMORY_INCLUDE_MEDIA_ARCHIVE；可用 -image <path> 指定")
			return true
		}
		store, err := memory.NewDatabaseStore(memory.DatabaseOptions{
			DSN:                 cfg.MemoryDatabaseDSN,
			ProfileID:           cfg.ProfileID,
			IncludeMediaArchive: true,
			MediaVisibility:     memory.VisOwnerOnly,
			MediaArchiveTable:   cfg.MemoryMediaArchiveTable,
			MediaOCRColumn:      cfg.MemoryMediaOCRColumn,
			MediaCaptionColumn:  cfg.MemoryMediaCaptionColumn,
			MediaTimeColumn:     cfg.MemoryMediaTimeColumn,
			MediaSenderColumn:   cfg.MemoryMediaSenderColumn,
			MediaFilePathColumn: cfg.MemoryMediaFilePathColumn,
			MediaMsgIDColumn:    cfg.MemoryMediaMsgIDColumn,
			MediaStatusColumn:   cfg.MemoryMediaStatusColumn,
			MediaRoot:           cfg.MemoryMediaRoot,
			MediaVault:          cfg.MemoryMediaVault,
		})
		if err != nil {
			log.Printf("初始化媒体库失败: %v", err)
			return false
		}
		defer func() { _ = store.Close() }()
		results := store.SearchMedia("", "owner", 1)
		if len(results) == 0 || results[0].FilePath == "" {
			fmt.Println("  媒体库无可用图片")
			return false
		}
		path = results[0].FilePath
	}
	fmt.Println("  图片:", path)
	msgID, err := fs.SendImage(ctx, path, receiveID)
	if err != nil {
		log.Printf("SendImage 失败: %v", err)
		return false
	}
	fmt.Println("  message_id:", msgID)
	return true
}

// loadDotEnv populates the environment from a .env file (KEY=VALUE per line)
// so smoke can run with `go run ./cmd/smoke` without sourcing .env first.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)
		if _, set := os.LookupEnv(key); !set {
			if err := os.Setenv(key, val); err != nil {
				log.Printf("设置环境变量 %s 失败: %v", key, err)
			}
		}
	}
}
