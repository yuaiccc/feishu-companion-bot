"""飞书 OpenAPI 客户端：直接调 HTTP API，不再依赖 lark-cli 子进程。
- 读消息、发消息（webhook）、表情回复
- tenant_access_token 自动缓存刷新
"""
import json
import time
import requests

from config import (
    FEISHU_APP_ID, FEISHU_APP_SECRET, FEISHU_OPEN_API,
    FEISHU_CHAT_ID, FEISHU_SHUSHU_OPEN_ID, FEISHU_WEBHOOK_URL, DRY_RUN,
)

# ---- tenant_access_token 缓存 ----
_token_cache = {"token": "", "expires_at": 0}


def _get_tenant_access_token() -> str:
    """获取 tenant_access_token，带缓存（有效期 2 小时，提前 5 分钟刷新）。"""
    if _token_cache["token"] and time.time() < _token_cache["expires_at"]:
        return _token_cache["token"]

    resp = requests.post(
        f"{FEISHU_OPEN_API}/auth/v3/tenant_access_token/internal",
        json={"app_id": FEISHU_APP_ID, "app_secret": FEISHU_APP_SECRET},
        timeout=30,
    )
    resp.raise_for_status()
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"获取 token 失败: {data}")
    _token_cache["token"] = data["tenant_access_token"]
    _token_cache["expires_at"] = time.time() + data["expire"] - 300
    return _token_cache["token"]


def _auth_headers() -> dict:
    return {"Authorization": f"Bearer {_get_tenant_access_token()}"}


# ---- 读消息 ----

def fetch_chat_messages(chat_id: str = "", limit: int = 20) -> list[dict]:
    """读取群里最近的完整对话（三哥 + 舒舒）。
    返回 [{message_id, time, content, sender, is_shushu}]，按时间倒序。
    """
    chat_id = chat_id or FEISHU_CHAT_ID
    if not chat_id:
        print("  [feishu_api] 缺少 chat_id，跳过")
        return []

    try:
        resp = requests.get(
            f"{FEISHU_OPEN_API}/im/v1/messages",
            headers=_auth_headers(),
            params={
                "container_id": chat_id,
                "container_id_type": "chat",
                "sort_type": "ByCreateTimeDesc",
                "page_size": min(limit, 50),
            },
            timeout=30,
        )
        resp.raise_for_status()
        data = resp.json()
        if data.get("code") != 0:
            print(f"  [feishu_api] 读取消息失败: {data.get('msg', '')}")
            return []
    except Exception as e:
        print(f"  [feishu_api] 读取消息异常: {e}")
        return []

    messages = []
    items = data.get("data", {}).get("items", [])

    for item in items:
        sender = item.get("sender", {})
        sender_id = sender.get("id", "")

        msg_type = item.get("msg_type", "")
        content_raw = item.get("body", {}).get("content", "")
        create_time = item.get("create_time", "")

        content = _extract_text(msg_type, content_raw)
        if not content:
            continue

        time_str = _format_time(create_time)
        is_shushu = sender_id == FEISHU_SHUSHU_OPEN_ID

        messages.append({
            "message_id": item.get("message_id", ""),
            "time": time_str,
            "content": content,
            "sender": "舒舒" if is_shushu else "三哥",
            "is_shushu": is_shushu,
        })

    return messages


def fetch_shushu_messages(chat_id: str = "", limit: int = 20) -> list[dict]:
    """只读舒舒的消息（兼容旧接口）。"""
    all_msgs = fetch_chat_messages(chat_id, limit)
    return [m for m in all_msgs if m["is_shushu"]]


# ---- 发消息（通过 webhook 机器人）----

def send_text(text: str) -> bool:
    """通过 webhook 机器人发文本消息。dry_run 时只打印。"""
    if DRY_RUN:
        print("\n  " + "=" * 56)
        print("  DRY RUN - 以下消息通过 webhook 机器人发送（不会真正发送）")
        print("=" * 56)
        print(f"  [机器人回复] {text}")
        print("=" * 56 + "\n")
        return True

    resp = requests.post(
        FEISHU_WEBHOOK_URL,
        json={"msg_type": "text", "content": {"text": text}},
        timeout=30,
    )
    resp.raise_for_status()
    result = resp.json()
    if result.get("code") != 0:
        raise RuntimeError(f"飞书 webhook 返回错误: {result}")
    return True


def send_card(card: dict) -> bool:
    """通过 webhook 机器人发卡片消息。dry_run 时只打印。"""
    if DRY_RUN:
        print("\n  " + "=" * 56)
        print("  DRY RUN - 以下卡片通过 webhook 机器人发送（不会真正发送）")
        print("=" * 56)
        print(json.dumps(card, ensure_ascii=False, indent=2))
        print("=" * 56 + "\n")
        return True

    resp = requests.post(
        FEISHU_WEBHOOK_URL,
        json={"msg_type": "interactive", "card": card["card"]},
        timeout=30,
    )
    resp.raise_for_status()
    result = resp.json()
    if result.get("code") != 0:
        raise RuntimeError(f"飞书 webhook 返回错误: {result}")
    return True


# ---- 表情回复 ----

def react_to_message(message_id: str, emoji_type: str = "HEART") -> bool:
    """给某条消息添加表情回复。dry_run 时只打印。"""
    if DRY_RUN:
        print("\n  " + "=" * 56)
        print("  DRY RUN - 以下操作不会真正执行")
        print("=" * 56)
        print(f"  [表情回复] 消息: {message_id}")
        print(f"  表情: {emoji_type}")
        print("=" * 56 + "\n")
        return True

    resp = requests.post(
        f"{FEISHU_OPEN_API}/im/v1/messages/{message_id}/reactions",
        headers={**_auth_headers(), "Content-Type": "application/json"},
        json={"reaction_type": {"emoji_type": emoji_type}},
        timeout=30,
    )
    resp.raise_for_status()
    data = resp.json()
    return data.get("code") == 0


# ---- 工具函数 ----

def _extract_text(msg_type: str, content_raw: str) -> str:
    if not content_raw:
        return ""
    if msg_type == "text":
        try:
            c = json.loads(content_raw)
            if isinstance(c, dict) and "text" in c:
                return c["text"].strip()
        except (json.JSONDecodeError, TypeError):
            pass
        return content_raw.strip()
    if msg_type == "post":
        try:
            c = json.loads(content_raw)
        except (json.JSONDecodeError, TypeError):
            return ""
        texts = []
        for v in c.values():
            if isinstance(v, dict):
                if v.get("title"):
                    texts.append(v["title"])
                for para in v.get("content", []):
                    if isinstance(para, list):
                        for elem in para:
                            if isinstance(elem, dict) and elem.get("tag") == "text":
                                texts.append(elem.get("text", ""))
        return " ".join(texts).strip()
    if msg_type == "interactive":
        return "[卡片消息]"
    return ""


def _format_time(create_time: str) -> str:
    if not create_time:
        return ""
    try:
        ts = int(create_time)
        # 飞书 API 返回毫秒级时间戳（13位），需要转成秒
        if ts > 1e12:
            ts = ts // 1000
        from datetime import datetime, timezone, timedelta
        shanghai = timezone(timedelta(hours=8))
        dt = datetime.fromtimestamp(ts, tz=shanghai)
        return dt.strftime("%m-%d %H:%M")
    except (ValueError, TypeError):
        return create_time


def format_for_deepseek(messages: list[dict]) -> str:
    """把对话格式化成给 DeepSeek 看的文本。"""
    if not messages:
        return ""
    lines = []
    for m in messages:
        lines.append(f"  [{m['time']}] {m['sender']}说: {m['content']}")
    return "\n".join(lines)
