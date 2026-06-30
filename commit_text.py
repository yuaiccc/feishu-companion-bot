"""Commit message wording helpers for Feishu activity cards."""
from __future__ import annotations

import re


_TYPE_MAP = {
    "feat": "新增",
    "feature": "新增",
    "fix": "修复",
    "bugfix": "修复",
    "docs": "更新文档",
    "doc": "更新文档",
    "style": "整理样式",
    "refactor": "整理逻辑",
    "perf": "优化性能",
    "test": "补充测试",
    "tests": "补充测试",
    "chore": "维护配置",
    "build": "调整构建",
    "ci": "调整自动化",
    "revert": "回退改动",
}

_WORD_MAP = {
    "bot": "机器人",
    "persona": "人设",
    "fallback": "兜底",
    "summary": "总结",
    "summaries": "总结",
    "activity": "活动",
    "activities": "活动",
    "feishu": "飞书",
    "lark": "飞书",
    "message": "消息",
    "messages": "消息",
    "mention": "提到机器人",
    "mentions": "提到机器人",
    "handler": "处理逻辑",
    "handling": "处理逻辑",
    "local": "本地",
    "status": "状态",
    "alert": "提醒",
    "alerts": "提醒",
    "service": "服务",
    "launch": "启动",
    "restart": "重启",
    "poll": "轮询",
    "polling": "轮询",
    "github": "GitHub",
    "commit": "提交",
    "commits": "提交",
    "card": "卡片",
    "cards": "卡片",
    "table": "表格",
    "time": "时间",
    "field": "字段",
    "fields": "字段",
    "private": "私有",
    "chat": "聊天",
    "context": "上下文",
    "deepseek": "DeepSeek",
    "openapi": "开放接口",
    "correct": "修正",
    "timezone": "时区",
    "scheduler": "定时任务",
    "in": "里的",
    "add": "增加",
    "require": "要求",
    "refine": "调整",
    "harden": "加固",
    "event": "事件",
    "events": "事件",
    "stale": "过期",
    "recalled": "已撤回",
    "unavailable": "不可用",
    "window": "窗口",
    "app": "应用",
    "apps": "应用",
}

_PHRASE_MAP = {
    "harden feishu message handling": "加固飞书消息处理",
    "correct timezone handling in scheduler": "修复定时任务时区处理",
    "add local bot status alerts": "增加本地机器人状态提醒",
    "fix feishu bot mention matching": "修复飞书机器人提及识别",
    "keep local feishu bot online": "保持本地飞书机器人在线",
    "handle bare feishu bot mentions": "处理只提到机器人的飞书消息",
    "require deepseek summaries for activity cards": "活动卡片强制使用 DeepSeek 总结",
    "refine bot persona and fallback context": "调整机器人人设和兜底上下文",
}

_commit_text_cache: dict[str, str] = {}


def brief_commit_messages(messages: list[str], limit: int = 3) -> str:
    """Return a short Chinese-only description list for commit messages."""
    translated = []
    for message in messages:
        text = translate_commit_message(message)
        if text:
            translated.append(text)
        if len(translated) >= limit:
            break
    return "；".join(translated)


def translate_commit_message(message: str) -> str:
    """Translate a commit subject into plain Chinese for non-technical readers."""
    subject = (message or "").strip().split("\n")[0].strip()
    if not subject:
        return ""
    if subject in _commit_text_cache:
        return _commit_text_cache[subject]

    translated = _translate_with_deepseek(subject) or _translate_by_rules(subject)
    translated = _clean_result(translated)
    _commit_text_cache[subject] = translated
    return translated


def _translate_with_deepseek(subject: str) -> str:
    try:
        from config import DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL
    except Exception:
        return ""
    if not DEEPSEEK_API_KEY:
        return ""

    try:
        import requests

        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers={
                "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
                "Content-Type": "application/json",
            },
            json={
                "model": DEEPSEEK_MODEL,
                "messages": [
                    {
                        "role": "system",
                        "content": (
                            "把 Git commit 标题改写成普通人能看懂的中文短句。"
                            "不要保留英文技术前缀，不要输出引号，不超过24个字。"
                        ),
                    },
                    {"role": "user", "content": subject},
                ],
                "temperature": 0.1,
                "max_tokens": 60,
            },
            timeout=12,
        )
        resp.raise_for_status()
        return resp.json()["choices"][0]["message"]["content"].strip()
    except Exception:
        return ""


def _translate_by_rules(subject: str) -> str:
    lower = subject.lower().strip()
    if lower in _PHRASE_MAP:
        return _PHRASE_MAP[lower]

    match = re.match(r"^([a-zA-Z]+)(?:\([^)]+\))?!?:\s*(.+)$", subject)
    if match:
        prefix, rest = match.groups()
        action = _TYPE_MAP.get(prefix.lower(), "更新")
        rest_phrase = _PHRASE_MAP.get(rest.lower().strip())
        if rest_phrase:
            return rest_phrase
        return f"{action}{_translate_words(rest)}"

    words = _translate_words(subject)
    if re.search(r"[A-Za-z]", words):
        return f"更新{words}"
    return words


def _translate_words(text: str) -> str:
    normalized = re.sub(r"[_/\\-]+", " ", text)
    normalized = re.sub(r"\s+", " ", normalized).strip()
    if not normalized:
        return ""
    pieces = []
    for token in normalized.split(" "):
        clean = token.strip(".,:;!?()[]{}")
        if not clean:
            continue
        pieces.append(_WORD_MAP.get(clean.lower(), clean))
    return "".join(pieces)


def _clean_result(text: str) -> str:
    text = (text or "").strip().strip("\"'“”‘’")
    text = re.sub(r"\s+", "", text)
    if len(text) > 40:
        text = text[:37] + "..."
    return text
