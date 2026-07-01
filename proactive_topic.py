"""Proactive topic planner that starts one quiet-time group conversation per day."""
from __future__ import annotations

from datetime import datetime, timezone, timedelta

import requests

from config import (
    DEEPSEEK_API_KEY,
    DEEPSEEK_BASE_URL,
    DEEPSEEK_MODEL,
    FEISHU_CHAT_ID,
    FEISHU_SANGE_OPEN_ID,
    FEISHU_SHUSHU_OPEN_ID,
    PROACTIVE_TOPIC_ACTIVE_END,
    PROACTIVE_TOPIC_ACTIVE_START,
    PROACTIVE_TOPIC_ENABLED,
    PROACTIVE_TOPIC_MAX_PER_DAY,
    PROACTIVE_TOPIC_QUIET_SECONDS,
)
from feishu_api import fetch_chat_messages, send_text
from local_apps import get_local_status_summary
from memory import search_memories
from profile import owner_name, target_name
from state import can_send_proactive_today, load_state, mark_proactive_sent
from text_safety import sanitize_public_text

_SHANGHAI = timezone(timedelta(hours=8))


def maybe_send_proactive_topic(now: datetime | None = None) -> str:
    """Send one proactive topic if the group has been quiet long enough."""
    if not PROACTIVE_TOPIC_ENABLED:
        return "disabled"
    now = now or datetime.now(_SHANGHAI)
    if now.tzinfo is None:
        now = now.replace(tzinfo=_SHANGHAI)
    if not _in_active_window(now):
        return "outside_active_window"

    date_key = now.strftime("%Y-%m-%d")
    state = load_state()
    if not can_send_proactive_today(state, date_key, PROACTIVE_TOPIC_MAX_PER_DAY):
        return "daily_limit"

    messages = fetch_chat_messages(FEISHU_CHAT_ID, limit=12)
    if not messages:
        return "no_messages"
    if not _is_group_quiet(messages, now):
        return "not_quiet"

    topic = _generate_topic(messages)
    if not topic:
        return "no_topic"
    text = _with_mentions(topic)
    send_text(text, receive_id=FEISHU_CHAT_ID)
    state = load_state()
    mark_proactive_sent(state, date_key, now.timestamp())
    return "sent"


def _is_group_quiet(messages: list[dict], now: datetime | None = None) -> bool:
    if not messages:
        return True
    now = now or datetime.now(_SHANGHAI)
    if now.tzinfo is None:
        now = now.replace(tzinfo=_SHANGHAI)
    latest_ts = max(int(m.get("timestamp", 0) or 0) for m in messages)
    if latest_ts <= 0:
        return True
    # Feishu create_time is milliseconds.
    latest_seconds = latest_ts / 1000 if latest_ts > 10_000_000_000 else latest_ts
    return now.timestamp() - latest_seconds >= PROACTIVE_TOPIC_QUIET_SECONDS


def _in_active_window(now: datetime) -> bool:
    current = now.strftime("%H:%M")
    start = PROACTIVE_TOPIC_ACTIVE_START
    end = PROACTIVE_TOPIC_ACTIVE_END
    if start <= end:
        return start <= current <= end
    return current >= start or current <= end


def _generate_topic(messages: list[dict]) -> str:
    memories = search_memories("最近适合三哥和舒舒聊天的话题", audience="target", top_k=4)
    local_status = ""
    try:
        local_status = get_local_status_summary()
    except Exception:
        local_status = ""

    if not DEEPSEEK_API_KEY:
        return "刚刚群里安静了一会儿，小弟来冒个泡：你们俩今天有没有什么想一起看的、想一起聊的小事呀？"

    recent = "\n".join(
        f"{m.get('sender', '')}: {m.get('content', '')}"
        for m in reversed(messages[-8:])
    )
    prompt = """你是三哥的小弟，不是三哥本人。你要在群聊冷场后主动抛一个轻量话题，让三哥和舒舒自然聊起来。

要求：
- 只写 1-2 句话，像群里自然开场
- 不要总结一大段，不要列表
- 不要冒充三哥，不要说“我想你”
- 可以说“小弟来开个话题”
- 重点是让两个人都有话接
- 不要总提写代码、做项目、GitHub
- 群里称呼女生时在“舒舒”和“烨子”里选一个，不要把两个名字并列说
- 不要出现“微里”
- 不要暴露私密地址、token、手机号等隐私"""
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": prompt},
            {
                "role": "user",
                "content": (
                    f"最近群聊：\n{recent or '暂无'}\n\n"
                    f"可用公开记忆：\n" + "\n".join(f"- {m}" for m in memories) + "\n\n"
                    f"三哥电脑状态线索：{local_status or '暂无'}"
                ),
            },
        ],
        "temperature": 0.8,
        "max_tokens": 120,
    }
    try:
        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers={
                "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
                "Content-Type": "application/json",
            },
            json=payload,
            timeout=20,
        )
        resp.raise_for_status()
        return sanitize_public_text(resp.json()["choices"][0]["message"]["content"].strip())
    except Exception as e:
        print(f"  [主动话题] DeepSeek 生成失败: {e}", flush=True)
        return "刚刚群里安静了一会儿，小弟来开个话题：你们俩今晚想聊点轻松的，还是一起挑个番/电影看看？"


def _with_mentions(text: str) -> str:
    mentions = []
    if FEISHU_SANGE_OPEN_ID:
        mentions.append(f'<at user_id="{FEISHU_SANGE_OPEN_ID}">{owner_name()}</at>')
    else:
        mentions.append(owner_name())
    if FEISHU_SHUSHU_OPEN_ID:
        mentions.append(f'<at user_id="{FEISHU_SHUSHU_OPEN_ID}">{target_name()}</at>')
    else:
        mentions.append(target_name())
    return f"{' '.join(mentions)} {sanitize_public_text(text)}"
