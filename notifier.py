"""飞书消息模块：使用飞书卡片原生 table 组件构建 commit 表格。"""
from datetime import datetime, timezone, timedelta
from commit_text import brief_commit_messages

_SHANGHAI = timezone(timedelta(hours=8))

# repo 通俗描述缓存
_repo_desc_cache: dict[str, str] = {}

# 已知的仓库通俗描述（覆盖常见仓库）
_REPO_DESC_MAP = {
    "project-history": "和舒舒的聊天机器人",
    "bytedance-algorithm-roadmap": "字节跳动算法路线图，系统学习算法",
    "interview": "程序员面试题库，备战技术面试",
    "paddle": "百度飞桨深度学习框架",
    "mall": "电商系统实战项目（Spring Boot）",
    "MediaCrawler": "社交媒体数据爬虫工具",
    "electrobun": "跨平台桌面应用开发框架",
    "agentops": "AI Agent 运维监控工具",
    "nn-verify": "神经网络鲁棒性验证（Marabou/Z3）",
    "lean-utils": "Lean 4 形式化验证工具集",
}


def _get_repo_desc(repo: str) -> str:
    """获取仓库的通俗描述。优先用内置映射，否则查 GitHub API，再否则用仓库名猜测。"""
    if not repo:
        return ""
    short = repo.split("/")[-1] if "/" in repo else repo
    if short in _repo_desc_cache:
        return _repo_desc_cache[short]

    # 1. 先查内置映射
    if short in _REPO_DESC_MAP:
        _repo_desc_cache[short] = _REPO_DESC_MAP[short]
        return _REPO_DESC_MAP[short]

    # 2. 查 GitHub API 的 description
    try:
        import requests
        from config import GITHUB_TOKEN
        resp = requests.get(
            f"https://api.github.com/repos/{repo}",
            headers={"Authorization": f"token {GITHUB_TOKEN}"},
            timeout=10,
        )
        gh_desc = resp.json().get("description", "") or ""
        if gh_desc:
            _repo_desc_cache[short] = gh_desc
            return gh_desc
    except Exception:
        pass

    # 3. 没有描述就返回空
    _repo_desc_cache[short] = ""
    return ""


def build_message(activities: list[dict], summary: str = "") -> dict:
    """构建飞书交互式卡片消息：原生 table 表格 + 统计。summary 可选。
    使用 schema 2.0 格式和原生 table 组件。
    同一项目 1 小时内的提交合并成一行。
    """
    repo_set = {a["repo"] for a in activities if a["repo"]}
    repo_count = len(repo_set)
    activity_count = len(activities)

    # ---- 合并同一项目 1 小时内的 PushEvent ----
    star_repos = []
    table_rows = []
    push_groups: list[tuple[str, list[dict]]] = []
    current_push_group_by_repo: dict[str, list[dict]] = {}

    for a in activities:
        if a.get("type") == "WatchEvent":
            if a["repo"] not in star_repos:
                star_repos.append(a["repo"])
        elif a.get("type") == "PushEvent":
            repo = a["repo"]
            # 检查是否可以合并到已有分组（同一仓库 + 组内总跨度 1 小时内）
            group = current_push_group_by_repo.get(repo)
            if group:
                first_time = _parse_time(group[0]["created_at"])
                cur_time = _parse_time(a["created_at"])
                if first_time and cur_time and abs((cur_time - first_time).total_seconds()) <= 3600:
                    group.append(a)
                    continue

            group = [a]
            push_groups.append((repo, group))
            current_push_group_by_repo[repo] = group
        else:
            table_rows.append({
                "time": _format_time(a["created_at"]),
                "desc": _get_repo_desc(a["repo"]),
                "content": _format_content(a),
            })

    # 把合并后的 push 分组转成表格行
    for repo, group in push_groups:
        if len(group) == 1:
            # 单条不合并
            a = group[0]
            table_rows.append({
                "time": _format_time(a["created_at"]),
                "desc": _get_repo_desc(a["repo"]),
                "content": _format_content(a),
            })
        else:
            # 多条合并
            first_time = _format_time(group[0]["created_at"])
            total_commits = sum(g["detail"].get("commit_count", 1) for g in group)
            all_msgs = []
            for g in group:
                all_msgs.extend(g["detail"].get("commit_messages", []))
            brief = brief_commit_messages(all_msgs, limit=3)
            content = f"提交 {total_commits} 次" + (f": {brief}" if brief else "")
            table_rows.append({
                "time": first_time,
                "desc": _get_repo_desc(repo),
                "content": content,
            })

    # Star 合并成一行
    if star_repos:
        star_descs = [f"{r.split('/')[-1]}: {_get_repo_desc(r)}" for r in star_repos]
        table_rows.append({
            "time": _format_time(activities[0]["created_at"]) if activities else "",
            "desc": "; ".join(star_descs),
            "content": f"Star 收藏 {len(star_repos)} 个项目",
        })

    # body elements
    body_elements = []

    body_elements.append({
        "tag": "markdown",
        "content": summary or "舒舒，秋酿这边刚刚有新动态。总结暂时没生成出来，但我先把时间线放下面给你看。",
    })

    # 原生 table 组件。手机端列宽很窄，保留两列让时间完整显示。
    body_elements.append({
        "tag": "table",
        "columns": [
            {
                "data_type": "text",
                "name": "time",
                "display_name": "时间",
                "horizontal_align": "center",
                "width": "34%",
            },
            {
                "data_type": "text",
                "name": "activity",
                "display_name": "动态",
                "horizontal_align": "left",
                "width": "auto",
            },
        ],
        "rows": _compact_rows(table_rows),
        "row_height": "low",
        "header_style": {
            "background_style": "grey",
            "bold": True,
            "lines": 1,
        },
        "page_size": min(len(table_rows), 20),
        "margin": "0px 0px 0px 0px",
    })

    # 统计行（schema 2.0 不支持 note，用 markdown）
    body_elements.append({
        "tag": "markdown",
        "content": f"📊 本次共 {activity_count} 条活动 | 涉及 {repo_count} 个仓库",
    })

    # schema 2.0 格式
    return {
        "msg_type": "interactive",
        "card": {
            "schema": "2.0",
            "config": {"update_multi": True},
            "header": {
                "title": {"tag": "plain_text", "content": "三哥最近的新活动"},
                "template": "turquoise",
                "padding": "12px 12px 12px 12px",
            },
            "body": {
                "direction": "vertical",
                "padding": "12px 12px 12px 12px",
                "elements": body_elements,
            },
        },
    }


def _format_time(iso_str: str) -> str:
    """把 ISO 时间转成上海时区的 MM-DD HH:MM。"""
    try:
        dt = datetime.fromisoformat(iso_str.replace("Z", "+00:00"))
        return dt.astimezone(_SHANGHAI).strftime("%m-%d %H:%M")
    except Exception:
        return iso_str


def _compact_rows(rows: list[dict]) -> list[dict]:
    """把项目介绍和操作合并，避免手机端把时间列压成省略号。"""
    compact = []
    for row in rows:
        desc = row.get("desc", "")
        content = row.get("content", "")
        activity = f"{desc} | {content}" if desc else content
        compact.append({"time": row.get("time", ""), "activity": activity})
    return compact


def _parse_time(iso_str: str):
    """把 ISO 时间转成 datetime 对象，失败返回 None。"""
    try:
        return datetime.fromisoformat(iso_str.replace("Z", "+00:00"))
    except Exception:
        return None


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
        brief = brief_commit_messages(msgs, limit=3)
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


def _zh_action(action: str) -> str:
    return {
        "opened": "新建",
        "closed": "关闭",
        "reopened": "重新打开",
        "merged": "合并",
    }.get(action, action)


def _zh_ref(rtype: str) -> str:
    return {"branch": "分支", "tag": "标签"}.get(rtype, rtype)
