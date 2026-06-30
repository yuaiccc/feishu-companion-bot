"""Daily love-note summary appended to an existing Feishu Docx/Wiki document."""
from __future__ import annotations

import html
import json
import uuid
from datetime import datetime, time as dtime, timedelta, timezone

import requests

from config import (
    DEEPSEEK_API_KEY,
    DEEPSEEK_BASE_URL,
    DEEPSEEK_MODEL,
    FEISHU_APP_ID,
    FEISHU_APP_SECRET,
    FEISHU_OPEN_API,
    LOVE_NOTE_DOC_TOKEN,
    LOVE_NOTE_MESSAGE_LIMIT,
    LOVE_NOTE_RUN_AT,
    LOVE_NOTE_WIKI_TOKEN,
)
from feishu_api import fetch_chat_messages
from state import load_state, save_state
from text_safety import sanitize_public_text


_SHANGHAI = timezone(timedelta(hours=8))


def run_daily_love_note(target_date: datetime | None = None, force: bool = False) -> str:
    """Summarize today's chat and append it to the configured love note."""
    target_date = target_date or datetime.now(_SHANGHAI)
    date_key = target_date.strftime("%Y-%m-%d")

    state = load_state()
    if not force and state.get("last_love_note_date") == date_key:
        return f"{date_key} 已经写入过恋爱笔记，跳过。"

    messages = _today_messages(target_date)
    if not messages:
        return f"{date_key} 没有读到可总结的聊天内容，跳过。"

    summary = summarize_love_day(messages, date_key)
    append_love_note(summary)

    state["last_love_note_date"] = date_key
    save_state(state)
    return summary


def summarize_love_day(messages: list[dict], date_key: str) -> str:
    chat_text = "\n".join(
        f"[{m.get('time')}] {m.get('sender')}: {m.get('content')}"
        for m in messages
    )
    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {
                "role": "system",
                "content": """你是三哥的小弟，负责把三哥和舒舒当天的聊天整理成恋爱笔记。
要求：
- 输出 Markdown，不要代码块
- 温柔、真实，不要过度腻
- 舒舒和烨子是同一个人，称呼时二选一，不要并列
- 不要冒充三哥本人
- 保留当天有画面感的一两句话
- 不要编造聊天里没有的事情
- “舒舒说的话”只能引用当天聊天里 sender 为舒舒的原文；如果没有舒舒文字消息，写“今天没有可引用的舒舒文字消息”
- 不要把三哥转述、猜测或表情包脑补成舒舒说的话""",
            },
            {
                "role": "user",
                "content": f"""日期：{date_key}

当天聊天：
{chat_text}

请按这个结构输出：
## {date_key}

### 今天的小事
- ...

### 舒舒说的话
> ...

### 三哥该记得
- ...

### 今天的总结
...""",
            },
        ],
        "temperature": 0.6,
        "max_tokens": 900,
    }
    resp = requests.post(
        f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
        headers=headers,
        json=payload,
        timeout=60,
    )
    resp.raise_for_status()
    return sanitize_public_text(resp.json()["choices"][0]["message"]["content"].strip())


def append_love_note(markdown_summary: str) -> dict:
    doc_token = LOVE_NOTE_DOC_TOKEN or resolve_wiki_doc_token(LOVE_NOTE_WIKI_TOKEN)
    if not doc_token:
        raise RuntimeError("缺少 LOVE_NOTE_DOC_TOKEN 或 LOVE_NOTE_WIKI_TOKEN")
    document = get_docx_document(doc_token)
    revision_id = int(document.get("revision_id", -1))
    append_index = get_docx_root_child_count(doc_token)
    blocks = markdown_to_docx_blocks(markdown_summary)
    return create_docx_children(doc_token, doc_token, blocks, revision_id=revision_id, index=append_index)


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


def _today_messages(target_date: datetime) -> list[dict]:
    start = datetime.combine(target_date.date(), dtime.min, tzinfo=_SHANGHAI)
    end = start + timedelta(days=1)
    start_ms = int(start.timestamp() * 1000)
    end_ms = int(end.timestamp() * 1000)
    messages = fetch_chat_messages(limit=LOVE_NOTE_MESSAGE_LIMIT)
    filtered = [
        m for m in messages
        if start_ms <= int(m.get("timestamp") or 0) < end_ms
    ]
    return list(reversed(filtered))


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
