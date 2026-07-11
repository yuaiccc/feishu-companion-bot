package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Feishu
	FeishuAppID        string
	FeishuAppSecret    string
	FeishuChatID       string
	FeishuBotOpenID    string
	FeishuOwnerOpenID  string
	FeishuTargetOpenID string
	FeishuReadMsg      bool

	// DeepSeek
	DeepSeekAPIKey  string
	DeepSeekBaseURL string
	DeepSeekModel   string

	// Ollama (for embeddings)
	OllamaBaseURL     string
	OllamaModel       string
	OllamaVisionModel string

	// Image understanding
	FeishuOCREnabled   bool
	FeishuOCRCooldown  time.Duration
	LocalVisionEnabled bool

	// GitHub
	GitHubUsername     string
	GitHubToken        string
	GitHubPrivateRepos []string
	PollInterval       time.Duration

	// Memory
	MemoryEnabled             bool
	MemoryConfirmationEnabled bool
	MemoryDatabaseDSN         string
	MemoryIncludeChatArchive  bool
	MemoryChatVisibility      string
	// Chat archive source (read-only long-term chat memory). Table/column
	// names are configurable so other deployments can point at their own
	// chat-archive schema without editing code.
	MemoryChatArchiveTable      string
	MemoryChatArchiveTextColumn string
	MemoryChatArchiveTimeColumn string
	MemoryIncludeMediaArchive   bool
	MemoryMediaVisibility       string
	MemoryMediaArchiveTable     string
	MemoryMediaOCRColumn        string
	MemoryMediaCaptionColumn    string
	MemoryMediaTimeColumn       string
	MemoryMediaSenderColumn     string
	MemoryMediaFilePathColumn   string
	MemoryMediaMsgIDColumn      string
	MemoryMediaSendImage        bool

	// Profile
	ProfileID string

	// Behavior
	DryRun                       bool
	StreamingReplyEnabled        bool
	StreamingReplyUpdateInterval time.Duration
	ProactiveTopicEnabled        bool
	ProactiveTopicCheckInterval  time.Duration
	ProactiveTopicCooldown       time.Duration

	// External search
	ExternalSearchEnabled    bool
	ExternalSearchBackend    string
	ExternalSearchFallbackOC bool
	DeerFlowBackendDir       string
	DeerFlowPython           string
	DeerFlowGatewayURL       string
	OpenClawCLI              string

	// Local daily job
	LocalDailyJobModule string
	LocalDailyJobRunAt  string

	// Health
	HealthCheckCooldown time.Duration

	// Event
	EventMaxAgeSeconds int
}

func Load() *Config {
	loadDotEnv(".env")
	c := &Config{
		FeishuAppID:        getEnv("FEISHU_APP_ID", ""),
		FeishuAppSecret:    getEnv("FEISHU_APP_SECRET", ""),
		FeishuChatID:       getEnv("FEISHU_CHAT_ID", ""),
		FeishuBotOpenID:    getEnv("FEISHU_BOT_OPEN_ID", ""),
		FeishuOwnerOpenID:  getEnv("FEISHU_OWNER_OPEN_ID", ""),
		FeishuTargetOpenID: getEnv("FEISHU_TARGET_OPEN_ID", ""),
		FeishuReadMsg:      getEnvBool("FEISHU_READ_MESSAGES", true),

		DeepSeekAPIKey:  getEnv("DEEPSEEK_API_KEY", ""),
		DeepSeekBaseURL: getEnv("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		DeepSeekModel:   getEnv("DEEPSEEK_MODEL", "deepseek-chat"),

		OllamaBaseURL:      getEnv("OLLAMA_BASE_URL", getEnv("MEMORY_OLLAMA_BASE_URL", "http://localhost:11434")),
		OllamaModel:        getEnv("OLLAMA_MODEL", getEnv("MEMORY_OLLAMA_EMBED_MODEL", "nomic-embed-text")),
		OllamaVisionModel:  getEnv("OLLAMA_VISION_MODEL", "qwen2.5vl:3b"),
		FeishuOCREnabled:   getEnvBool("FEISHU_OCR_ENABLED", true),
		FeishuOCRCooldown:  getEnvDuration("FEISHU_OCR_COOLDOWN_SECONDS", 5*time.Minute),
		LocalVisionEnabled: getEnvBool("LOCAL_VISION_ENABLED", true),

		GitHubUsername: getEnv("GH_USERNAME", getEnv("GITHUB_USERNAME", "")),
		GitHubToken:    getEnv("GH_TOKEN", getEnv("GITHUB_TOKEN", "")),
		PollInterval:   getEnvDuration("POLL_INTERVAL_SECONDS", 10*time.Minute),

		MemoryEnabled:             getEnvBool("MEMORY_ENABLED", true),
		MemoryConfirmationEnabled: getEnvBool("MEMORY_CONFIRMATION_ENABLED", true),
		MemoryDatabaseDSN:         normalizeJDBCMySQLDSN(getEnv("MEMORY_DATABASE_DSN", "")),
		MemoryIncludeChatArchive:  getEnvBool("MEMORY_INCLUDE_CHAT_ARCHIVE", false),
		MemoryChatVisibility:      getEnv("MEMORY_CHAT_ARCHIVE_VISIBILITY", "owner_only"),

		MemoryChatArchiveTable:      getEnv("MEMORY_CHAT_ARCHIVE_TABLE", "chat_message_chunks"),
		MemoryChatArchiveTextColumn: getEnv("MEMORY_CHAT_ARCHIVE_TEXT_COLUMN", "chunk_text"),
		MemoryChatArchiveTimeColumn: getEnv("MEMORY_CHAT_ARCHIVE_TIME_COLUMN", "end_time"),
		MemoryIncludeMediaArchive:   getEnvBool("MEMORY_INCLUDE_MEDIA_ARCHIVE", false),
		MemoryMediaVisibility:       getEnv("MEMORY_MEDIA_ARCHIVE_VISIBILITY", "owner_only"),
		MemoryMediaArchiveTable:     getEnv("MEMORY_MEDIA_ARCHIVE_TABLE", "media_assets"),
		MemoryMediaOCRColumn:        getEnv("MEMORY_MEDIA_OCR_COLUMN", "ocr_text"),
		MemoryMediaCaptionColumn:    getEnv("MEMORY_MEDIA_CAPTION_COLUMN", "caption"),
		MemoryMediaTimeColumn:       getEnv("MEMORY_MEDIA_TIME_COLUMN", "sent_at"),
		MemoryMediaSenderColumn:     getEnv("MEMORY_MEDIA_SENDER_COLUMN", "sender"),
		MemoryMediaFilePathColumn:   getEnv("MEMORY_MEDIA_FILE_PATH_COLUMN", "file_path"),
		MemoryMediaMsgIDColumn:      getEnv("MEMORY_MEDIA_MSGID_COLUMN", "msgid"),
		MemoryMediaSendImage:        getEnvBool("MEMORY_MEDIA_SEND_IMAGE", true),

		ProfileID: getEnv("PROFILE_ID", "default"),

		DryRun:                       getEnvBool("DRY_RUN", true),
		StreamingReplyEnabled:        getEnvBool("STREAMING_REPLY_ENABLED", true),
		StreamingReplyUpdateInterval: getEnvDuration("STREAMING_REPLY_UPDATE_INTERVAL_SECONDS", 350*time.Millisecond),
		ProactiveTopicEnabled:        getEnvBool("PROACTIVE_TOPIC_ENABLED", true),
		ProactiveTopicCheckInterval:  getEnvDuration("PROACTIVE_TOPIC_CHECK_INTERVAL_SECONDS", 5*time.Minute),
		ProactiveTopicCooldown:       getEnvDuration("PROACTIVE_TOPIC_COOLDOWN_HOURS", 24*time.Hour),

		ExternalSearchEnabled:    getEnvBool("EXTERNAL_SEARCH_ENABLED", false),
		ExternalSearchBackend:    getEnv("EXTERNAL_SEARCH_BACKEND", "deerflow"),
		ExternalSearchFallbackOC: getEnvBool("EXTERNAL_SEARCH_FALLBACK_OPENCLAW", true),
		DeerFlowBackendDir:       getEnv("DEERFLOW_BACKEND_DIR", ""),
		DeerFlowPython:           getEnv("DEERFLOW_PYTHON", "python"),
		DeerFlowGatewayURL:       getEnv("DEERFLOW_GATEWAY_URL", ""),
		OpenClawCLI:              getEnv("OPENCLAW_CLI", "openclaw"),

		LocalDailyJobModule: getEnv("LOCAL_DAILY_JOB_MODULE", ""),
		LocalDailyJobRunAt:  getEnv("LOCAL_DAILY_JOB_RUN_AT", "23:55"),

		HealthCheckCooldown: getEnvDuration("STATUS_NOTIFY_COOLDOWN_SECONDS", 5*time.Minute),

		EventMaxAgeSeconds: getEnvInt("FEISHU_EVENT_MAX_AGE_SECONDS", 300),
	}

	if repos := getEnv("GH_PRIVATE_REPOS", getEnv("GITHUB_PRIVATE_REPOS", "")); repos != "" {
		c.GitHubPrivateRepos = splitEnv(repos, ",")
	}

	return c
}

func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, value)
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return defaultVal
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return time.Duration(i) * time.Second
		}
	}
	return defaultVal
}

func splitEnv(v, sep string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range splitString(v, sep) {
		part = trimString(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitString(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s)-len(sep); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	out = append(out, s[start:])
	return out
}

func trimString(s string) string {
	i, j := 0, len(s)-1
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	for j >= i && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
		j--
	}
	return s[i : j+1]
}

func normalizeJDBCMySQLDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	const jdbcPrefix = "jdbc:mysql://"
	if strings.HasPrefix(dsn, jdbcPrefix) {
		return mysqlURLToGoDSN("mysql://" + strings.TrimPrefix(dsn, jdbcPrefix))
	}
	if strings.HasPrefix(dsn, "mysql://") {
		return mysqlURLToGoDSN(dsn)
	}
	return dsn
}

func mysqlURLToGoDSN(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	user := "root"
	if u.User != nil {
		user = u.User.Username()
		if password, ok := u.User.Password(); ok {
			user = fmt.Sprintf("%s:%s", user, password)
		}
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	query := u.RawQuery
	if query == "" {
		query = "parseTime=true&charset=utf8mb4"
	}
	return fmt.Sprintf("%s@tcp(%s)/%s?%s", user, u.Host, dbName, query)
}
