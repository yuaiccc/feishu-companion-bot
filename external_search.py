"""External web search through the local OpenClaw CLI."""
import json
import os
import re
import shutil
import subprocess
from pathlib import Path
from urllib.parse import urlparse

import requests

from config import (
    DEEPSEEK_API_KEY,
    DEEPSEEK_BASE_URL,
    DEEPSEEK_MODEL,
    EXTERNAL_SEARCH_ENABLED,
    OPENCLAW_CLI,
    OPENCLAW_SEARCH_LIMIT,
    OPENCLAW_SEARCH_PROVIDER,
    OPENCLAW_SEARCH_TIMEOUT_SECONDS,
)
from text_safety import sanitize_public_text


_UNTRUSTED_MARKER_RE = re.compile(r"<<<(?:END_)?EXTERNAL_UNTRUSTED_CONTENT[^>]*>>>")
_NOISE_LINE_PREFIXES = (
    "[state-migrations]",
    "- Left plugin install index",
)


def _clean_external_text(text: str) -> str:
    text = _UNTRUSTED_MARKER_RE.sub("", text or "")
    text = text.replace("Source: Web Search", "")
    text = text.replace("---", " ")
    text = re.sub(r"\s+", " ", text).strip()
    return sanitize_public_text(text)


def _extract_json_object(output: str) -> dict:
    lines = []
    for line in output.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith(_NOISE_LINE_PREFIXES):
            continue
        lines.append(line)
    cleaned = "\n".join(lines).strip()
    start = cleaned.find("{")
    end = cleaned.rfind("}")
    if start < 0 or end < start:
        raise ValueError("OpenClaw did not return JSON")
    return json.loads(cleaned[start : end + 1])


def _result_items(payload: dict) -> list[dict]:
    items = []
    for output in payload.get("outputs", []):
        result = output.get("result") or {}
        for item in result.get("results") or []:
            title = _clean_external_text(item.get("title", ""))
            snippet = _clean_external_text(item.get("snippet", ""))
            url = (item.get("url") or "").strip()
            if title or snippet or url:
                items.append({"title": title, "snippet": snippet, "url": url})
    return items


def search_web(query: str, limit: int | None = None) -> list[dict]:
    """Run OpenClaw web search and return cleaned result items."""
    if not EXTERNAL_SEARCH_ENABLED:
        raise RuntimeError("外部搜索未开启")
    query = (query or "").strip()
    if not query:
        return []

    cli = _resolve_openclaw_cli()
    cmd = [
        cli,
        "infer",
        "web",
        "search",
        "--query",
        query,
        "--limit",
        str(limit or OPENCLAW_SEARCH_LIMIT),
        "--json",
    ]
    if OPENCLAW_SEARCH_PROVIDER:
        cmd.extend(["--provider", OPENCLAW_SEARCH_PROVIDER])

    env = os.environ.copy()
    cli_dir = str(Path(cli).parent)
    env["PATH"] = f"{cli_dir}:{env.get('PATH', '')}"

    proc = subprocess.run(
        cmd,
        text=True,
        capture_output=True,
        timeout=OPENCLAW_SEARCH_TIMEOUT_SECONDS,
        check=False,
        env=env,
    )
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip()
        raise RuntimeError(f"OpenClaw 搜索失败: {detail[:300]}")

    payload = _extract_json_object(proc.stdout)
    if not payload.get("ok"):
        raise RuntimeError("OpenClaw 搜索返回失败")
    return _result_items(payload)


def _resolve_openclaw_cli() -> str:
    if "/" in OPENCLAW_CLI and Path(OPENCLAW_CLI).exists():
        return OPENCLAW_CLI
    resolved = shutil.which(OPENCLAW_CLI)
    if resolved:
        return resolved

    candidates = []
    candidates.extend(Path.home().glob(".local/state/fnm_multishells/*/bin/openclaw"))
    candidates.extend([
        Path.home() / ".local/bin/openclaw",
        Path("/opt/homebrew/bin/openclaw"),
        Path("/usr/local/bin/openclaw"),
    ])
    existing = [p for p in candidates if p.exists() and p.is_file()]
    if existing:
        existing.sort(key=lambda p: p.stat().st_mtime, reverse=True)
        return str(existing[0])
    return OPENCLAW_CLI


def summarize_search_results(query: str, results: list[dict]) -> str:
    """Summarize search results with source awareness."""
    if not results:
        return "小弟没搜到靠谱结果，换个关键词再试试。"

    result_text = "\n".join(
        f"{idx}. 标题：{item.get('title')}\n摘要：{item.get('snippet')}\n链接：{item.get('url')}"
        for idx, item in enumerate(results, 1)
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
                "content": """你是三哥的小弟，帮舒舒或三哥整理外部搜索结果。
要求：
- 简短、可靠，不要把搜索结果当作绝对事实
- 明确说这是小弟搜到的结果
- 优先给结论，再列 2-4 个要点
- 保留来源链接，链接数量不要超过 4 个
- 群里称呼舒烨时只用舒舒或烨子
- 不要使用“微里”这个名字
- 不要编造搜索结果里没有的信息""",
            },
            {
                "role": "user",
                "content": f"搜索问题：{query}\n\n搜索结果：\n{result_text}",
            },
        ],
        "temperature": 0.3,
        "max_tokens": 500,
    }
    try:
        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers=headers,
            json=payload,
            timeout=30,
        )
        resp.raise_for_status()
        return sanitize_public_text(resp.json()["choices"][0]["message"]["content"].strip())
    except Exception:
        lines = ["小弟搜到这些结果，可以先参考："]
        for item in results[:4]:
            title = item.get("title") or item.get("url") or "搜索结果"
            snippet = item.get("snippet") or ""
            url = item.get("url") or ""
            line = f"- {title}"
            if snippet:
                line += f"：{snippet[:90]}"
            if url:
                line += f"\n  {url}"
            lines.append(line)
        return sanitize_public_text("\n".join(lines))


def answer_external_search(query: str) -> str:
    results = search_web(query)
    return summarize_search_results(query, results)


def summarize_search_intro(query: str, results: list[dict]) -> str:
    """Generate a short one-paragraph intro for a search card."""
    if not results:
        return "小弟没搜到靠谱结果，换个关键词再试试。"

    result_text = "\n".join(
        f"{idx}. {item.get('title')} | {item.get('snippet')}"
        for idx, item in enumerate(results[:5], 1)
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
                "content": """你是三哥的小弟，给搜索结果卡片写一句很短的开场。
要求：
- 只写 1-2 句，总共不超过 80 字
- 不要列 bullet，不要 markdown
- 说明这是小弟搜到的，不要当绝对事实
- 群里称呼舒烨时只用舒舒或烨子
- 不要使用“微里”这个名字""",
            },
            {"role": "user", "content": f"问题：{query}\n\n结果：\n{result_text}"},
        ],
        "temperature": 0.3,
        "max_tokens": 120,
    }
    try:
        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers=headers,
            json=payload,
            timeout=20,
        )
        resp.raise_for_status()
        return sanitize_public_text(resp.json()["choices"][0]["message"]["content"].strip())
    except Exception:
        return "小弟先搜到这些结果，热度和片单可能会变，舒舒可以挑感兴趣的点开看看。"


def build_search_card(query: str, results: list[dict], intro: str = "") -> dict:
    """Build a compact Feishu table card for web search results."""
    intro = sanitize_public_text(intro or summarize_search_intro(query, results))
    rows = []
    links = []
    for idx, item in enumerate(results[:5], 1):
        title = _shorten(item.get("title") or "搜索结果", 42)
        snippet = _shorten(item.get("snippet") or "打开来源看详情", 90)
        url = item.get("url") or ""
        host = _source_host(url)
        rows.append({
            "item": title,
            "reason": snippet,
            "source": host or f"来源{idx}",
        })
        if url:
            safe_title = title.replace("[", "").replace("]", "")
            links.append(f"{idx}. [{safe_title}]({url})")

    elements = []
    if intro:
        elements.append({"tag": "markdown", "content": intro})
    elements.append({
        "tag": "table",
        "columns": [
            {
                "data_type": "text",
                "name": "item",
                "display_name": "推荐",
                "horizontal_align": "left",
                "width": "30%",
            },
            {
                "data_type": "text",
                "name": "reason",
                "display_name": "看点",
                "horizontal_align": "left",
                "width": "auto",
            },
            {
                "data_type": "text",
                "name": "source",
                "display_name": "来源",
                "horizontal_align": "center",
                "width": "22%",
            },
        ],
        "rows": rows or [{"item": "暂无", "reason": "没有搜到靠谱结果", "source": "-"}],
        "row_height": "low",
        "header_style": {
            "background_style": "grey",
            "bold": True,
            "lines": 1,
        },
        "page_size": max(1, min(len(rows), 5)),
        "margin": "0px 0px 0px 0px",
    })
    if links:
        elements.append({
            "tag": "markdown",
            "content": "来源链接：\n" + "\n".join(links[:5]),
        })

    return {
        "msg_type": "interactive",
        "card": {
            "schema": "2.0",
            "config": {"update_multi": True},
            "header": {
                "title": {"tag": "plain_text", "content": "小弟搜到的近期推荐"},
                "template": "turquoise",
                "padding": "12px 12px 12px 12px",
            },
            "body": {
                "direction": "vertical",
                "padding": "12px 12px 12px 12px",
                "elements": elements,
            },
        },
    }


def build_external_search_card(query: str) -> dict:
    results = search_web(query)
    intro = summarize_search_intro(query, results)
    return build_search_card(query, results, intro)


def _shorten(text: str, limit: int) -> str:
    text = re.sub(r"\s+", " ", sanitize_public_text(text or "")).strip()
    if len(text) <= limit:
        return text
    return text[: max(0, limit - 1)].rstrip() + "…"


def _source_host(url: str) -> str:
    try:
        host = urlparse(url).netloc
    except Exception:
        return ""
    return host.replace("www.", "").replace("m.", "")
