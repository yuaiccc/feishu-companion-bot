"""External web search through local DeerFlow or OpenClaw."""
import hashlib
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
    DEERFLOW_BACKEND_DIR,
    DEERFLOW_PYTHON,
    DEERFLOW_SEARCH_THREAD_PREFIX,
    DEERFLOW_SEARCH_TIMEOUT_SECONDS,
    EXTERNAL_SEARCH_BACKEND,
    EXTERNAL_SEARCH_ENABLED,
    EXTERNAL_SEARCH_FALLBACK_OPENCLAW,
    OPENCLAW_CLI,
    OPENCLAW_SEARCH_LIMIT,
    OPENCLAW_SEARCH_PROVIDER,
    OPENCLAW_SEARCH_TIMEOUT_SECONDS,
)
from profile import bot_role, owner_name, target_addressing_instruction, target_name
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
    """Run the configured local web search backend and return cleaned result items."""
    if not EXTERNAL_SEARCH_ENABLED:
        raise RuntimeError("外部搜索未开启")
    query = (query or "").strip()
    if not query:
        return []

    backend = EXTERNAL_SEARCH_BACKEND or "deerflow"
    if backend not in ("deerflow", "openclaw", "auto"):
        raise RuntimeError(f"未知外部搜索后端: {backend}")

    if backend in ("deerflow", "auto"):
        try:
            return search_deerflow(query, limit=limit)
        except Exception as e:
            if backend == "deerflow" and not EXTERNAL_SEARCH_FALLBACK_OPENCLAW:
                raise
            print(f"  [external-search] DeerFlow 搜索失败，回退 OpenClaw: {e}", flush=True)

    return search_openclaw(query, limit=limit)


def search_openclaw(query: str, limit: int | None = None) -> list[dict]:
    """Run OpenClaw web search and return cleaned result items."""
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


def search_deerflow(query: str, limit: int | None = None) -> list[dict]:
    """Ask local DeerFlow to perform a web-aware research pass.

    DeerFlow returns a synthesized answer instead of a raw search-result list, so the
    adapter exposes that answer as one primary result and extracts cited URLs into
    secondary rows for Feishu table cards.
    """
    query = (query or "").strip()
    if not query:
        return []

    backend_dir = Path(DEERFLOW_BACKEND_DIR).expanduser()
    python = _resolve_deerflow_python(backend_dir)
    if not backend_dir.exists():
        raise RuntimeError(f"DeerFlow 后端目录不存在: {backend_dir}")
    if not Path(python).exists():
        raise RuntimeError(f"DeerFlow Python 不存在: {python}")

    prompt = _build_deerflow_search_prompt(query, limit or OPENCLAW_SEARCH_LIMIT)
    thread_id = _deerflow_thread_id(query)
    env = os.environ.copy()
    harness_dir = backend_dir / "packages" / "harness"
    env["PYTHONPATH"] = f"{backend_dir}:{harness_dir}:{env.get('PYTHONPATH', '')}"
    env["DEERFLOW_SEARCH_PROMPT"] = prompt
    env["DEERFLOW_SEARCH_THREAD_ID"] = thread_id
    script = """
import json
import os
from deerflow.client import DeerFlowClient

client = DeerFlowClient(thinking_enabled=False, subagent_enabled=True)
answer = client.chat(
    os.environ["DEERFLOW_SEARCH_PROMPT"],
    thread_id=os.environ["DEERFLOW_SEARCH_THREAD_ID"],
)
print(json.dumps({"answer": answer}, ensure_ascii=False))
"""
    proc = subprocess.run(
        [python, "-c", script],
        cwd=str(backend_dir),
        text=True,
        capture_output=True,
        timeout=DEERFLOW_SEARCH_TIMEOUT_SECONDS,
        check=False,
        env=env,
    )
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip()
        raise RuntimeError(f"DeerFlow 搜索失败: {detail[:300]}")

    payload = _extract_json_object(proc.stdout)
    answer = _clean_external_text(payload.get("answer", ""))
    if not answer:
        raise RuntimeError("DeerFlow 没有返回有效搜索结论")
    return _deerflow_result_items(answer, limit or OPENCLAW_SEARCH_LIMIT)


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


def _resolve_deerflow_python(backend_dir: Path) -> str:
    configured = Path(DEERFLOW_PYTHON).expanduser()
    if configured.exists():
        return str(configured)
    local_python = backend_dir / ".venv/bin/python"
    if local_python.exists():
        return str(local_python)
    return shutil.which("python3") or "python3"


def _build_deerflow_search_prompt(query: str, limit: int) -> str:
    return f"""请联网检索并整理这个问题：{query}

要求：
- 优先使用公开网页、官方来源或社区来源，不要凭空补全。
- 输出中文，先给 1 段简短结论，再列 {min(limit, 5)} 条以内来源线索。
- 每条来源线索保留标题、要点和 URL。
- 如果无法联网或没有可靠结果，请明确说明。"""


def _deerflow_thread_id(query: str) -> str:
    digest = hashlib.sha1(query.encode("utf-8")).hexdigest()[:12]
    return f"{DEERFLOW_SEARCH_THREAD_PREFIX}-{digest}"


def _deerflow_result_items(answer: str, limit: int) -> list[dict]:
    urls = _extract_urls(answer)
    items = [{
        "title": "DeerFlow 本地调研结论",
        "snippet": _shorten(answer, 240),
        "url": "",
        "provider": "deerflow",
    }]
    for url in urls[: max(0, limit - 1)]:
        items.append({
            "title": _source_host(url) or "DeerFlow 提到的来源",
            "snippet": "DeerFlow 调研过程中提到的来源链接。",
            "url": url,
            "provider": "deerflow",
        })
    return items[: max(1, limit)]


def _extract_urls(text: str) -> list[str]:
    seen = set()
    urls = []
    for match in re.finditer(r"https?://[^\s)\]}>\"'，。；、]+", text or ""):
        url = match.group(0).rstrip(".,;:!?")
        if url and url not in seen:
            seen.add(url)
            urls.append(url)
    return urls


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
                "content": f"""你是{bot_role()}，帮{target_name()}或{owner_name()}整理外部搜索结果。
要求：
- 简短、可靠，不要把搜索结果当作绝对事实
- 明确说这是机器人搜到的结果
- 优先给结论，再列 2-4 个要点
- 保留来源链接，链接数量不要超过 4 个
- {target_addressing_instruction()}
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
    if any(item.get("provider") == "deerflow" for item in results):
        return "小弟让本地 DeerFlow 做了一轮联网调研，先把结论和来源线索列成表。"
    return "小弟搜到这些相关结果，热度和片单可能会变，先按来源列成表给你看。"


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


def remember_search_interaction(query: str, results: list[dict], actor: str = "用户") -> None:
    """Store a compact interest memory from a successful search."""
    try:
        from memory import add_manual_memory
    except Exception:
        return
    topic = _search_topic(query)
    if not topic:
        return
    titles = []
    for item in results[:3]:
        title = _clean_external_text(item.get("title", ""))
        if title:
            titles.append(title[:30])
    suffix = f"；相关来源包括：{'、'.join(titles)}" if titles else ""
    fact = f"{actor}对“{topic}”感兴趣，曾让机器人搜索过这个主题{suffix}。"
    try:
        add_manual_memory(fact, category="preference", visibility="public_to_target", source_type="external_search")
    except Exception as e:
        print(f"  [search-memory] 写入搜索记忆失败: {e}", flush=True)


def _search_topic(query: str) -> str:
    text = sanitize_public_text(re.sub(r"\s+", " ", query or "").strip())
    text = re.sub(r"^(帮我|给我|你)?(搜索|搜一下|查一下|查查|找一下)", "", text)
    text = re.sub(r"(吗|呢|吧|？|\\?)$", "", text).strip()
    if len(text) < 3:
        return ""
    return _shorten(text, 36)


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
