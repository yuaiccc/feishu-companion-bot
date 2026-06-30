"""集中管理所有配置和密钥，从 .env 文件加载。"""
import os
from pathlib import Path
from dotenv import load_dotenv

BASE_DIR = Path(__file__).resolve().parent
load_dotenv(BASE_DIR / ".env")

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
FEISHU_BOT_OPEN_ID = os.getenv("FEISHU_BOT_OPEN_ID", "")
FEISHU_CHAT_ID = os.getenv("FEISHU_CHAT_ID", "")
FEISHU_STATUS_CHAT_ID = os.getenv("FEISHU_STATUS_CHAT_ID", "")
FEISHU_READ_MESSAGES = os.getenv("FEISHU_READ_MESSAGES", "true").lower() in ("true", "1", "yes")
FEISHU_EVENT_MAX_AGE_SECONDS = int(os.getenv("FEISHU_EVENT_MAX_AGE_SECONDS", "600"))

# ---- 记忆模块 ----
MEMORY_ENABLED = os.getenv("MEMORY_ENABLED", "true").lower() in ("true", "1", "yes")
MEMORY_DIR = BASE_DIR / os.getenv("MEMORY_DIR", "memory_data")

# ---- 外部搜索（本地 OpenClaw）----
EXTERNAL_SEARCH_ENABLED = os.getenv("EXTERNAL_SEARCH_ENABLED", "true").lower() in ("true", "1", "yes")
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

# ---- 状态文件 ----
STATE_FILE = BASE_DIR / "state.json"
