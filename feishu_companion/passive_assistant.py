"""Passive background helper for non-mentioned group chat messages."""
from __future__ import annotations

import hashlib
import re
import threading
import time
from dataclasses import dataclass

from feishu_companion.config import (
    PASSIVE_ASSIST_ENABLED,
    PASSIVE_ASSIST_MAX_PER_HOUR,
    PASSIVE_ASSIST_QUIET_SECONDS,
    PASSIVE_ASSIST_RECENT_WINDOW_SECONDS,
    PASSIVE_ASSIST_TOPIC_COOLDOWN_SECONDS,
)
from feishu_companion.external_search import build_external_search_card
from feishu_companion.feishu_api import send_card
from feishu_companion.state import (
    can_send_passive_now,
    is_passive_message_processed,
    is_passive_topic_in_cooldown,
    load_state,
    mark_passive_message_processed,
    mark_passive_topic_sent,
)


_TOPIC_PATTERNS = [
    (re.compile(r"(CLANNAD|clannad|古河渚|冈崎朋也|岡崎朋也)"), "CLANNAD 古河渚 作品角色 背景介绍"),
    (re.compile(r"(B站|b站|哔哩|bilibili).{0,12}(新番|番剧|热门|热榜)|(?:新番|番剧).{0,12}(B站|b站|哔哩|bilibili|热门|推荐)"), "近期 B站 热门 新番 推荐"),
    (re.compile(r"(新番|番剧|动漫|动画|恋爱番|虐番|BE|HE)"), ""),
    (re.compile(r"(电影|电视剧|游戏|小说|漫画|书|角色|人物|景点|学校|大学|城市|新闻|热搜|热榜)"), ""),
]

_LOW_SIGNAL = (
    "哈哈", "hhh", "233", "嗯", "哦", "好", "好的", "可以", "想你", "爱你",
    "晚安", "早安", "摸头", "亲亲", "抱抱", "贴贴",
)


@dataclass
class PassiveCandidate:
    message_id: str
    chat_id: str
    content: str
    query: str
    topic_key: str


class PassiveAssistant:
    """Collect non-mentioned messages and send background cards after silence."""

    def __init__(self):
        self._messages: list[dict] = []
        self._lock = threading.Lock()
        self._timer: threading.Timer | None = None

    def on_message(self, msg_data: dict):
        if not PASSIVE_ASSIST_ENABLED:
            return
        if msg_data.get("chat_type") != "group":
            return
        content = (msg_data.get("content") or "").strip()
        if not content:
            return

        now = time.time()
        item = dict(msg_data)
        item["_received_at"] = now

        with self._lock:
            self._messages.append(item)
            cutoff = now - PASSIVE_ASSIST_RECENT_WINDOW_SECONDS
            self._messages = [m for m in self._messages if m.get("_received_at", 0) >= cutoff]
            if self._timer:
                self._timer.cancel()
            self._timer = threading.Timer(PASSIVE_ASSIST_QUIET_SECONDS, self._on_quiet_period)
            self._timer.daemon = True
            self._timer.start()

    def _on_quiet_period(self):
        try:
            candidate = self._pick_candidate()
            if not candidate:
                return

            state = load_state()
            if is_passive_message_processed(state, candidate.message_id):
                print(f"  [旁听] 消息已处理，跳过: {candidate.message_id}", flush=True)
                return
            if is_passive_topic_in_cooldown(
                state,
                candidate.topic_key,
                PASSIVE_ASSIST_TOPIC_COOLDOWN_SECONDS,
            ):
                print(f"  [旁听] 话题冷却中，跳过: {candidate.topic_key}", flush=True)
                mark_passive_message_processed(state, candidate.message_id)
                return
            if not can_send_passive_now(state, PASSIVE_ASSIST_MAX_PER_HOUR):
                print("  [旁听] 达到每小时上限，跳过", flush=True)
                mark_passive_message_processed(state, candidate.message_id)
                return

            print(f"  [旁听] 静默后补背景: query={candidate.query}", flush=True)
            card = build_external_search_card(candidate.query)
            send_card(card, receive_id=candidate.chat_id)
            state = load_state()
            mark_passive_topic_sent(state, candidate.topic_key, candidate.message_id)
        except Exception as e:
            print(f"  [旁听] 处理失败: {e}", flush=True)

    def _pick_candidate(self) -> PassiveCandidate | None:
        now = time.time()
        with self._lock:
            recent = [
                m for m in self._messages
                if now - float(m.get("_received_at", 0)) <= PASSIVE_ASSIST_RECENT_WINDOW_SECONDS
            ]
        if not recent:
            return None
        # If new chat arrived after this timer should have fired, do not interrupt.
        latest_ts = max(float(m.get("_received_at", 0)) for m in recent)
        if now - latest_ts < PASSIVE_ASSIST_QUIET_SECONDS - 1:
            return None

        for msg in reversed(recent):
            content = _normalize_content(msg.get("content", ""))
            if not _is_high_signal(content):
                continue
            query = _query_for_content(content)
            if not query:
                continue
            topic_key = _topic_key(query)
            return PassiveCandidate(
                message_id=msg.get("message_id", ""),
                chat_id=msg.get("chat_id", ""),
                content=content,
                query=query,
                topic_key=topic_key,
            )
        return None


def _normalize_content(content: str) -> str:
    content = re.sub(r"<[^>]+>", "", content or "")
    return re.sub(r"\s+", " ", content).strip()


def _is_high_signal(content: str) -> bool:
    if len(content) < 4:
        return False
    lowered = content.lower()
    if any(word in lowered for word in _LOW_SIGNAL) and len(content) <= 12:
        return False
    if "?" in content or "？" in content or any(w in content for w in ("是什么", "啥", "为什么", "怎么", "哪里", "哪个")):
        return True
    return any(pattern.search(content) for pattern, _ in _TOPIC_PATTERNS)


def _query_for_content(content: str) -> str:
    for pattern, query in _TOPIC_PATTERNS:
        if pattern.search(content):
            return query or f"{content} 背景 介绍 推荐"
    if len(content) <= 80:
        return f"{content} 背景 介绍"
    return ""


def _topic_key(query: str) -> str:
    normalized = re.sub(r"\s+", "", query.lower())
    return hashlib.sha1(normalized.encode("utf-8")).hexdigest()[:16]
