"""Public text sanitization before sending messages to Feishu."""
from __future__ import annotations

import json
from copy import deepcopy

_DISALLOWED_NICKNAME = "\u5fae\u91cc"


def sanitize_public_text(text: str) -> str:
    """Remove words that should not appear in user-visible bot output."""
    return (text or "").replace(_DISALLOWED_NICKNAME, "舒舒")


def sanitize_card(card: dict) -> dict:
    """Return a sanitized copy of a Feishu card payload."""
    return _sanitize_value(card)


def assert_public_text_clean(value) -> None:
    """Raise if a card/text payload still contains disallowed wording."""
    rendered = json.dumps(value, ensure_ascii=False) if not isinstance(value, str) else value
    if _DISALLOWED_NICKNAME in rendered:
        raise AssertionError("public output contains a disallowed nickname")


def _sanitize_value(value):
    if isinstance(value, str):
        return sanitize_public_text(value)
    if isinstance(value, list):
        return [_sanitize_value(item) for item in value]
    if isinstance(value, dict):
        return {key: _sanitize_value(item) for key, item in deepcopy(value).items()}
    return value
