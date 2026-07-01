"""Feishu Minutes context reader.

Uses official Feishu Minutes APIs:
- GET /open-apis/minutes/v1/minutes/:minute_token
- GET /open-apis/minutes/v1/minutes/:minute_token/transcript

This module is intentionally optional. If tokens or permissions are missing,
it returns an empty context and never blocks chat replies.
"""
import os
import json
import hashlib
from datetime import datetime, timezone, timedelta
from pathlib import Path

import requests
from profile import owner_name, target_name
from text_safety import sanitize_public_text

OPEN_API = "https://open.feishu.cn/open-apis"
SHANGHAI = timezone(timedelta(hours=8))
BASE_DIR = Path(__file__).resolve().parent


def _enabled() -> bool:
    return os.getenv("CALL_NOTES_ENABLED", "false").lower() in ("true", "1", "yes")


def _minute_tokens() -> list[str]:
    raw = os.getenv("FEISHU_MINUTE_TOKENS", "")
    return [t.strip() for t in raw.split(",") if t.strip()]


def _max_chars() -> int:
    try:
        return max(300, int(os.getenv("CALL_NOTES_MAX_CHARS", "1200")))
    except ValueError:
        return 1200


def _summary_max_chars() -> int:
    try:
        return max(200, int(os.getenv("CALL_NOTES_SUMMARY_MAX_CHARS", "700")))
    except ValueError:
        return 700


def _cache_file() -> Path:
    return BASE_DIR / os.getenv("CALL_NOTES_CACHE_FILE", "call_notes_cache.json")


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


def _load_cache() -> dict:
    try:
        with _cache_file().open("r", encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return {}


def _save_cache(cache: dict) -> None:
    try:
        with _cache_file().open("w", encoding="utf-8") as f:
            json.dump(cache, f, ensure_ascii=False, indent=2)
    except Exception as exc:
        print(f"  [call-notes] 写入摘要缓存失败: {exc}", flush=True)


def _cache_key(minute_token: str, transcript: str) -> str:
    raw = f"{minute_token}\n{transcript}".encode("utf-8", errors="ignore")
    return hashlib.sha256(raw).hexdigest()


def _summarize_transcript(title: str, created: str, transcript: str) -> str:
    summary = _summarize_transcript_with_deepseek(title, created, transcript)
    if not summary:
        summary = _fallback_summarize_transcript(transcript)
    summary = sanitize_public_text(summary)
    limit = _summary_max_chars()
    if len(summary) > limit:
        summary = summary[: limit - 1] + "…"
    return summary


def _summarize_transcript_with_deepseek(title: str, created: str, transcript: str) -> str:
    api_key = os.getenv("DEEPSEEK_API_KEY", "")
    if not api_key:
        return ""
    base_url = os.getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com").rstrip("/")
    model = os.getenv("DEEPSEEK_MODEL", "deepseek-chat")

    clipped = transcript[: min(len(transcript), 8000)]
    owner = owner_name()
    target = target_name()
    try:
        resp = requests.post(
            f"{base_url}/v1/chat/completions",
            headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
            json={
                "model": model,
                "messages": [
                    {
                        "role": "system",
                        "content": f"""你在整理通话纪要给{owner}本人作回复参考。
只抽取长期有用的关系上下文，不要写成给{target}看的话，不要煽情，不要暴露"读取了纪要"。
输出 3-5 条短要点，尽量包含：
- {target}最近在意/担心/开心的事
- {owner}答应过或应该记得的事
- 相处偏好、雷点、称呼习惯
不要编造，不确定就不写。""",
                    },
                    {
                        "role": "user",
                        "content": f"标题: {title}\n时间: {created}\n\n通话文字:\n{clipped}",
                    },
                ],
                "temperature": 0.2,
                "max_tokens": 350,
            },
            timeout=30,
        )
        resp.raise_for_status()
        return resp.json()["choices"][0]["message"]["content"].strip()
    except Exception as exc:
        print(f"  [call-notes] DeepSeek 摘要失败: {exc}", flush=True)
        return ""


def _fallback_summarize_transcript(transcript: str) -> str:
    owner = owner_name()
    target = target_name()
    keywords = (
        owner, target, "想你", "爱你", "开心", "难过", "委屈",
        "生气", "担心", "害怕", "记得", "答应", "约定", "晚安", "抱",
        "贴贴", "电话", "见面", "学校", "家里",
    )
    lines = []
    for raw in transcript.splitlines():
        line = raw.strip()
        if not line:
            continue
        if any(keyword in line for keyword in keywords):
            lines.append(line)
        if len(lines) >= 6:
            break
    if not lines:
        return ""
    text = "\n".join(f"- {line[:120]}" for line in lines)
    return f"从最近通话里可用的关系上下文：\n{text}"


def build_call_notes_context() -> str:
    """Return compact summarized call/minutes context for DeepSeek, or empty string."""
    if not _enabled():
        return ""

    tokens = _minute_tokens()
    if not tokens:
        return ""

    access_token = _get_tenant_token()
    if not access_token:
        return ""

    sections = []
    cache = _load_cache()
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
        key = _cache_key(minute_token, transcript)
        summary = cache.get(key)
        if not summary:
            summary = _summarize_transcript(title, created, transcript)
            if summary:
                cache[key] = summary
        if not summary:
            continue

        prefix = f"--- 通话摘要：{title}"
        if created:
            prefix += f"（{created}）"
        prefix += " ---"

        body = summary[:remaining]
        sections.append(f"{prefix}\n{body}")
        remaining -= len(body)

    if not sections:
        return ""
    _save_cache(cache)
    return "\n\n".join(sections)
