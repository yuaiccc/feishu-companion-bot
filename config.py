"""集中管理所有配置和密钥，从 .env 文件加载。"""
import os
from pathlib import Path
from dotenv import load_dotenv

BASE_DIR = Path(__file__).resolve().parent
load_dotenv(BASE_DIR / ".env")

# ---- Profile ----
PROFILE_ID = os.getenv("PROFILE_ID", "default")

# ---- DeepSeek ----
DEEPSEEK_API_KEY = os.getenv("DEEPSEEK_API_KEY", "")
DEEPSEEK_BASE_URL = os.getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com").rstrip("/")
DEEPSEEK_MODEL = os.getenv("DEEPSEEK_MODEL", "deepseek-chat")

# ---- Feishu Webhook ----
FEISHU_WEBHOOK_URL = os.getenv("FEISHU_WEBHOOK_URL", "")

# ---- Feishu 自建应用 OpenAPI ----
FEISHU_APP_ID = os.getenv("FEISHU_APP_ID", "")
FEISHU_APP_SECRET = os.getenv("FEISHU_APP_SECRET", "")
FEISHU_OPEN_API = "https://open.feishu.cn/open-apis"

# ---- Feishu 消息读取 ----
FEISHU_SHUSHU_OPEN_ID = os.getenv("FEISHU_SHUSHU_OPEN_ID", "")
FEISHU_SANGE_OPEN_ID = os.getenv("FEISHU_SANGE_OPEN_ID", "")
FEISHU_BOT_OPEN_ID = os.getenv("FEISHU_BOT_OPEN_ID", "")
FEISHU_CHAT_ID = os.getenv("FEISHU_CHAT_ID", "")
FEISHU_STATUS_CHAT_ID = os.getenv("FEISHU_STATUS_CHAT_ID", "")
FEISHU_READ_MESSAGES = os.getenv("FEISHU_READ_MESSAGES", "true").lower() in ("true", "1", "yes")
FEISHU_EVENT_MAX_AGE_SECONDS = int(os.getenv("FEISHU_EVENT_MAX_AGE_SECONDS", "600"))

# ---- 记忆模块 ----
MEMORY_ENABLED = os.getenv("MEMORY_ENABLED", "true").lower() in ("true", "1", "yes")
MEMORY_DIR = BASE_DIR / os.getenv("MEMORY_DIR", "memory_data")
MEMORY_EMBEDDING_ENABLED = os.getenv("MEMORY_EMBEDDING_ENABLED", "true").lower() in ("true", "1", "yes")
MEMORY_AGENTIC_RAG_ENABLED = os.getenv("MEMORY_AGENTIC_RAG_ENABLED", "true").lower() in ("true", "1", "yes")
MEMORY_AGENTIC_WRITE_ENABLED = os.getenv("MEMORY_AGENTIC_WRITE_ENABLED", "true").lower() in ("true", "1", "yes")
MEMORY_EMBEDDING_DIM = int(os.getenv("MEMORY_EMBEDDING_DIM", "256"))
MEMORY_RAG_CANDIDATES = int(os.getenv("MEMORY_RAG_CANDIDATES", "12"))
MEMORY_EMBEDDING_PROVIDER = os.getenv("MEMORY_EMBEDDING_PROVIDER", "local_hash").strip().lower()
MEMORY_OLLAMA_BASE_URL = os.getenv("MEMORY_OLLAMA_BASE_URL", "http://127.0.0.1:11434").rstrip("/")
MEMORY_OLLAMA_EMBED_MODEL = os.getenv("MEMORY_OLLAMA_EMBED_MODEL", "qwen3-embedding:0.6b")
MEMORY_OLLAMA_TIMEOUT_SECONDS = float(os.getenv("MEMORY_OLLAMA_TIMEOUT_SECONDS", "5"))

# ---- 外部搜索（本地 DeerFlow / OpenClaw）----
EXTERNAL_SEARCH_ENABLED = os.getenv("EXTERNAL_SEARCH_ENABLED", "true").lower() in ("true", "1", "yes")
EXTERNAL_SEARCH_BACKEND = os.getenv("EXTERNAL_SEARCH_BACKEND", "deerflow").strip().lower()
EXTERNAL_SEARCH_FALLBACK_OPENCLAW = os.getenv("EXTERNAL_SEARCH_FALLBACK_OPENCLAW", "true").lower() in ("true", "1", "yes")
DEERFLOW_BACKEND_DIR = Path(os.getenv("DEERFLOW_BACKEND_DIR", str(Path.home() / "Code/deer-flow/backend"))).expanduser()
DEERFLOW_PYTHON = os.getenv("DEERFLOW_PYTHON", str(DEERFLOW_BACKEND_DIR / ".venv/bin/python"))
DEERFLOW_SEARCH_TIMEOUT_SECONDS = int(os.getenv("DEERFLOW_SEARCH_TIMEOUT_SECONDS", "120"))
DEERFLOW_SEARCH_THREAD_PREFIX = os.getenv("DEERFLOW_SEARCH_THREAD_PREFIX", "feishu-companion-search")
OPENCLAW_CLI = os.getenv("OPENCLAW_CLI", "openclaw")
OPENCLAW_SEARCH_PROVIDER = os.getenv("OPENCLAW_SEARCH_PROVIDER", "").strip()
OPENCLAW_SEARCH_LIMIT = int(os.getenv("OPENCLAW_SEARCH_LIMIT", "5"))
OPENCLAW_SEARCH_TIMEOUT_SECONDS = int(os.getenv("OPENCLAW_SEARCH_TIMEOUT_SECONDS", "45"))

# ---- 旁听辅助（本地长连接）----
PASSIVE_ASSIST_ENABLED = os.getenv("PASSIVE_ASSIST_ENABLED", "true").lower() in ("true", "1", "yes")
PASSIVE_ASSIST_QUIET_SECONDS = int(os.getenv("PASSIVE_ASSIST_QUIET_SECONDS", "75"))
PASSIVE_ASSIST_RECENT_WINDOW_SECONDS = int(os.getenv("PASSIVE_ASSIST_RECENT_WINDOW_SECONDS", "480"))
PASSIVE_ASSIST_TOPIC_COOLDOWN_SECONDS = int(os.getenv("PASSIVE_ASSIST_TOPIC_COOLDOWN_SECONDS", "1800"))
PASSIVE_ASSIST_MAX_PER_HOUR = int(os.getenv("PASSIVE_ASSIST_MAX_PER_HOUR", "2"))

# ---- 主动话题（本地长连接）----
PROACTIVE_TOPIC_ENABLED = os.getenv("PROACTIVE_TOPIC_ENABLED", "true").lower() in ("true", "1", "yes")
PROACTIVE_TOPIC_MAX_PER_DAY = int(os.getenv("PROACTIVE_TOPIC_MAX_PER_DAY", "1"))
PROACTIVE_TOPIC_QUIET_SECONDS = int(os.getenv("PROACTIVE_TOPIC_QUIET_SECONDS", "1800"))
PROACTIVE_TOPIC_CHECK_INTERVAL_SECONDS = int(os.getenv("PROACTIVE_TOPIC_CHECK_INTERVAL_SECONDS", "300"))
PROACTIVE_TOPIC_ACTIVE_START = os.getenv("PROACTIVE_TOPIC_ACTIVE_START", "09:30")
PROACTIVE_TOPIC_ACTIVE_END = os.getenv("PROACTIVE_TOPIC_ACTIVE_END", "23:30")

# ---- 每日恋爱笔记 ----
LOVE_NOTE_ENABLED = os.getenv("LOVE_NOTE_ENABLED", "false").lower() in ("true", "1", "yes")
LOVE_NOTE_WIKI_TOKEN = os.getenv("LOVE_NOTE_WIKI_TOKEN", "")
LOVE_NOTE_DOC_TOKEN = os.getenv("LOVE_NOTE_DOC_TOKEN", "")
LOVE_NOTE_RUN_AT = os.getenv("LOVE_NOTE_RUN_AT", "23:55")

# ---- GitHub ----
GITHUB_USERNAME = os.getenv("GITHUB_USERNAME", "")
GITHUB_TOKEN = os.getenv("GITHUB_TOKEN", "")
# 额外追踪的 private 仓库（Events API 不返回 private 仓库活动）
GITHUB_PRIVATE_REPOS = [
    r.strip() for r in os.getenv("GITHUB_PRIVATE_REPOS", "").split(",") if r.strip()
]

# ---- 运行配置 ----
DRY_RUN = os.getenv("DRY_RUN", "true").lower() in ("true", "1", "yes")
POLL_INTERVAL_SECONDS = int(os.getenv("POLL_INTERVAL_SECONDS", "300"))
STATUS_NOTIFY_COOLDOWN_SECONDS = int(os.getenv("STATUS_NOTIFY_COOLDOWN_SECONDS", "300"))

# ---- 回复体验 ----
STREAMING_REPLY_ENABLED = os.getenv("STREAMING_REPLY_ENABLED", "true").lower() in ("true", "1", "yes")
STREAMING_REPLY_UPDATE_INTERVAL_SECONDS = float(os.getenv("STREAMING_REPLY_UPDATE_INTERVAL_SECONDS", "0.35"))

# ---- LLM 上下文管理 ----
CONTEXT_MAX_CHARS = int(os.getenv("CONTEXT_MAX_CHARS", "6000"))
CONTEXT_CHAT_MAX_CHARS = int(os.getenv("CONTEXT_CHAT_MAX_CHARS", "2400"))
CONTEXT_MEMORY_MAX_CHARS = int(os.getenv("CONTEXT_MEMORY_MAX_CHARS", "1600"))
CONTEXT_CALL_NOTES_MAX_CHARS = int(os.getenv("CONTEXT_CALL_NOTES_MAX_CHARS", "1600"))

# ---- 状态文件 ----
STATE_FILE = BASE_DIR / "state.json"
