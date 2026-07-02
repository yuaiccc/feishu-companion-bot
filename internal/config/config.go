package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	// Feishu
	FeishuAppID       string
	FeishuAppSecret   string
	FeishuChatID      string
	FeishuBotOpenID   string
	FeishuOwnerOpenID string
	FeishuReadMsg     bool

	// DeepSeek
	DeepSeekAPIKey  string
	DeepSeekBaseURL string
	DeepSeekModel   string

	// Ollama (for embeddings)
	OllamaBaseURL   string
	OllamaModel     string

	// GitHub
	GitHubUsername    string
	GitHubToken       string
	GitHubPrivateRepos []string
	PollInterval      time.Duration

	// Memory
	MemoryEnabled           bool
	MemoryConfirmationEnabled bool

	// Profile
	ProfileID string

	// Behavior
	DryRun                          bool
	StreamingReplyEnabled           bool
	StreamingReplyUpdateInterval    time.Duration
	ProactiveTopicEnabled           bool
	ProactiveTopicCheckInterval     time.Duration
	ProactiveTopicCooldown          time.Duration

	// External search
	ExternalSearchEnabled       bool
	ExternalSearchBackend       string
	ExternalSearchFallbackOC    bool
	DeerFlowBackendDir          string
	DeerFlowPython              string
	OpenClawCLI                 string

	// Local daily job
	LocalDailyJobModule string
	LocalDailyJobRunAt  string

	// Health
	HealthCheckCooldown time.Duration

	// Event
	EventMaxAgeSeconds int
}

func Load() *Config {
	c := &Config{
		FeishuAppID:     getEnv("FEISHU_APP_ID", ""),
		FeishuAppSecret: getEnv("FEISHU_APP_SECRET", ""),
		FeishuChatID:    getEnv("FEISHU_CHAT_ID", ""),
		FeishuBotOpenID:   getEnv("FEISHU_BOT_OPEN_ID", ""),
		FeishuOwnerOpenID: getEnv("FEISHU_OWNER_OPEN_ID", ""),
		FeishuReadMsg:   getEnvBool("FEISHU_READ_MESSAGES", true),

		DeepSeekAPIKey:  getEnv("DEEPSEEK_API_KEY", ""),
		DeepSeekBaseURL: getEnv("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		DeepSeekModel:   getEnv("DEEPSEEK_MODEL", "deepseek-chat"),

		OllamaBaseURL: getEnv("OLLAMA_BASE_URL", "http://localhost:11434"),
		OllamaModel:   getEnv("OLLAMA_MODEL", "nomic-embed-text"),

		GitHubUsername: getEnv("GH_USERNAME", getEnv("GITHUB_USERNAME", "")),
		GitHubToken:    getEnv("GH_TOKEN", getEnv("GITHUB_TOKEN", "")),
		PollInterval:   getEnvDuration("POLL_INTERVAL_SECONDS", 10*time.Minute),

		MemoryEnabled:           getEnvBool("MEMORY_ENABLED", true),
		MemoryConfirmationEnabled: getEnvBool("MEMORY_CONFIRMATION_ENABLED", true),

		ProfileID: getEnv("PROFILE_ID", "default"),

		DryRun:               getEnvBool("DRY_RUN", true),
		StreamingReplyEnabled: getEnvBool("STREAMING_REPLY_ENABLED", true),
		StreamingReplyUpdateInterval: getEnvDuration("STREAMING_REPLY_UPDATE_INTERVAL_SECONDS", 350*time.Millisecond),
		ProactiveTopicEnabled: getEnvBool("PROACTIVE_TOPIC_ENABLED", true),
		ProactiveTopicCheckInterval: getEnvDuration("PROACTIVE_TOPIC_CHECK_INTERVAL_SECONDS", 5*time.Minute),
		ProactiveTopicCooldown: getEnvDuration("PROACTIVE_TOPIC_COOLDOWN_HOURS", 24*time.Hour),

		ExternalSearchEnabled: getEnvBool("EXTERNAL_SEARCH_ENABLED", false),
		ExternalSearchBackend: getEnv("EXTERNAL_SEARCH_BACKEND", "deerflow"),
		ExternalSearchFallbackOC: getEnvBool("EXTERNAL_SEARCH_FALLBACK_OPENCLAW", true),
		DeerFlowBackendDir:   getEnv("DEERFLOW_BACKEND_DIR", ""),
		DeerFlowPython:       getEnv("DEERFLOW_PYTHON", "python"),
		OpenClawCLI:          getEnv("OPENCLAW_CLI", "openclaw"),

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