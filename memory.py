"""Privacy-first memory module with local embeddings and agentic retrieval.

The JSON file remains the source of truth. Embeddings are deterministic local
hash vectors, so private memories are not sent to a third-party embedding API.
DeepSeek is only used after visibility filtering, and never receives memories
marked ``private``.
"""
import json
import math
import os
import re
import hashlib
from datetime import datetime

from config import (
    DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL,
    MEMORY_ENABLED, MEMORY_DIR, MEMORY_EMBEDDING_ENABLED,
    MEMORY_AGENTIC_RAG_ENABLED, MEMORY_EMBEDDING_DIM, MEMORY_RAG_CANDIDATES,
    MEMORY_EMBEDDING_PROVIDER, MEMORY_OLLAMA_BASE_URL,
    MEMORY_OLLAMA_EMBED_MODEL, MEMORY_OLLAMA_TIMEOUT_SECONDS,
)
from profile import memory_category_keywords, profile_id

import requests

_MEMORY_FILE = None
_MAX_MEMORIES = int(os.getenv("MEMORY_MAX_ITEMS", "200"))
_HASH_EMBEDDING_MODEL = "local-hash-ngram-v1"
_LAST_EMBEDDING_MODEL = _HASH_EMBEDDING_MODEL
_OLLAMA_FAILED = False
_VISIBILITY_PUBLIC = "public_to_target"
_VISIBILITY_OWNER = "owner_only"
_VISIBILITY_PRIVATE = "private"
_CATEGORY_KEYWORDS = {
    "person": ("生日", "学校", "大学", "本科", "家住", "地址", "家里", "姓名", "小名"),
    "relationship": ("想你", "爱你", "在一起", "电话", "晚安", "抱抱"),
    "preference": ("喜欢", "不喜欢", "偏好", "爱喝", "不加糖", "吃", "喝", "散步"),
    "schedule": ("晚上", "早上", "明天", "今天", "最近", "待在", "回家", "学校"),
}
_PRIVATE_PATTERNS = (
    re.compile(r"(?i)\b(sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{20,})\b"),
    re.compile(r"(?i)\b(api[_-]?key|secret|token|password|passwd|密码|密钥)\b"),
    re.compile(r"(家住|住址|地址|小区|栋|单元|门牌|身份证|手机号|电话号)"),
    re.compile(r"\d+\s*(栋|单元|室|号楼|号)"),
)
_OWNER_ONLY_PATTERNS = (
    re.compile(r"[\u4e00-\u9fff]{2,}(省|市|区|县|乡|镇|街道|路|巷)"),
)
_QUERY_STOP_TERMS = {
    "什么", "怎么", "哪里", "在哪", "最近", "一下", "这个", "那个",
    "她的", "他的", "是不是", "有没有", "可以", "需要",
}


def _get_memory_file():
    """获取记忆文件路径。"""
    global _MEMORY_FILE
    if _MEMORY_FILE is None:
        profile_dir = MEMORY_DIR / profile_id()
        profile_dir.mkdir(parents=True, exist_ok=True)
        profile_file = profile_dir / "memories.json"
        legacy_file = MEMORY_DIR / "memories.json"
        if not profile_file.exists() and legacy_file.exists():
            profile_file.write_text(legacy_file.read_text(encoding="utf-8"), encoding="utf-8")
        _MEMORY_FILE = str(profile_file)
    return _MEMORY_FILE


def _load_all() -> list[dict]:
    """加载所有记忆。"""
    path = _get_memory_file()
    if not os.path.exists(path):
        return []
    try:
        with open(path, "r", encoding="utf-8") as f:
            data = json.load(f)
        if not isinstance(data, list):
            return []
        normalized = []
        changed = False
        for idx, mem in enumerate(data, start=1):
            if not isinstance(mem, dict):
                continue
            before = json.dumps(mem, ensure_ascii=False, sort_keys=True)
            normalized.append(_normalize_memory_entry(mem, idx))
            after = json.dumps(normalized[-1], ensure_ascii=False, sort_keys=True)
            changed = changed or before != after
        if changed:
            _save_all(normalized)
        return normalized
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


def _normalize_memory_entry(mem: dict, idx: int = 0) -> dict:
    content = str(mem.get("content", "")).strip()
    category = mem.get("category") or _categorize(content)
    visibility = _most_restrictive_visibility(
        mem.get("visibility") or _VISIBILITY_PUBLIC,
        _infer_visibility(content, category),
    )
    now = datetime.now().strftime("%Y-%m-%d %H:%M")
    mem["id"] = str(mem.get("id") or f"mem_{idx}")
    mem["content"] = content
    mem["category"] = category
    mem["visibility"] = _normalize_visibility(visibility)
    mem["importance"] = int(mem.get("importance") or _importance(content, category))
    mem["confidence"] = float(mem.get("confidence") or 0.7)
    mem["time"] = mem.get("time") or now
    mem["last_seen"] = mem.get("last_seen") or mem.get("time") or now
    mem["seen_count"] = int(mem.get("seen_count") or 1)
    mem["embedding_model"] = mem.get("embedding_model") or _HASH_EMBEDDING_MODEL
    if MEMORY_EMBEDDING_ENABLED and (
        not _valid_embedding(mem.get("embedding"))
        or mem.get("embedding_model") != _current_embedding_model()
    ):
        mem["embedding"] = _embed_text(content)
        mem["embedding_model"] = _last_embedding_model()
    if "source_messages" in mem:
        mem["source_messages"] = _redact_source_messages(mem.get("source_messages") or [])
    return mem


def _categorize(text: str) -> str:
    keywords_by_category = dict(_CATEGORY_KEYWORDS)
    keywords_by_category.update(memory_category_keywords())
    for category, keywords in keywords_by_category.items():
        if any(keyword in text for keyword in keywords):
            return category
    return "note"


def _infer_visibility(text: str, category: str) -> str:
    if _contains_private_secret(text):
        return _VISIBILITY_PRIVATE
    if _contains_owner_only_info(text):
        return _VISIBILITY_OWNER
    if category in ("person", "schedule") and any(word in text for word in ("家", "住", "地址", "电话")):
        return _VISIBILITY_OWNER
    return _VISIBILITY_PUBLIC


def _normalize_visibility(visibility: str) -> str:
    if visibility in (_VISIBILITY_PUBLIC, _VISIBILITY_OWNER, _VISIBILITY_PRIVATE):
        return visibility
    return _VISIBILITY_OWNER


def _contains_private_secret(text: str) -> bool:
    return any(pattern.search(text or "") for pattern in _PRIVATE_PATTERNS)


def _contains_owner_only_info(text: str) -> bool:
    return any(pattern.search(text or "") for pattern in _OWNER_ONLY_PATTERNS)


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
    existing["visibility"] = _most_restrictive_visibility(
        existing.get("visibility"),
        _infer_visibility(fact, _categorize(fact)),
    )
    existing["confidence"] = max(float(existing.get("confidence", 0.7)), 0.75)
    if MEMORY_EMBEDDING_ENABLED:
        existing["embedding"] = _embed_text(existing.get("content", ""))
        existing["embedding_model"] = _last_embedding_model()
    existing["source_messages"] = _redact_source_messages(messages)


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


def _most_restrictive_visibility(*values: str) -> str:
    rank = {_VISIBILITY_PUBLIC: 0, _VISIBILITY_OWNER: 1, _VISIBILITY_PRIVATE: 2}
    normalized = [_normalize_visibility(v or _VISIBILITY_OWNER) for v in values]
    return max(normalized, key=lambda v: rank[v])


def _redact_source_messages(messages: list[dict]) -> list[dict]:
    safe = []
    for m in messages or []:
        content = str(m.get("content", ""))
        if _contains_private_secret(content):
            content = "[已隐藏敏感内容]"
        elif _contains_owner_only_info(content):
            content = content[:20] + "..." if len(content) > 20 else content
        else:
            content = content[:50]
        safe.append({
            "sender": m.get("sender", ""),
            "content": content,
            "time": m.get("time", ""),
        })
    return safe


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
    """Keyword score with Chinese ngrams, avoiding noisy single-character hits."""
    if not query or not text:
        return 0.0
    text_lower = text.lower()
    score = 0.0
    for term in _query_terms(query):
        if term in text_lower:
            score += min(len(term), 6) * 0.25
    return score


def _query_terms(query: str) -> set[str]:
    normalized = _normalize_text(query)
    terms = set()
    for token in re.findall(r"[A-Za-z0-9_]{2,}", query.lower()):
        terms.add(token)
    for n in (2, 3, 4):
        if len(normalized) >= n:
            for i in range(len(normalized) - n + 1):
                term = normalized[i:i + n]
                if term not in _QUERY_STOP_TERMS:
                    terms.add(term)
    return terms


def _embed_text(text: str) -> list[float]:
    """Create an embedding with the configured provider, falling back locally."""
    global _LAST_EMBEDDING_MODEL, _OLLAMA_FAILED
    if MEMORY_EMBEDDING_PROVIDER == "ollama" and not _OLLAMA_FAILED:
        embedding = _embed_with_ollama(text)
        if embedding:
            _LAST_EMBEDDING_MODEL = _current_embedding_model()
            return embedding
        _OLLAMA_FAILED = True

    _LAST_EMBEDDING_MODEL = _HASH_EMBEDDING_MODEL
    return _embed_text_hash(text)


def _embed_with_ollama(text: str) -> list[float]:
    if not text:
        return []
    try:
        session = requests.Session()
        session.trust_env = False
        resp = session.post(
            f"{MEMORY_OLLAMA_BASE_URL}/api/embed",
            json={"model": MEMORY_OLLAMA_EMBED_MODEL, "input": text},
            timeout=MEMORY_OLLAMA_TIMEOUT_SECONDS,
        )
        resp.raise_for_status()
        data = resp.json()
        embeddings = data.get("embeddings") or []
        if embeddings and isinstance(embeddings[0], list):
            return _normalize_vector([float(v) for v in embeddings[0]])
    except Exception as e:
        print(f"  [memory] Ollama embedding 失败，回退本地哈希: {e}", flush=True)
    return []


def _embed_text_hash(text: str) -> list[float]:
    """Create a deterministic local embedding from character ngrams."""
    dim = max(int(MEMORY_EMBEDDING_DIM or 256), 32)
    vec = [0.0] * dim
    normalized = _normalize_text(text)
    if not normalized:
        return vec

    grams = []
    for n in (1, 2, 3):
        if len(normalized) >= n:
            grams.extend(normalized[i:i + n] for i in range(len(normalized) - n + 1))
    for token in re.findall(r"[A-Za-z0-9_]+", text or ""):
        grams.append(token.lower())

    for gram in grams:
        digest = hashlib.blake2b(gram.encode("utf-8"), digest_size=8).digest()
        bucket = int.from_bytes(digest[:4], "big") % dim
        sign = 1.0 if digest[4] % 2 == 0 else -1.0
        weight = 1.4 if len(gram) >= 2 else 0.7
        vec[bucket] += sign * weight

    return _normalize_vector(vec)


def _normalize_vector(vec: list[float]) -> list[float]:
    norm = math.sqrt(sum(v * v for v in vec))
    if norm <= 0:
        return vec
    return [round(v / norm, 6) for v in vec]


def _valid_embedding(value) -> bool:
    return isinstance(value, list) and len(value) > 0


def _current_embedding_model() -> str:
    if MEMORY_EMBEDDING_PROVIDER == "ollama":
        return f"ollama:{MEMORY_OLLAMA_EMBED_MODEL}"
    return _HASH_EMBEDDING_MODEL


def _last_embedding_model() -> str:
    return _LAST_EMBEDDING_MODEL


def _cosine(a: list[float], b: list[float]) -> float:
    if not a or not b or len(a) != len(b):
        return 0.0
    return sum(x * y for x, y in zip(a, b))


def _allowed_for_audience(mem: dict, audience: str) -> bool:
    visibility = _normalize_visibility(mem.get("visibility"))
    if visibility == _VISIBILITY_PRIVATE:
        return False
    if audience == "owner":
        return True
    return visibility == _VISIBILITY_PUBLIC


def _memory_score(query: str, query_embedding: list[float], mem: dict) -> float:
    text = mem.get("content", "")
    keyword = _keyword_score(query, text)
    source_score = sum(
        _keyword_score(query, sm.get("content", "")) * 0.2
        for sm in mem.get("source_messages", [])
    )
    semantic = _cosine(query_embedding, mem.get("embedding", [])) if MEMORY_EMBEDDING_ENABLED else 0.0
    importance = int(mem.get("importance", 1)) * 0.12
    repeated = min(int(mem.get("seen_count", 1)), 5) * 0.06
    return keyword + source_score + max(semantic, 0.0) * 4.0 + importance + repeated


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
        visibility = _infer_visibility(fact, category)
        entry = {
            "id": f"mem_{len(all_memories) + len(new_entries) + 1}",
            "content": fact,
            "category": category,
            "importance": _importance(fact, category),
            "visibility": visibility,
            "confidence": 0.75,
            "time": now,
            "last_seen": now,
            "seen_count": 1,
            "embedding_model": "",
            "embedding": _embed_text(fact) if MEMORY_EMBEDDING_ENABLED else [],
            "source_messages": _redact_source_messages(messages),
        }
        entry["embedding_model"] = _last_embedding_model() if MEMORY_EMBEDDING_ENABLED else ""
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


def search_memories(
    query: str,
    user_id: str = "shushu_chat",
    top_k: int = 5,
    audience: str = "target",
) -> list[str]:
    """Search relevant memories with privacy filtering, embeddings, and agentic rerank."""
    if not MEMORY_ENABLED:
        return []

    all_memories = _load_all()
    if not all_memories:
        return []

    query_embedding = _embed_text(query) if MEMORY_EMBEDDING_ENABLED else []
    scored = []
    for mem in all_memories:
        mem = _normalize_memory_entry(mem)
        if not _allowed_for_audience(mem, audience):
            continue
        score = _memory_score(query, query_embedding, mem)
        if score > 0:
            scored.append((score, mem))

    scored.sort(key=lambda x: x[0], reverse=True)
    candidates = [mem for _, mem in scored[:max(MEMORY_RAG_CANDIDATES, top_k)]]
    candidates = _dedupe_memory_candidates(candidates)
    if not candidates:
        return []
    selected = _agentic_select_memories(query, candidates, top_k, audience)
    return [m.get("content", "") for m in selected[:top_k] if m.get("content")]


def _dedupe_memory_candidates(candidates: list[dict]) -> list[dict]:
    deduped = []
    seen = []
    for mem in candidates:
        normalized = _normalize_text(mem.get("content", ""))
        if not normalized:
            continue
        if any(normalized == item or normalized in item or item in normalized for item in seen):
            continue
        seen.append(normalized)
        deduped.append(mem)
    return deduped


def _agentic_select_memories(query: str, candidates: list[dict], top_k: int, audience: str) -> list[dict]:
    """Let DeepSeek choose the final context from already privacy-filtered candidates."""
    if not MEMORY_AGENTIC_RAG_ENABLED or not DEEPSEEK_API_KEY or len(candidates) <= top_k:
        return candidates[:top_k]

    safe_candidates = [
        {
            "id": str(mem.get("id")),
            "content": mem.get("content", ""),
            "category": mem.get("category", "note"),
            "importance": mem.get("importance", 1),
            "visibility": mem.get("visibility", _VISIBILITY_PUBLIC),
        }
        for mem in candidates
        if _allowed_for_audience(mem, audience)
    ]
    if not safe_candidates:
        return []

    system = (
        "你是隐私优先的记忆检索器。只能从候选记忆中选择与用户当前问题真正相关的记忆。"
        "不要扩写、不要编造、不要选择 private 记忆。"
        "如果候选记忆只是泛泛相关或可能造成隐私泄露，就不要选。"
        "只输出 JSON 数组，数组元素是记忆 id 字符串。"
    )
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": system},
            {
                "role": "user",
                "content": json.dumps(
                    {
                        "query": query,
                        "audience": audience,
                        "max_items": top_k,
                        "candidates": safe_candidates,
                    },
                    ensure_ascii=False,
                ),
            },
        ],
        "temperature": 0.0,
        "max_tokens": 160,
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
        raw = resp.json()["choices"][0]["message"]["content"].strip()
        ids = _parse_json_id_list(raw)
        if not ids:
            return candidates[:top_k]
        by_id = {str(mem.get("id")): mem for mem in candidates}
        selected = [by_id[mid] for mid in ids if mid in by_id and _allowed_for_audience(by_id[mid], audience)]
        return selected[:top_k] or candidates[:top_k]
    except Exception as e:
        print(f"  [memory] agentic rerank 失败: {e}", flush=True)
        return candidates[:top_k]


def _parse_json_id_list(raw: str) -> list[str]:
    text = (raw or "").strip()
    if text.startswith("```"):
        text = text.strip("`").strip()
        if text.lower().startswith("json"):
            text = text[4:].strip()
    start = text.find("[")
    end = text.rfind("]")
    if start >= 0 and end >= start:
        text = text[start:end + 1]
    try:
        data = json.loads(text)
    except Exception:
        return []
    if not isinstance(data, list):
        return []
    return [str(item) for item in data if isinstance(item, (str, int))]


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
