"""本地状态持久化：记录已处理的事件和舒舒消息，避免重复汇报。"""
import json
from config import STATE_FILE

_MAX_PROCESSED = 500  # 最多保留 500 条已处理 ID


def load_state() -> dict:
    if STATE_FILE.exists():
        with open(STATE_FILE, "r", encoding="utf-8") as f:
            return json.load(f)
    return {
        "last_event_time": None,
        "processed_event_ids": [],
        "processed_shushu_message_ids": [],
    }


def save_state(state: dict) -> None:
    with open(STATE_FILE, "w", encoding="utf-8") as f:
        json.dump(state, f, indent=2, ensure_ascii=False)


# ---- GitHub 事件去重 ----

def filter_new_events(events: list[dict], state: dict) -> list[dict]:
    """返回尚未处理过的新事件（按 event id 和时间戳双重去重）。"""
    processed_ids = set(state.get("processed_event_ids", []))
    last_time = state.get("last_event_time")

    new_events = []
    for ev in events:
        ev_id = ev.get("id", "")
        created_at = ev.get("created_at", "")

        if ev_id and ev_id in processed_ids:
            continue
        if last_time and created_at and created_at <= last_time:
            continue
        new_events.append(ev)
    return new_events


def update_state(state: dict, new_events: list[dict]) -> None:
    """将新事件写入状态文件。"""
    processed_ids = list(state.get("processed_event_ids", []))
    latest_time = state.get("last_event_time")

    for ev in new_events:
        ev_id = ev.get("id", "")
        if ev_id and ev_id not in processed_ids:
            processed_ids.append(ev_id)
        created_at = ev.get("created_at", "")
        if created_at and (not latest_time or created_at > latest_time):
            latest_time = created_at

    state["processed_event_ids"] = processed_ids[-_MAX_PROCESSED:]
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
