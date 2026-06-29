"""飞书消息模块：构建消息卡片（含 commit 表格），dry_run 模式下只打印不发送。"""
import json
from datetime import datetime, timezone, timedelta

import requests
from config import FEISHU_WEBHOOK_URL, DRY_RUN

_SHANGHAI = timezone(timedelta(hours=8))


def build_message(activities: list[dict], summary: str = "") -> dict:
    """构建飞书交互式卡片消息：commit 表格 + 统计。summary 可选。"""
    repo_set = {a["repo"] for a in activities if a["repo"]}
    repo_count = len(repo_set)
    activity_count = len(activities)

    table_md = _build_table(activities)
    header_note = f"📊 本次共 {activity_count} 条活动 | 涉及 {repo_count} 个仓库"

    elements = []
    if summary:
        elements.append({
            "tag": "div",
            "text": {"tag": "lark_md", "content": summary},
        })
        elements.append({"tag": "hr"})

    elements.append({
        "tag": "div",
        "text": {"tag": "lark_md", "content": table_md},
    })
    elements.append({"tag": "hr"})
    elements.append({
        "tag": "note",
        "elements": [
            {"tag": "plain_text", "content": header_note},
        ],
    })

    return {
        "msg_type": "interactive",
        "card": {
            "config": {"wide_screen_mode": True},
            "header": {
                "title": {
                    "tag": "plain_text",
                    "content": "三哥的 GitHub 进度汇报",
                },
                "template": "turquoise",
            },
            "elements": elements,
        },
    }


def _build_table(activities: list[dict]) -> str:
    """用飞书 lark_md 的 markdown 表语法构建 commit 表格。"""
    rows = ["| 时间 | 仓库 | 内容 |", "| --- | --- | --- |"]
    for a in activities:
        time_str = _format_time(a["created_at"])
        repo = _short_repo(a["repo"])
        content = _format_content(a)
        # 转义可能的管道符
        content = content.replace("|", "\\|")
        rows.append(f"| {time_str} | {repo} | {content} |")
    return "\n".join(rows)


def _format_time(iso_str: str) -> str:
    """把 ISO 时间转成上海时区的 MM-DD HH:MM。"""
    try:
        dt = datetime.fromisoformat(iso_str.replace("Z", "+00:00"))
        return dt.astimezone(_SHANGHAI).strftime("%m-%d %H:%M")
    except Exception:
        return iso_str


def _short_repo(full: str) -> str:
    """yuaiccc/project-history → project-history"""
    return full.split("/")[-1] if "/" in full else full


def _format_content(a: dict) -> str:
    """把单条活动格式化成表格里的一句通俗描述。"""
    detail = a["detail"]
    atype = a["type"]

    if atype == "PushEvent":
        msgs = detail.get("commit_messages", [])
        count = detail.get("commit_count", len(msgs) if msgs else 1)
        brief = _brief_messages(msgs)
        if brief:
            return f"提交 {count} 次: {brief}"
        branch = detail.get("branch", "")
        if branch:
            return f"提交代码到 {branch}"
        return "提交代码"
    if atype == "PullRequestEvent":
        return f"{_zh_action(detail.get('action', ''))}PR: {detail.get('title', '')}"
    if atype == "IssuesEvent":
        return f"{_zh_action(detail.get('action', ''))}Issue: {detail.get('title', '')}"
    if atype == "CreateEvent":
        rtype = detail.get("ref_type", "")
        rname = detail.get("ref", "")
        if rtype == "repository":
            return "新建仓库"
        return f"创建{_zh_ref(rtype)} {rname}"
    if atype == "IssueCommentEvent":
        body = detail.get("body", "")
        return f"评论: {body[:40]}"
    if atype == "WatchEvent":
        return "Star 收藏"
    if atype == "ForkEvent":
        return f"Fork 到 {detail.get('forked_to', '')}"
    if atype == "ReleaseEvent":
        return f"发布版本 {detail.get('tag', '')}"
    return atype


def _brief_messages(msgs: list[str]) -> str:
    """把多条 commit message 合并成简短描述。"""
    if not msgs:
        return ""
    cleaned = []
    for m in msgs:
        m = m.strip().split("\n")[0]
        if len(m) > 40:
            m = m[:37] + "..."
        cleaned.append(m)
    return "; ".join(cleaned)


def _zh_action(action: str) -> str:
    return {
        "opened": "新建",
        "closed": "关闭",
        "reopened": "重新打开",
        "merged": "合并",
    }.get(action, action)


def _zh_ref(rtype: str) -> str:
    return {"branch": "分支", "tag": "标签"}.get(rtype, rtype)


def send_message(message: dict) -> bool:
    """发送消息到飞书。dry_run 模式下只打印不发送。"""
    if DRY_RUN:
        print("\n" + "=" * 60)
        print("  DRY RUN - 以下消息不会发送到飞书群")
        print("=" * 60)
        print(json.dumps(message, ensure_ascii=False, indent=2))
        print("=" * 60 + "\n")
        return True

    resp = requests.post(FEISHU_WEBHOOK_URL, json=message, timeout=30)
    resp.raise_for_status()
    result = resp.json()
    if result.get("code") != 0:
        raise RuntimeError(f"飞书 API 返回错误: {result}")
    return True
