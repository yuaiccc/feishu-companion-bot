"""轻量记忆模块：JSON 文件存储 + DeepSeek 提取关键信息 + 关键词搜索。
不依赖 scipy/sklearn/HuggingFace，稳定可靠。
"""
import json
import os
import re
from datetime import datetime

from config import (
    DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL,
    MEMORY_ENABLED, MEMORY_DIR,
)

import requests

_MEMORY_FILE = None
_MAX_MEMORIES = int(os.getenv("MEMORY_MAX_ITEMS", "200"))
_CATEGORY_KEYWORDS = {
    "person": ("生日", "学校", "大学", "本科", "家住", "地址", "家里", "姓名", "小名"),
    "relationship": ("想你", "爱你", "在一起", "舒舒", "烨子", "秋酿", "三哥", "电话", "晚安", "抱抱"),
    "preference": ("喜欢", "不喜欢", "偏好", "爱喝", "不加糖", "吃", "喝", "散步"),
    "schedule": ("晚上", "早上", "明天", "今天", "最近", "待在", "回家", "学校"),
}


def _get_memory_file():
    """获取记忆文件路径。"""
    global _MEMORY_FILE
    if _MEMORY_FILE is None:
        MEMORY_DIR.mkdir(parents=True, exist_ok=True)
        _MEMORY_FILE = str(MEMORY_DIR / "memories.json")
    return _MEMORY_FILE


def _load_all() -> list[dict]:
    """加载所有记忆。"""
    path = _get_memory_file()
    if not os.path.exists(path):
        return []
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except (json.JSONDecodeError, IOError):
        return []


def _save_all(memories: list[dict]):
    """保存所有记忆。"""
    path = _get_memory_file()
    with open(path, "w", encoding="utf-8") as f:
        json.dump(memories, f, ensure_ascii=False, indent=2)


def _normalize_text(text: str) -> str:
    text = re.sub(r"\s+", "", (text or "").lower())
    text = re.sub(r"[，。！？、,.!?；;：:（）()【】\[\]\"'“”‘’]", "", text)
    return text


def _categorize(text: str) -> str:
    for category, keywords in _CATEGORY_KEYWORDS.items():
        if any(keyword in text for keyword in keywords):
            return category
    return "note"


def _importance(text: str, category: str) -> int:
    score = 2
    if category in ("person", "relationship"):
        score += 2
    if category == "preference":
        score += 1
    if any(word in text for word in ("生日", "学校", "家住", "电话", "不加糖", "在一起")):
        score += 1
    return min(score, 5)


def _merge_memory(existing: dict, fact: str, now: str, messages: list[dict]):
    existing["content"] = fact if len(fact) > len(existing.get("content", "")) else existing.get("content", fact)
    existing["last_seen"] = now
    existing["seen_count"] = int(existing.get("seen_count", 1)) + 1
    existing["category"] = existing.get("category") or _categorize(existing.get("content", ""))
    existing["importance"] = max(int(existing.get("importance", 1)), _importance(existing.get("content", ""), existing["category"]))
    existing["source_messages"] = [
        {"sender": m["sender"], "content": m["content"][:50], "time": m["time"]}
        for m in messages
    ]


def _prune_memories(memories: list[dict]) -> list[dict]:
    if len(memories) <= _MAX_MEMORIES:
        return memories
    def key(mem: dict):
        return (
            int(mem.get("importance", 1)),
            int(mem.get("seen_count", 1)),
            mem.get("last_seen") or mem.get("time") or "",
        )
    return sorted(memories, key=key, reverse=True)[:_MAX_MEMORIES]


def _extract_facts(messages: list[dict]) -> list[str]:
    """用 DeepSeek 从对话中提取关键信息（记忆）。"""
    if not messages:
        return []

    chat_text = "\n".join(
        f"[{m['time']}] {m['sender']}: {m['content']}" for m in messages
    )

    prompt = """你是一个记忆提取器。从以下对话中提取关键信息（比如：喜好、习惯、重要事件、情感状态等）。
每条记忆用一句话描述，直接输出，每行一条，不要编号。
只提取有长期价值的信息，忽略无意义的寒暄。
如果没有值得记忆的信息，输出空行。"""

    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": prompt},
            {"role": "user", "content": chat_text},
        ],
        "temperature": 0.1,
        "max_tokens": 200,
    }

    try:
        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers=headers,
            json=payload,
            timeout=30,
        )
        resp.raise_for_status()
        text = resp.json()["choices"][0]["message"]["content"].strip()
        facts = [line.strip() for line in text.split("\n") if line.strip()]
        return facts
    except Exception as e:
        print(f"  [memory] 提取记忆失败: {e}", flush=True)
        return []


def _keyword_score(query: str, text: str) -> float:
    """简单的关键词匹配评分。返回 0-1 的分数。"""
    if not query or not text:
        return 0.0
    query_words = set(query.lower().split())
    text_lower = text.lower()
    score = 0.0
    for word in query_words:
        if len(word) >= 2 and word in text_lower:
            score += 1.0
    # 也做单字匹配（中文）
    for char in query:
        if char in text:
            score += 0.3
    return score


def add_memories(messages: list[dict], user_id: str = "shushu_chat") -> list:
    """把对话存入记忆。DeepSeek 自动提取关键信息。"""
    if not MEMORY_ENABLED or not messages:
        return []

    # 提取关键信息
    facts = _extract_facts(messages)
    if not facts:
        return []

    all_memories = _load_all()
    now = datetime.now().strftime("%Y-%m-%d %H:%M")
    new_entries = []
    normalized_index = {
        _normalize_text(m.get("content", "")): m
        for m in all_memories
        if m.get("content")
    }

    for fact in facts:
        normalized = _normalize_text(fact)
        if not normalized:
            continue
        existing = normalized_index.get(normalized)
        if not existing:
            for key, mem in normalized_index.items():
                if normalized in key or key in normalized:
                    existing = mem
                    break
        if existing:
            _merge_memory(existing, fact, now, messages)
            continue
        category = _categorize(fact)
        entry = {
            "id": f"mem_{len(all_memories) + len(new_entries) + 1}",
            "content": fact,
            "category": category,
            "importance": _importance(fact, category),
            "time": now,
            "last_seen": now,
            "seen_count": 1,
            "source_messages": [
                {"sender": m["sender"], "content": m["content"][:50], "time": m["time"]}
                for m in messages
            ],
        }
        new_entries.append(entry)
        all_memories.append(entry)
        normalized_index[normalized] = entry

    all_memories = _prune_memories(all_memories)
    _save_all(all_memories)

    if new_entries:
        print(f"  [memory] 新增 {len(new_entries)} 条记忆:", flush=True)
        for e in new_entries:
            print(f"    {e['content']}", flush=True)

    return new_entries


def search_memories(query: str, user_id: str = "shushu_chat", top_k: int = 5) -> list[str]:
    """搜索相关记忆。使用关键词匹配。"""
    if not MEMORY_ENABLED:
        return []

    all_memories = _load_all()
    if not all_memories:
        return []

    scored = []
    for mem in all_memories:
        text = mem.get("content", "")
        score = _keyword_score(query, text)
        # 也搜索 source_messages
        for sm in mem.get("source_messages", []):
            score += _keyword_score(query, sm.get("content", "")) * 0.5
        if score > 0:
            score += int(mem.get("importance", 1)) * 0.2
            score += min(int(mem.get("seen_count", 1)), 5) * 0.1
            scored.append((score, text))

    scored.sort(key=lambda x: x[0], reverse=True)
    results = [text for _, text in scored[:top_k]]

    return results


def get_all_memories(user_id: str = "shushu_chat") -> list[str]:
    """获取所有记忆（用于展示或调试）。"""
    if not MEMORY_ENABLED:
        return []
    all_memories = _load_all()
    return [m.get("content", "") for m in all_memories]


def format_for_deepseek(memories: list[str]) -> str:
    """把记忆格式化成给 DeepSeek 看的文本。"""
    if not memories:
        return ""
    lines = ["--- 相关记忆 ---"]
    for m in memories:
        lines.append(f"  - {m}")
    return "\n".join(lines)
