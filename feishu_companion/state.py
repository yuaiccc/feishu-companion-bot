"""本地状态持久化：记录已处理事件、消息和旁听话题，避免重复汇报。"""
import json
import time
from feishu_companion.config import STATE_FILE

_MAX_PROCESSED = 500  # 最多保留 500 条已处理 ID


def load_state() -> dict:
    if STATE_FILE.exists():
        with open(STATE_FILE, "r", encoding="utf-8") as f:
            return json.load(f)
    return {
        "last_event_time": None,
        "processed_event_ids": [],
        "processed_event_fingerprints": [],
        "processed_shushu_message_ids": [],
        "passive_processed_message_ids": [],
        "passive_topic_timestamps": {},
        "passive_sent_timestamps": [],
        "proactive_topic_sent_dates": {},
        "streaming_reply_contexts": {},
        "streaming_reply_callback_ids": [],
    }


def save_state(state: dict) -> None:
    with open(STATE_FILE, "w", encoding="utf-8") as f:
        json.dump(state, f, indent=2, ensure_ascii=False)


# ---- GitHub 事件去重 ----

def filter_new_events(events: list[dict], state: dict) -> list[dict]:
    """返回尚未处理过的新事件（按 event id、指纹和时间戳去重）。"""
    from feishu_companion.github_client import event_fingerprint

    processed_ids = set(state.get("processed_event_ids", []))
    processed_fingerprints = set(state.get("processed_event_fingerprints", []))
    last_time = state.get("last_event_time")

    new_events = []
    batch_fingerprints = set()
    for ev in events:
        ev_id = ev.get("id", "")
        created_at = ev.get("created_at", "")
        fingerprint = event_fingerprint(ev)

        if ev_id and ev_id in processed_ids:
            continue
        if fingerprint and fingerprint in processed_fingerprints:
            continue
        if fingerprint and fingerprint in batch_fingerprints:
            continue
        if last_time and created_at and created_at <= last_time:
            continue
        if fingerprint:
            batch_fingerprints.add(fingerprint)
        new_events.append(ev)
    return new_events


def update_state(state: dict, new_events: list[dict]) -> None:
    """将新事件写入状态文件。"""
    from feishu_companion.github_client import event_fingerprint

    processed_ids = list(state.get("processed_event_ids", []))
    processed_fingerprints = list(state.get("processed_event_fingerprints", []))
    latest_time = state.get("last_event_time")

    for ev in new_events:
        ev_id = ev.get("id", "")
        if ev_id and ev_id not in processed_ids:
            processed_ids.append(ev_id)
        fingerprint = event_fingerprint(ev)
        if fingerprint and fingerprint not in processed_fingerprints:
            processed_fingerprints.append(fingerprint)
        created_at = ev.get("created_at", "")
        if created_at and (not latest_time or created_at > latest_time):
            latest_time = created_at

    state["processed_event_ids"] = processed_ids[-_MAX_PROCESSED:]
    state["processed_event_fingerprints"] = processed_fingerprints[-_MAX_PROCESSED:]
    state["last_event_time"] = latest_time
    save_state(state)


# ---- 舒舒消息去重 ----

def filter_new_shushu_messages(messages: list[dict], state: dict) -> list[dict]:
    """返回尚未处理过的舒舒新消息。"""
    processed_ids = set(state.get("processed_shushu_message_ids", []))
    new_messages = []
    for msg in messages:
        msg_id = msg.get("message_id", "")
        if msg_id and msg_id in processed_ids:
            continue
        new_messages.append(msg)
    return new_messages


def mark_shushu_messages_processed(state: dict, messages: list[dict]) -> None:
    """将舒舒消息标记为已处理。"""
    processed_ids = list(state.get("processed_shushu_message_ids", []))
    for msg in messages:
        msg_id = msg.get("message_id", "")
        if msg_id and msg_id not in processed_ids:
            processed_ids.append(msg_id)
    state["processed_shushu_message_ids"] = processed_ids[-_MAX_PROCESSED:]
    save_state(state)


# ---- 旁听辅助去重 / 冷却 ----

def is_passive_message_processed(state: dict, message_id: str) -> bool:
    return bool(message_id and message_id in set(state.get("passive_processed_message_ids", [])))


def mark_passive_message_processed(state: dict, message_id: str) -> None:
    if not message_id:
        return
    processed_ids = list(state.get("passive_processed_message_ids", []))
    if message_id not in processed_ids:
        processed_ids.append(message_id)
    state["passive_processed_message_ids"] = processed_ids[-_MAX_PROCESSED:]
    save_state(state)


def is_passive_topic_in_cooldown(state: dict, topic_key: str, cooldown_seconds: int, now: float | None = None) -> bool:
    if not topic_key:
        return True
    now = now or time.time()
    timestamps = state.get("passive_topic_timestamps", {}) or {}
    last_ts = float(timestamps.get(topic_key, 0) or 0)
    return last_ts > 0 and now - last_ts < cooldown_seconds


def can_send_passive_now(state: dict, max_per_hour: int, now: float | None = None) -> bool:
    now = now or time.time()
    sent = [float(ts) for ts in state.get("passive_sent_timestamps", []) if now - float(ts) < 3600]
    state["passive_sent_timestamps"] = sent
    return len(sent) < max_per_hour


def mark_passive_topic_sent(state: dict, topic_key: str, message_id: str, now: float | None = None) -> None:
    now = now or time.time()
    if message_id:
        processed_ids = list(state.get("passive_processed_message_ids", []))
        if message_id not in processed_ids:
            processed_ids.append(message_id)
        state["passive_processed_message_ids"] = processed_ids[-_MAX_PROCESSED:]
    if topic_key:
        timestamps = dict(state.get("passive_topic_timestamps", {}) or {})
        timestamps[topic_key] = now
        # Keep the state file small.
        state["passive_topic_timestamps"] = {
            key: ts for key, ts in timestamps.items() if now - float(ts) < 7 * 24 * 3600
        }
    sent = [float(ts) for ts in state.get("passive_sent_timestamps", []) if now - float(ts) < 3600]
    sent.append(now)
    state["passive_sent_timestamps"] = sent[-20:]
    save_state(state)


# ---- 主动话题去重 / 每日上限 ----

def proactive_sent_count(state: dict, date_key: str) -> int:
    sent = state.get("proactive_topic_sent_dates", {}) or {}
    return int(sent.get(date_key, 0) or 0)


def can_send_proactive_today(state: dict, date_key: str, max_per_day: int) -> bool:
    return proactive_sent_count(state, date_key) < max_per_day


def mark_proactive_sent(state: dict, date_key: str, now: float | None = None) -> None:
    now = now or time.time()
    sent = dict(state.get("proactive_topic_sent_dates", {}) or {})
    sent[date_key] = int(sent.get(date_key, 0) or 0) + 1
    # Keep roughly one month of daily counters.
    state["proactive_topic_sent_dates"] = {
        key: count for key, count in sent.items()
        if key >= time.strftime("%Y-%m-%d", time.localtime(now - 31 * 24 * 3600))
    }
    state["proactive_topic_last_sent_at"] = now
    save_state(state)


# ---- 流式回复卡片短期上下文 / 按钮幂等 ----

def remember_streaming_reply_context(state: dict, message_id: str, context: dict, now: float | None = None) -> None:
    if not message_id:
        return
    now = now or time.time()
    contexts = dict(state.get("streaming_reply_contexts", {}) or {})
    compact = dict(context)
    compact["created_at"] = now
    contexts[message_id] = compact
    state["streaming_reply_contexts"] = {
        key: value for key, value in contexts.items()
        if now - float(value.get("created_at", now)) < 24 * 3600
    }
    save_state(state)


def get_streaming_reply_context(state: dict, message_id: str) -> dict:
    return dict((state.get("streaming_reply_contexts", {}) or {}).get(message_id, {}) or {})


def is_streaming_callback_processed(state: dict, callback_id: str) -> bool:
    return bool(callback_id and callback_id in set(state.get("streaming_reply_callback_ids", [])))


def mark_streaming_callback_processed(state: dict, callback_id: str) -> None:
    if not callback_id:
        return
    ids = list(state.get("streaming_reply_callback_ids", []) or [])
    if callback_id not in ids:
        ids.append(callback_id)
    state["streaming_reply_callback_ids"] = ids[-_MAX_PROCESSED:]
    save_state(state)
