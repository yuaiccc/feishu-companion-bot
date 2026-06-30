"""Daily love-note reaction posted as a comment on an existing Feishu Docx/Wiki."""
from __future__ import annotations

import html
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


def run_daily_love_note(target_date: datetime | None = None, force: bool = False) -> str:
    """React to the configured love-note document and comment on it."""
    target_date = target_date or datetime.now(_SHANGHAI)
    date_key = target_date.strftime("%Y-%m-%d")

    state = load_state()
    if not force and state.get("last_love_note_date") == date_key:
        return f"{date_key} 已经写入过恋爱笔记，跳过。"

    source_text = fetch_love_note_content()
    if not source_text.strip():
        return f"{date_key} 没有读到可评论的文档内容，跳过。"

    comment = generate_love_note_reaction(source_text, date_key)
    add_love_note_comment(comment)

    state["last_love_note_date"] = date_key
    save_state(state)
    return comment


def preview_daily_love_note(target_date: datetime | None = None) -> str:
    """Generate the daily love-note reaction without writing the document or state."""
    target_date = target_date or datetime.now(_SHANGHAI)
    source_text = fetch_love_note_content()
    if not source_text.strip():
        return f"{target_date:%Y-%m-%d} 没有读到可评论的文档内容。"
    return generate_love_note_reaction(source_text, target_date.strftime("%Y-%m-%d"))


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


def fetch_love_note_content() -> str:
    doc_token = LOVE_NOTE_DOC_TOKEN or resolve_wiki_doc_token(LOVE_NOTE_WIKI_TOKEN)
    if not doc_token:
        raise RuntimeError("缺少 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")
    return get_docx_raw_content(doc_token)


def add_love_note_comment(comment_text: str) -> dict:
    doc_token = LOVE_NOTE_DOC_TOKEN or resolve_wiki_doc_token(LOVE_NOTE_WIKI_TOKEN)
    if not doc_token:
        raise RuntimeError("缺少 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")
    anchor_block_id = pick_love_note_comment_anchor(doc_token)
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


def pick_love_note_comment_anchor(doc_token: str) -> str:
    """Pick the last non-empty text block so the daily comment stays contextual."""
    for block in reversed(get_docx_blocks(doc_token)):
        if _block_plain_text(block).strip():
            return block.get("block_id", "")
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


def _comment_text_elements(text: str) -> list[dict]:
    cleaned = sanitize_public_text(text or "").replace("<", "&lt;").replace(">", "&gt;").strip()
    if not cleaned:
        cleaned = "今天没有生成有效总结。"
    chunks = [cleaned[i:i + 900] for i in range(0, min(len(cleaned), 9000), 900)]
    return [{"type": "text", "text": chunk} for chunk in chunks]


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
