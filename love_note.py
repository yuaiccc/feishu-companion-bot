"""Daily love-note reaction posted as a comment on an existing Feishu Docx/Wiki."""
from __future__ import annotations

import html
import json
import uuid
from datetime import datetime, timedelta, timezone

import requests

from config import (
    DEEPSEEK_API_KEY,
    DEEPSEEK_BASE_URL,
    DEEPSEEK_MODEL,
    FEISHU_APP_ID,
    FEISHU_APP_SECRET,
    FEISHU_OPEN_API,
    LOVE_NOTE_DOC_TOKEN,
    LOVE_NOTE_RUN_AT,
    LOVE_NOTE_WIKI_TOKEN,
)
from state import load_state, save_state
from text_safety import sanitize_public_text


_SHANGHAI = timezone(timedelta(hours=8))
_MAX_DOCUMENT_SOURCE_CHARS = 12000
_MAX_DAILY_LOVE_NOTE_COMMENTS = 2


def run_daily_love_note(target_date: datetime | None = None, force: bool = False) -> str:
    """React to the configured love-note document and comment on it."""
    target_date = target_date or datetime.now(_SHANGHAI)
    date_key = target_date.strftime("%Y-%m-%d")
    doc_token = LOVE_NOTE_DOC_TOKEN or resolve_wiki_doc_token(LOVE_NOTE_WIKI_TOKEN)
    if not doc_token:
        raise RuntimeError("缺少 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")

    state = load_state()
    daily_counts = dict(state.get("love_note_daily_comment_counts", {}) or {})
    already_sent = int(daily_counts.get(date_key, 0) or 0)
    remaining = _MAX_DAILY_LOVE_NOTE_COMMENTS - already_sent
    if not force and remaining <= 0:
        return f"{date_key} 恋爱笔记短评已达到每日上限 {_MAX_DAILY_LOVE_NOTE_COMMENTS} 条，跳过。"

    document = get_docx_document(doc_token)
    blocks = get_docx_blocks(doc_token)
    current_blocks = _text_block_candidates(blocks)
    if not current_blocks:
        return f"{date_key} 没有读到可评论的文档内容，跳过。"

    seen_ids = set(state.get("love_note_seen_block_ids", []) or [])
    if not seen_ids and not force:
        _mark_love_note_seen(state, current_blocks, document)
        save_state(state)
        return f"{date_key} 已建立恋爱笔记增量基线，本次不评论旧内容。"

    new_blocks = [block for block in current_blocks if block["block_id"] not in seen_ids]
    if not new_blocks:
        _mark_love_note_seen(state, current_blocks, document)
        save_state(state)
        return f"{date_key} 恋爱笔记没有新增正文，不评论。"

    limit = _MAX_DAILY_LOVE_NOTE_COMMENTS if force else max(0, remaining)
    reactions = generate_love_note_reactions(new_blocks, date_key, limit=limit)
    created = []
    used_block_ids = set()
    for item in reactions:
        block_id = item.get("block_id", "")
        comment = item.get("comment", "")
        valid_ids = {block["block_id"] for block in new_blocks}
        if block_id not in valid_ids or block_id in used_block_ids:
            continue
        if not _is_acceptable_reaction(comment):
            continue
        result = create_docx_comment(doc_token, block_id, comment)
        created.append({
            "block_id": block_id,
            "comment": comment,
            "comment_id": result.get("data", {}).get("comment_id", ""),
            "reply_id": result.get("data", {}).get("reply_id", ""),
        })
        used_block_ids.add(block_id)
        if len(created) >= limit:
            break

    _mark_love_note_seen(state, current_blocks, document)
    daily_counts[date_key] = already_sent + len(created)
    state["love_note_daily_comment_counts"] = _trim_daily_counts(daily_counts)
    state["last_love_note_date"] = date_key
    save_state(state)
    if not created:
        return f"{date_key} 恋爱笔记有新增内容，但没有找到适合短评的段落。"
    return "\n".join(item["comment"] for item in created)


def preview_daily_love_note(target_date: datetime | None = None) -> str:
    """Generate the daily love-note reaction without writing the document or state."""
    target_date = target_date or datetime.now(_SHANGHAI)
    doc_token = LOVE_NOTE_DOC_TOKEN or resolve_wiki_doc_token(LOVE_NOTE_WIKI_TOKEN)
    if not doc_token:
        raise RuntimeError("缺少 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")
    blocks = get_docx_blocks(doc_token)
    current_blocks = _text_block_candidates(blocks)
    if not current_blocks:
        return f"{target_date:%Y-%m-%d} 没有读到可评论的文档内容。"
    state = load_state()
    seen_ids = set(state.get("love_note_seen_block_ids", []) or [])
    new_blocks = [block for block in current_blocks if block["block_id"] not in seen_ids]
    if not seen_ids:
        return f"{target_date:%Y-%m-%d} 还没有增量基线；正式运行会先建立基线，不评论旧内容。"
    if not new_blocks:
        return f"{target_date:%Y-%m-%d} 恋爱笔记没有新增正文，不评论。"
    reactions = generate_love_note_reactions(
        new_blocks,
        target_date.strftime("%Y-%m-%d"),
        limit=_MAX_DAILY_LOVE_NOTE_COMMENTS,
    )
    return "\n".join(f"{item['block_id']}: {item['comment']}" for item in reactions) or "没有合适短评。"


def generate_love_note_reaction(document_text: str, date_key: str) -> str:
    source_text = _trim_document_source(document_text)
    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {
                "role": "system",
                "content": """你是三哥的小弟，负责在三哥和舒舒的飞书恋爱笔记里留下旁观嗑糖短评。
要求：
- 只输出一条短评论，不要标题、不要分节、不要列表、不要 Markdown
- 像读完内容后的自然评论：觉得甜、被可爱到、磕到了、提醒三哥珍惜
- 语气可以轻松一点，但不要油腻，不要长篇总结
- 舒舒和烨子是同一个人，称呼时二选一，不要并列
- 不要冒充三哥本人
- 输入是飞书文档正文，不是聊天消息列表
- 只能根据文档里已经写下来的内容发感想，不要编造未出现的事件
- 可以轻轻点到一个具体细节，但不要复述整篇
- 文档正文通常没有作者元数据；如果无法从文字本身判断是谁说的，不要强行归到舒舒或三哥名下
- 不要写“舒舒觉得/舒舒说/三哥说”这类归因，除非原文明确能支持；可以改写成“文档里写到”
- 评论长度控制在 60 到 120 个中文字符
- 不要出现“每日总结”“文档里记录的小事”“三哥该记得”等总结模板词""",
            },
            {
                "role": "user",
                "content": f"""日期：{date_key}

恋爱笔记正文：
{source_text}

请写一条适合挂在飞书文档评论里的嗑糖短评。""",
            },
        ],
        "temperature": 0.8,
        "max_tokens": 220,
    }
    resp = requests.post(
        f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
        headers=headers,
        json=payload,
        timeout=60,
    )
    resp.raise_for_status()
    return sanitize_public_text(resp.json()["choices"][0]["message"]["content"].strip())


def generate_love_note_reactions(blocks: list[dict], date_key: str, limit: int = 2) -> list[dict]:
    if limit <= 0:
        return []
    candidates = [
        {"block_id": block["block_id"], "text": block["text"][:220]}
        for block in blocks
        if block.get("block_id") and block.get("text")
    ]
    if not candidates:
        return []
    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {
                "role": "system",
                "content": """你是三哥的小弟，负责在三哥和舒舒的飞书恋爱笔记新增内容旁留下嗑糖短评。
要求：
- 只评论值得嗑糖或有关系感的新增段落；普通功能说明、无情绪内容可以不评论
- 最多输出用户要求的条数，可以少于上限
- 每条短评 35 到 90 个中文字符，不要标题、不要分节、不要 Markdown
- 评论要贴合对应原文，不能泛泛总结整篇
- 不要冒充三哥本人，不要过度油腻
- 舒舒和烨子是同一个人，称呼时二选一，不要并列
- 不要出现“每日总结”“文档里记录的小事”“三哥该记得”等总结模板词
- 只输出 JSON 数组，元素为 {"block_id":"...","comment":"..."}""",
            },
            {
                "role": "user",
                "content": (
                    f"日期：{date_key}\n"
                    f"最多评论 {limit} 条。\n"
                    "新增段落 JSON：\n"
                    f"{json.dumps(candidates, ensure_ascii=False)}"
                ),
            },
        ],
        "temperature": 0.75,
        "max_tokens": 700,
    }
    resp = requests.post(
        f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
        headers=headers,
        json=payload,
        timeout=60,
    )
    resp.raise_for_status()
    raw = resp.json()["choices"][0]["message"]["content"].strip()
    items = _loads_json_array(raw)
    valid_ids = {item["block_id"] for item in candidates}
    reactions = []
    for item in items:
        block_id = str(item.get("block_id", "")).strip()
        comment = sanitize_public_text(str(item.get("comment", "")).strip())
        if block_id in valid_ids and _is_acceptable_reaction(comment):
            reactions.append({"block_id": block_id, "comment": comment})
        if len(reactions) >= limit:
            break
    return reactions


def fetch_love_note_content() -> str:
    doc_token = LOVE_NOTE_DOC_TOKEN or resolve_wiki_doc_token(LOVE_NOTE_WIKI_TOKEN)
    if not doc_token:
        raise RuntimeError("缺少 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")
    return get_docx_raw_content(doc_token)


def add_love_note_comment(comment_text: str) -> dict:
    doc_token = LOVE_NOTE_DOC_TOKEN or resolve_wiki_doc_token(LOVE_NOTE_WIKI_TOKEN)
    if not doc_token:
        raise RuntimeError("缺少 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")
    anchor_block_id = pick_love_note_comment_anchor(doc_token, comment_text)
    return create_docx_comment(doc_token, anchor_block_id, comment_text)


def resolve_wiki_doc_token(wiki_token: str) -> str:
    if not wiki_token:
        return ""
    token = _tenant_token()
    resp = requests.get(
        f"{FEISHU_OPEN_API}/wiki/v2/spaces/get_node",
        headers={"Authorization": f"Bearer {token}"},
        params={"token": wiki_token},
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"解析 Wiki 节点失败: {data.get('msg')}")
    node = data.get("data", {}).get("node", {})
    if node.get("obj_type") != "docx":
        raise RuntimeError(f"Wiki 节点不是 docx: {node.get('obj_type')}")
    return node.get("obj_token", "")


def get_docx_raw_content(doc_token: str) -> str:
    token = _tenant_token()
    resp = requests.get(
        f"{FEISHU_OPEN_API}/docx/v1/documents/{doc_token}/raw_content",
        headers={"Authorization": f"Bearer {token}"},
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"读取文档正文失败: {data.get('msg')}")
    return data.get("data", {}).get("content", "")


def get_docx_blocks(doc_token: str) -> list[dict]:
    token = _tenant_token()
    resp = requests.get(
        f"{FEISHU_OPEN_API}/docx/v1/documents/{doc_token}/blocks",
        headers={"Authorization": f"Bearer {token}"},
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"读取文档块失败: {data.get('msg')}")
    return data.get("data", {}).get("items", [])


def pick_love_note_comment_anchor(doc_token: str, comment_text: str = "") -> str:
    """Pick the text block that best matches the short reaction comment."""
    candidates = _comment_anchor_candidates(get_docx_blocks(doc_token))
    if not candidates:
        raise RuntimeError("没有找到可挂评论的正文块")
    model_choice = _pick_anchor_with_deepseek(candidates, comment_text)
    if model_choice:
        return model_choice
    scored_choice = _pick_anchor_by_score(candidates, comment_text)
    if scored_choice:
        return scored_choice
    raise RuntimeError("没有找到可挂评论的正文块")


def create_docx_comment(doc_token: str, block_id: str, markdown_summary: str) -> dict:
    if not block_id:
        raise RuntimeError("缺少评论锚点 block_id")
    token = _tenant_token()
    resp = requests.post(
        f"{FEISHU_OPEN_API}/drive/v1/files/{doc_token}/new_comments",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        json={
            "file_type": "docx",
            "anchor": {"block_id": block_id},
            "reply_elements": _comment_text_elements(markdown_summary),
        },
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"创建文档评论失败: {data.get('msg')} {data.get('error')}")
    return data


def hide_love_note_comment(comment_id: str, reply_id: str = "", doc_token: str = "") -> dict:
    """Best-effort removal for bot comments: delete reply if possible, otherwise mark solved."""
    doc_token = doc_token or LOVE_NOTE_DOC_TOKEN or resolve_wiki_doc_token(LOVE_NOTE_WIKI_TOKEN)
    if not doc_token:
        raise RuntimeError("缺少 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")
    if reply_id:
        deleted = _delete_comment_reply(doc_token, comment_id, reply_id)
        if deleted.get("ok"):
            return deleted
    return _mark_comment_solved(doc_token, comment_id)


def get_docx_document(doc_token: str) -> dict:
    token = _tenant_token()
    resp = requests.get(
        f"{FEISHU_OPEN_API}/docx/v1/documents/{doc_token}",
        headers={"Authorization": f"Bearer {token}"},
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"读取文档失败: {data.get('msg')}")
    return data.get("data", {}).get("document", {})


def get_docx_root_child_count(doc_token: str) -> int:
    token = _tenant_token()
    resp = requests.get(
        f"{FEISHU_OPEN_API}/docx/v1/documents/{doc_token}/blocks",
        headers={"Authorization": f"Bearer {token}"},
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"读取文档块失败: {data.get('msg')}")
    items = data.get("data", {}).get("items", [])
    for item in items:
        if item.get("block_id") == doc_token:
            return len(item.get("children") or [])
    return 0


def create_docx_children(
    doc_token: str,
    block_id: str,
    blocks: list[dict],
    revision_id: int = -1,
    index: int = 0,
) -> dict:
    token = _tenant_token()
    resp = requests.post(
        f"{FEISHU_OPEN_API}/docx/v1/documents/{doc_token}/blocks/{block_id}/children",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        params={
            "document_revision_id": revision_id,
            "client_token": str(uuid.uuid4()),
        },
        json={"index": index, "children": blocks},
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"追加文档内容失败: {data.get('msg')} {data.get('error')}")
    return data


def markdown_to_docx_blocks(markdown: str) -> list[dict]:
    blocks = []
    for raw_line in (markdown or "").splitlines():
        line = raw_line.strip()
        if not line:
            continue
        if line.startswith("### "):
            blocks.append(_heading(2, line[4:].strip()))
        elif line.startswith("## "):
            blocks.append(_heading(1, line[3:].strip()))
        elif line.startswith("- "):
            blocks.append(_bullet(line[2:].strip()))
        elif line.startswith("> "):
            blocks.append(_paragraph("“" + line[2:].strip() + "”"))
        else:
            blocks.append(_paragraph(line))
    return blocks


def _heading(level: int, text: str) -> dict:
    block_type = 3 if level == 1 else 4
    key = "heading1" if level == 1 else "heading2"
    return {
        "block_type": block_type,
        key: {"elements": [_text_run(text)], "style": {}},
    }


def _paragraph(text: str) -> dict:
    return {
        "block_type": 2,
        "text": {"elements": [_text_run(text)], "style": {}},
    }


def _bullet(text: str) -> dict:
    return {
        "block_type": 12,
        "bullet": {"elements": [_text_run(text)], "style": {}},
    }


def _text_run(text: str) -> dict:
    return {
        "text_run": {
            "content": html.unescape(sanitize_public_text(text or "")),
            "text_element_style": {},
        }
    }


def _block_plain_text(block: dict) -> str:
    text = block.get("text") or block.get("heading1") or block.get("heading2") or {}
    pieces = []
    for element in text.get("elements") or []:
        run = element.get("text_run") or {}
        pieces.append(run.get("content") or "")
    return "".join(pieces)


def _text_block_candidates(blocks: list[dict]) -> list[dict]:
    candidates = []
    for block in blocks:
        text = sanitize_public_text(_block_plain_text(block)).strip()
        block_id = block.get("block_id", "")
        if not block_id or not text:
            continue
        candidates.append({
            "block_id": block_id,
            "text": text,
            "comment_ids": list(block.get("comment_ids") or []),
        })
    return candidates


def _mark_love_note_seen(state: dict, blocks: list[dict], document: dict | None = None) -> None:
    state["love_note_seen_block_ids"] = [block["block_id"] for block in blocks if block.get("block_id")]
    if document:
        state["love_note_last_revision_id"] = document.get("revision_id")


def _trim_daily_counts(counts: dict) -> dict:
    return dict(list(counts.items())[-14:])


def _loads_json_array(raw: str) -> list[dict]:
    text = (raw or "").strip()
    if text.startswith("```"):
        text = text.strip("`")
        if text.lower().startswith("json"):
            text = text[4:].strip()
    start = text.find("[")
    end = text.rfind("]")
    if start >= 0 and end >= start:
        text = text[start:end + 1]
    data = json.loads(text)
    return data if isinstance(data, list) else []


def _is_acceptable_reaction(comment: str) -> bool:
    text = sanitize_public_text(comment or "").strip()
    if not text:
        return False
    blocked = ["每日总结", "文档里记录的小事", "三哥该记得", "###", "##", "- "]
    if any(word in text for word in blocked):
        return False
    return 8 <= len(text) <= 140


def _comment_anchor_candidates(blocks: list[dict]) -> list[dict]:
    candidates = _comment_anchor_candidates_from_blocks(blocks, skip_commented=True)
    if candidates:
        return candidates
    return _comment_anchor_candidates_from_blocks(blocks, skip_commented=False)


def _comment_anchor_candidates_from_blocks(blocks: list[dict], skip_commented: bool) -> list[dict]:
    candidates = []
    for block in blocks:
        if skip_commented and block.get("comment_ids"):
            continue
        text = sanitize_public_text(_block_plain_text(block)).strip()
        block_id = block.get("block_id", "")
        if not block_id or not text:
            continue
        candidates.append({"block_id": block_id, "text": text})
    return candidates


def _pick_anchor_with_deepseek(candidates: list[dict], comment_text: str) -> str:
    if not candidates or not DEEPSEEK_API_KEY:
        return ""
    compact_candidates = [
        {"block_id": item["block_id"], "text": item["text"][:160]}
        for item in candidates[-40:]
    ]
    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {
                "role": "system",
                "content": "你负责给飞书恋爱笔记短评选择最适合挂评论的原文段落。只返回 JSON。",
            },
            {
                "role": "user",
                "content": (
                    "短评：\n"
                    f"{comment_text}\n\n"
                    "候选段落 JSON：\n"
                    f"{json.dumps(compact_candidates, ensure_ascii=False)}\n\n"
                    "请选择最能支撑这条短评、最值得嗑糖的段落。"
                    "只输出 {\"block_id\":\"...\"}，不要解释。"
                ),
            },
        ],
        "temperature": 0.2,
        "max_tokens": 80,
    }
    try:
        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers=headers,
            json=payload,
            timeout=30,
        )
        resp.raise_for_status()
        raw = resp.json()["choices"][0]["message"]["content"].strip()
        selected = json.loads(raw).get("block_id", "")
    except Exception:
        return ""
    valid_ids = {item["block_id"] for item in candidates}
    return selected if selected in valid_ids else ""


def _pick_anchor_by_score(candidates: list[dict], comment_text: str) -> str:
    sweet_terms = [
        "想你",
        "想和",
        "永远",
        "陪",
        "可爱",
        "萌",
        "孤独",
        "在干什么",
        "在干嘛",
        "一起",
        "早点睡",
        "误会",
        "喜欢",
        "得意",
    ]
    comment_terms = [term for term in sweet_terms if term in comment_text]
    best_id = ""
    best_score = -1
    for index, item in enumerate(candidates):
        text = item["text"]
        score = sum(3 for term in comment_terms if term in text)
        score += sum(1 for term in sweet_terms if term in text)
        # Mild recency bias, without forcing the last block.
        score += min(index, 20) / 100
        if score > best_score:
            best_id = item["block_id"]
            best_score = score
    return best_id


def _comment_text_elements(text: str) -> list[dict]:
    cleaned = sanitize_public_text(text or "").replace("<", "&lt;").replace(">", "&gt;").strip()
    if not cleaned:
        cleaned = "今天没有生成有效短评。"
    chunks = [cleaned[i:i + 900] for i in range(0, min(len(cleaned), 9000), 900)]
    return [{"type": "text", "text": chunk} for chunk in chunks]


def _delete_comment_reply(doc_token: str, comment_id: str, reply_id: str) -> dict:
    token = _tenant_token()
    resp = requests.delete(
        f"{FEISHU_OPEN_API}/drive/v1/files/{doc_token}/comments/{comment_id}/replys/{reply_id}",
        headers={"Authorization": f"Bearer {token}"},
        params={"file_type": "docx"},
        timeout=30,
    )
    data = resp.json()
    return {"ok": data.get("code") == 0, "data": data}


def _mark_comment_solved(doc_token: str, comment_id: str) -> dict:
    token = _tenant_token()
    resp = requests.patch(
        f"{FEISHU_OPEN_API}/drive/v1/files/{doc_token}/comments/{comment_id}",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        params={"file_type": "docx"},
        json={"is_solved": True},
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"隐藏评论失败: {data.get('msg')} {data.get('error')}")
    return {"ok": True, "data": data}


def _trim_document_source(document_text: str) -> str:
    lines = []
    skip_generated = False
    for raw_line in (document_text or "").splitlines():
        line = raw_line.strip()
        if line.startswith("每日总结 "):
            skip_generated = True
            continue
        if skip_generated and line.startswith("## "):
            skip_generated = False
        if skip_generated:
            continue
        lines.append(raw_line)
    text = "\n".join(lines).strip()
    if len(text) <= _MAX_DOCUMENT_SOURCE_CHARS:
        return text
    return text[-_MAX_DOCUMENT_SOURCE_CHARS:]


def _tenant_token() -> str:
    resp = requests.post(
        f"{FEISHU_OPEN_API}/auth/v3/tenant_access_token/internal",
        json={"app_id": FEISHU_APP_ID, "app_secret": FEISHU_APP_SECRET},
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"获取 tenant_access_token 失败: {data.get('msg')}")
    return data["tenant_access_token"]
