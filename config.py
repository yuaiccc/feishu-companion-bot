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
FEISHU_CHAT_ID = os.getenv("FEISHU_CHAT_ID", "")
FEISHU_READ_MESSAGES = os.getenv("FEISHU_READ_MESSAGES", "true").lower() in ("true", "1", "yes")

# ---- 记忆模块 ----
MEMORY_ENABLED = os.getenv("MEMORY_ENABLED", "true").lower() in ("true", "1", "yes")
MEMORY_DIR = BASE_DIR / os.getenv("MEMORY_DIR", "memory_data")

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

# ---- 状态文件 ----
STATE_FILE = BASE_DIR / "state.json"
