"""Memory audit card for reviewing long-term memory quality."""
from __future__ import annotations

from collections import Counter

from feishu_companion.memory import _is_low_value_memory, _load_all, _normalize_text


def build_memory_audit_card(audience: str = "owner") -> dict:
    """Build a Feishu card summarizing memory health and review candidates."""
    memories = [m for m in _load_all() if m.get("content")]
    stats = _audit_stats(memories)
    rows = _audit_rows(memories, audience=audience)
    return {
        "msg_type": "interactive",
        "card": {
            "schema": "2.0",
            "config": {"update_multi": True},
            "header": {
                "title": {"tag": "plain_text", "content": "记忆审计面板"},
                "template": "blue",
                "padding": "12px 12px 12px 12px",
            },
            "body": {
                "direction": "vertical",
                "padding": "12px 12px 12px 12px",
                "elements": [
                    {
                        "tag": "markdown",
                        "content": (
                            f"共 {stats['total']} 条记忆｜公开 {stats['public_to_target']}｜"
                            f"仅三哥 {stats['owner_only']}｜私密 {stats['private']}｜"
                            f"低置信 {stats['low_confidence']}｜疑似噪声 {stats['low_value']}｜"
                            f"疑似重复 {stats['duplicates']}"
                        ),
                    },
                    {
                        "tag": "table",
                        "columns": [
                            {
                                "data_type": "text",
                                "name": "type",
                                "display_name": "类型",
                                "width": "22%",
                            },
                            {
                                "data_type": "text",
                                "name": "memory",
                                "display_name": "记忆",
                                "width": "auto",
                            },
                            {
                                "data_type": "text",
                                "name": "action",
                                "display_name": "建议",
                                "width": "24%",
                            },
                        ],
                        "rows": rows or [{"type": "正常", "memory": "暂时没有明显需要处理的记忆", "action": "无需处理"}],
                        "row_height": "low",
                        "header_style": {"background_style": "grey", "bold": True, "lines": 1},
                        "page_size": min(max(len(rows), 1), 10),
                    },
                    {
                        "tag": "markdown",
                        "content": "提示：私密记忆不会在发给舒舒的回复里注入；删除/改写建议后续可以接按钮确认。",
                    },
                ],
            },
        },
    }


def _audit_stats(memories: list[dict]) -> dict:
    visibility = Counter(m.get("visibility", "unknown") for m in memories)
    duplicate_count = sum(len(group) - 1 for group in _duplicate_groups(memories) if len(group) > 1)
    return {
        "total": len(memories),
        "public_to_target": visibility.get("public_to_target", 0),
        "owner_only": visibility.get("owner_only", 0),
        "private": visibility.get("private", 0),
        "low_confidence": sum(1 for m in memories if float(m.get("confidence", 0.7) or 0) < 0.55),
        "low_value": sum(1 for m in memories if _is_low_value_memory(m.get("content", ""))),
        "duplicates": duplicate_count,
    }


def _audit_rows(memories: list[dict], audience: str = "owner") -> list[dict]:
    rows = []
    seen_duplicate_ids = set()
    for group in _duplicate_groups(memories):
        if len(group) <= 1:
            continue
        primary = group[0]
        seen_duplicate_ids.update(str(m.get("id")) for m in group)
        rows.append({
            "type": "疑似重复",
            "memory": _display_memory(primary, audience),
            "action": f"合并 {len(group)} 条",
        })

    for mem in memories:
        if str(mem.get("id")) in seen_duplicate_ids:
            continue
        content = mem.get("content", "")
        confidence = float(mem.get("confidence", 0.7) or 0)
        if mem.get("visibility") == "private":
            rows.append({"type": "私密", "memory": _display_memory(mem, audience), "action": "仅本地保留"})
        elif confidence < 0.55:
            rows.append({"type": "低置信", "memory": _display_memory(mem, audience), "action": "待确认"})
        elif _is_low_value_memory(content):
            rows.append({"type": "疑似噪声", "memory": _display_memory(mem, audience), "action": "建议删除"})
        if len(rows) >= 10:
            break
    return rows


def _duplicate_groups(memories: list[dict]) -> list[list[dict]]:
    groups: list[list[dict]] = []
    used = set()
    for idx, mem in enumerate(memories):
        if idx in used:
            continue
        key = _normalize_text(mem.get("content", ""))
        if not key:
            continue
        group = [mem]
        for other_idx in range(idx + 1, len(memories)):
            if other_idx in used:
                continue
            other_key = _normalize_text(memories[other_idx].get("content", ""))
            if other_key and (key == other_key or key in other_key or other_key in key):
                group.append(memories[other_idx])
                used.add(other_idx)
        if len(group) > 1:
            used.add(idx)
            groups.append(group)
    return groups


def _display_memory(mem: dict, audience: str) -> str:
    content = str(mem.get("content", ""))
    if mem.get("visibility") == "private" and audience != "owner":
        return "[私密记忆已隐藏]"
    return content[:90] + ("..." if len(content) > 90 else "")
