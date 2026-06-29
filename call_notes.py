"""Feishu Minutes context reader.

Uses official Feishu Minutes APIs:
- GET /open-apis/minutes/v1/minutes/:minute_token
- GET /open-apis/minutes/v1/minutes/:minute_token/transcript

This module is intentionally optional. If tokens or permissions are missing,
it returns an empty context and never blocks chat replies.
"""
import os
from datetime import datetime, timezone, timedelta

import requests

OPEN_API = "https://open.feishu.cn/open-apis"
SHANGHAI = timezone(timedelta(hours=8))


def _enabled() -> bool:
    return os.getenv("CALL_NOTES_ENABLED", "false").lower() in ("true", "1", "yes")


def _minute_tokens() -> list[str]:
    raw = os.getenv("FEISHU_MINUTE_TOKENS", "")
    return [t.strip() for t in raw.split(",") if t.strip()]


def _max_chars() -> int:
    try:
        return max(500, int(os.getenv("CALL_NOTES_MAX_CHARS", "3000")))
    except ValueError:
        return 3000


def _get_tenant_token() -> str:
    app_id = os.getenv("FEISHU_APP_ID", "")
    app_secret = os.getenv("FEISHU_APP_SECRET", "")
    if not app_id or not app_secret:
        return ""

    resp = requests.post(
        f"{OPEN_API}/auth/v3/tenant_access_token/internal",
        json={"app_id": app_id, "app_secret": app_secret},
        timeout=20,
    )
    data = resp.json()
    return data.get("tenant_access_token", "")


def _headers(token: str) -> dict:
    return {"Authorization": f"Bearer {token}"}


def _format_ms(ms: str) -> str:
    try:
        dt = datetime.fromtimestamp(int(ms) / 1000, tz=timezone.utc)
        return dt.astimezone(SHANGHAI).strftime("%Y-%m-%d %H:%M")
    except Exception:
        return ""


def fetch_minute_info(minute_token: str, access_token: str) -> dict:
    resp = requests.get(
        f"{OPEN_API}/minutes/v1/minutes/{minute_token}",
        headers=_headers(access_token),
        params={"user_id_type": "open_id"},
        timeout=20,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"minute info failed: {data.get('code')} {data.get('msg')}")
    return data.get("data", {}).get("minute", {}) or {}


def fetch_minute_transcript(minute_token: str, access_token: str) -> str:
    resp = requests.get(
        f"{OPEN_API}/minutes/v1/minutes/{minute_token}/transcript",
        headers=_headers(access_token),
        params={
            "need_speaker": "true",
            "need_timestamp": "true",
            "file_format": "txt",
        },
        timeout=30,
    )
    if resp.status_code != 200:
        raise RuntimeError(f"minute transcript failed: http {resp.status_code}")
    return resp.content.decode("utf-8", errors="replace").strip()


def build_call_notes_context() -> str:
    """Return compact call/minutes context for DeepSeek, or empty string."""
    if not _enabled():
        return ""

    tokens = _minute_tokens()
    if not tokens:
        return ""

    access_token = _get_tenant_token()
    if not access_token:
        return ""

    sections = []
    remaining = _max_chars()
    for minute_token in tokens:
        if remaining <= 0:
            break
        try:
            info = fetch_minute_info(minute_token, access_token)
            transcript = fetch_minute_transcript(minute_token, access_token)
        except Exception as exc:
            print(f"  [call-notes] 读取妙记失败 token={minute_token[:8]}...: {exc}", flush=True)
            continue

        title = info.get("title") or "通话纪要"
        created = _format_ms(info.get("create_time", ""))
        prefix = f"--- 通话纪要：{title}"
        if created:
            prefix += f"（{created}）"
        prefix += " ---"

        body = transcript[:remaining]
        sections.append(f"{prefix}\n{body}")
        remaining -= len(body)

    if not sections:
        return ""
    return "\n\n".join(sections)
