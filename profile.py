"""Profile configuration for reusable companion-bot deployments."""
from __future__ import annotations

import json
from functools import lru_cache
from pathlib import Path

from config import BASE_DIR, PROFILE_ID


_PROFILE_DIR = BASE_DIR / "profiles"
_DEFAULT_PROFILE_ID = "default"


@lru_cache(maxsize=1)
def load_profile() -> dict:
    profile_path = _PROFILE_DIR / f"{PROFILE_ID}.json"
    if not profile_path.exists():
        profile_path = _PROFILE_DIR / f"{_DEFAULT_PROFILE_ID}.json"
    with open(profile_path, "r", encoding="utf-8") as f:
        profile = json.load(f)
    profile.setdefault("id", profile_path.stem)
    return profile


def profile_id() -> str:
    return str(load_profile().get("id") or PROFILE_ID or _DEFAULT_PROFILE_ID)


def owner_name() -> str:
    return _person_name("owner", "用户")


def target_name() -> str:
    return _person_name("target", "对方")


def bot_role() -> str:
    return str(load_profile().get("bot", {}).get("role", "陪伴机器人"))


def target_addressing_instruction() -> str:
    aliases = load_profile().get("people", {}).get("target", {}).get("aliases", [])
    if len(aliases) >= 2:
        names = "、".join(f"\"{name}\"" for name in aliases[:3])
        return f"直接称呼对方时在 {names} 里任选一个；这些是同一个人，不要把两个名字并列说出来。"
    if aliases:
        return f"直接称呼对方时叫\"{aliases[0]}\"。"
    return "直接称呼对方时使用 profile 里的目标称呼。"


def relationship_context() -> str:
    profile = load_profile()
    lines = ["【背景信息（仅在相关时自然融入，不要每次都提）】"]
    for item in profile.get("relationship_context", []):
        lines.append(f"- {item}")
    lines.extend([
        f"- 机器人身份：{bot_role()}",
        f"- {target_addressing_instruction()}",
    ])
    return "\n".join(lines)


def memory_category_keywords() -> dict[str, tuple[str, ...]]:
    profile = load_profile()
    configured = profile.get("memory", {}).get("category_keywords", {})
    return {
        category: tuple(words)
        for category, words in configured.items()
        if isinstance(words, list)
    }


def _person_name(key: str, fallback: str) -> str:
    person = load_profile().get("people", {}).get(key, {})
    return str(person.get("display_name") or person.get("name") or fallback)
