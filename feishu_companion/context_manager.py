"""Context budgeting for LLM calls.

This module decides which local signals are stitched into each model request.
It keeps the prompt construction explicit, bounded, and auditable.
"""
from dataclasses import dataclass

from feishu_companion.config import (
    CONTEXT_CALL_NOTES_MAX_CHARS,
    CONTEXT_CHAT_MAX_CHARS,
    CONTEXT_MAX_CHARS,
    CONTEXT_MEMORY_MAX_CHARS,
)
from feishu_companion.text_safety import sanitize_public_text


@dataclass
class ContextBundle:
    text: str
    stats: dict


def _clip(text: str, max_chars: int, keep_tail: bool = False) -> str:
    if not text or max_chars <= 0:
        return ""
    text = str(text).strip()
    if len(text) <= max_chars:
        return text
    if max_chars <= 20:
        return text[:max_chars]
    if keep_tail:
        return "... " + text[-(max_chars - 4):]
    return text[:max_chars - 4] + " ..."


def _format_message(m: dict) -> str:
    return f"  [{m.get('time', '')}] {m.get('sender', '')}说: {m.get('content', '')}"


def _recent_chat_block(messages: list[dict], max_chars: int) -> tuple[str, int]:
    """Keep the latest messages, then restore chronological order."""
    if not messages or max_chars <= 0:
        return "", 0

    selected = []
    used = 0
    for msg in reversed(messages):
        line = sanitize_public_text(_format_message(msg))
        line = _clip(line, max_chars, keep_tail=True)
        cost = len(line) + 1
        if selected and used + cost > max_chars:
            break
        selected.append(line)
        used += cost
        if used >= max_chars:
            break

    selected.reverse()
    return "\n".join(selected), len(selected)


def _memory_block(memories: list[str], max_chars: int) -> tuple[str, int]:
    if not memories or max_chars <= 0:
        return "", 0

    lines = []
    used = 0
    for mem in memories:
        line = f"  - {sanitize_public_text(_clip(str(mem), 260))}"
        cost = len(line) + 1
        if lines and used + cost > max_chars:
            break
        lines.append(line)
        used += cost
        if used >= max_chars:
            break

    return "\n".join(lines), len(lines)


def _bounded_call_notes(call_notes_context: str, max_chars: int) -> tuple[str, int]:
    if not call_notes_context or max_chars <= 0:
        return "", 0
    clipped = sanitize_public_text(_clip(call_notes_context, max_chars))
    return clipped, len(clipped)


def build_reply_context(
    messages: list[dict],
    memories: list[str] | None = None,
    call_notes_context: str = "",
) -> ContextBundle:
    chat_text, chat_count = _recent_chat_block(messages, CONTEXT_CHAT_MAX_CHARS)
    memory_text, memory_count = _memory_block(memories or [], CONTEXT_MEMORY_MAX_CHARS)
    notes_text, notes_chars = _bounded_call_notes(call_notes_context, CONTEXT_CALL_NOTES_MAX_CHARS)

    sections = []
    if chat_text:
        sections.append(("最近对话", chat_text))
    if memory_text:
        sections.append(("相关记忆", memory_text))
    if notes_text:
        sections.append((
            "重要通话纪要上下文",
            notes_text + "\n\n这些通话纪要是关系里的重要信息源。只在相关时自然使用，不要暴露为'我读取了纪要'。",
        ))

    text_parts = []
    used = 0
    included_sections = []
    for title, body in sections:
        block = f"--- {title} ---\n{body}"
        cost = len(block) + 2
        if text_parts and used + cost > CONTEXT_MAX_CHARS:
            continue
        if cost > CONTEXT_MAX_CHARS:
            block = _clip(block, CONTEXT_MAX_CHARS, keep_tail=(title == "最近对话"))
            cost = len(block)
        text_parts.append(block)
        included_sections.append(title)
        used += cost

    stats = {
        "sections": included_sections,
        "chat_messages": chat_count,
        "memories": memory_count,
        "call_notes_chars": notes_chars,
        "chars": len("\n\n".join(text_parts)),
    }
    return ContextBundle(text="\n\n".join(text_parts), stats=stats)


def log_context_stats(kind: str, bundle: ContextBundle) -> None:
    stats = bundle.stats
    print(
        "  [上下文] "
        f"{kind}: sections={','.join(stats.get('sections', [])) or '-'} "
        f"chat={stats.get('chat_messages', 0)} "
        f"memories={stats.get('memories', 0)} "
        f"notes_chars={stats.get('call_notes_chars', 0)} "
        f"chars={stats.get('chars', 0)}",
        flush=True,
    )
