"""Service health checks for the local Feishu companion bot."""
from __future__ import annotations

import subprocess
import time
from pathlib import Path

import requests

from config import (
    DEEPSEEK_API_KEY,
    DEEPSEEK_BASE_URL,
    DEEPSEEK_MODEL,
    DEERFLOW_BACKEND_DIR,
    EXTERNAL_SEARCH_BACKEND,
    FEISHU_APP_ID,
    FEISHU_APP_SECRET,
    FEISHU_CHAT_ID,
    MEMORY_OLLAMA_BASE_URL,
    MEMORY_OLLAMA_EMBED_MODEL,
    OPENCLAW_SEARCH_TIMEOUT_SECONDS,
)
from external_search import _resolve_deerflow_python, _resolve_openclaw_cli
from local_apps import get_presence_summary
from memory import _load_all


def build_health_card() -> dict:
    rows = _health_rows()
    ok_count = sum(1 for row in rows if row["status"] == "正常")
    title = f"机器人自检（{ok_count}/{len(rows)} 正常）"
    return {
        "msg_type": "interactive",
        "card": {
            "schema": "2.0",
            "config": {"update_multi": True},
            "header": {
                "title": {"tag": "plain_text", "content": title},
                "template": "turquoise" if ok_count == len(rows) else "orange",
            },
            "body": {
                "direction": "vertical",
                "padding": "12px 12px 12px 12px",
                "elements": [
                    {
                        "tag": "table",
                        "columns": [
                            {"data_type": "text", "name": "item", "display_name": "项目", "width": "28%"},
                            {"data_type": "text", "name": "status", "display_name": "状态", "width": "18%"},
                            {"data_type": "text", "name": "detail", "display_name": "说明", "width": "auto"},
                        ],
                        "rows": rows,
                        "row_height": "low",
                        "header_style": {"background_style": "grey", "bold": True, "lines": 1},
                        "page_size": min(len(rows), 20),
                    },
                    {
                        "tag": "markdown",
                        "content": f"检查时间：{time.strftime('%m-%d %H:%M:%S')}",
                    },
                ],
            },
        },
    }


def _health_rows() -> list[dict]:
    rows = []
    rows.append(_config_check())
    rows.append(_deepseek_check())
    rows.append(_ollama_check())
    rows.append(_search_backend_check())
    rows.append(_memory_check())
    rows.append(_local_status_check())
    return rows


def _row(item: str, ok: bool, detail: str) -> dict:
    return {"item": item, "status": "正常" if ok else "异常", "detail": detail[:120]}


def _config_check() -> dict:
    missing = []
    if not FEISHU_APP_ID:
        missing.append("FEISHU_APP_ID")
    if not FEISHU_APP_SECRET:
        missing.append("FEISHU_APP_SECRET")
    if not FEISHU_CHAT_ID:
        missing.append("FEISHU_CHAT_ID")
    return _row("飞书配置", not missing, "已配置" if not missing else "缺少 " + "、".join(missing))


def _deepseek_check() -> dict:
    if not DEEPSEEK_API_KEY:
        return _row("DeepSeek", False, "未配置 API Key")
    try:
        session = requests.Session()
        session.trust_env = False
        resp = session.get(f"{DEEPSEEK_BASE_URL}/v1/models", timeout=6)
        ok = resp.status_code == 200
        return _row("DeepSeek", ok, f"{DEEPSEEK_MODEL} / HTTP {resp.status_code}")
    except Exception as e:
        return _row("DeepSeek", False, str(e))


def _ollama_check() -> dict:
    try:
        session = requests.Session()
        session.trust_env = False
        resp = session.post(
            f"{MEMORY_OLLAMA_BASE_URL}/api/embed",
            json={"model": MEMORY_OLLAMA_EMBED_MODEL, "input": "健康检查"},
            timeout=8,
        )
        resp.raise_for_status()
        data = resp.json()
        embedding = (data.get("embeddings") or [[]])[0]
        return _row("Ollama 向量", bool(embedding), f"{MEMORY_OLLAMA_EMBED_MODEL} / {len(embedding)} 维")
    except Exception as e:
        return _row("Ollama 向量", False, str(e))


def _search_backend_check() -> dict:
    backend = EXTERNAL_SEARCH_BACKEND or "deerflow"
    if backend == "deerflow":
        return _deerflow_check()
    if backend == "openclaw":
        return _openclaw_check()
    if backend == "auto":
        deerflow = _deerflow_check()
        openclaw = _openclaw_check()
        ok = deerflow["status"] == "正常" or openclaw["status"] == "正常"
        detail = f"DeerFlow: {deerflow['detail']}；OpenClaw: {openclaw['detail']}"
        return _row("外部搜索(auto)", ok, detail)
    return _row("外部搜索", False, f"未知后端: {backend}")


def _deerflow_check() -> dict:
    try:
        backend_dir = Path(DEERFLOW_BACKEND_DIR).expanduser()
        python = Path(_resolve_deerflow_python(backend_dir))
        ok = backend_dir.exists() and python.exists()
        detail = f"{backend_dir} / {python}"
        return _row("DeerFlow 搜索", ok, detail)
    except Exception as e:
        return _row("DeerFlow 搜索", False, str(e))


def _openclaw_check() -> dict:
    cli = _resolve_openclaw_cli()
    try:
        proc = subprocess.run(
            [cli, "--version"],
            text=True,
            capture_output=True,
            timeout=min(OPENCLAW_SEARCH_TIMEOUT_SECONDS, 8),
            check=False,
        )
        detail = (proc.stdout or proc.stderr or cli).strip().splitlines()[0] if (proc.stdout or proc.stderr) else cli
        return _row("OpenClaw", proc.returncode == 0, detail)
    except Exception as e:
        return _row("OpenClaw", False, str(e))


def _memory_check() -> dict:
    try:
        items = _load_all()
        models = {}
        for item in items:
            model = item.get("embedding_model", "unknown")
            models[model] = models.get(model, 0) + 1
        model_text = "，".join(f"{name}: {count}" for name, count in sorted(models.items())) or "无"
        return _row("记忆库", True, f"{len(items)} 条；{model_text}")
    except Exception as e:
        return _row("记忆库", False, str(e))


def _local_status_check() -> dict:
    try:
        summary = get_presence_summary()
        return _row("本机状态", bool(summary), summary or "未取得本机状态")
    except Exception as e:
        return _row("本机状态", False, str(e))
