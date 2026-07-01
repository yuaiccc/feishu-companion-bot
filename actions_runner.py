"""GitHub Actions 轮询脚本：无 WebSocket，纯 REST API 轮询。
每次 Actions 运行时执行：
1. 读飞书群最近消息，找出没回复过的，生成回复并发送
2. 检查 GitHub 新活动，有就推 commit 表格
3. 状态保存在 GitHub Actions Cache 或 Bitable 中

用法: python actions_runner.py
环境变量（从 GitHub Actions Secrets 注入）:
  FEISHU_APP_ID, FEISHU_APP_SECRET, FEISHU_CHAT_ID,
  FEISHU_SHUSHU_OPEN_ID, FEISHU_SANGE_OPEN_ID,
  GH_USERNAME, GH_TOKEN, GH_PRIVATE_REPOS,
  DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL
"""
import os
import sys
import json
import time
import hashlib
from datetime import datetime, timezone, timedelta

from feishu_companion.commit_text import brief_commit_messages, summarize_commit_activity, summarize_star_activity
from feishu_companion.call_notes import build_call_notes_context
from feishu_companion.profile import bot_role, owner_name, relationship_context, target_addressing_instruction, target_name
from feishu_companion.text_safety import sanitize_card, sanitize_public_text

# 确保输出不缓冲
sys.stdout.reconfigure(line_buffering=True)
sys.stderr.reconfigure(line_buffering=True)

# ---- 配置 ----
FEISHU_APP_ID = os.getenv("FEISHU_APP_ID", "")
FEISHU_APP_SECRET = os.getenv("FEISHU_APP_SECRET", "")
FEISHU_CHAT_ID = os.getenv("FEISHU_CHAT_ID", "")
FEISHU_SHUSHU_OPEN_ID = os.getenv("FEISHU_SHUSHU_OPEN_ID", "")
FEISHU_SANGE_OPEN_ID = os.getenv("FEISHU_SANGE_OPEN_ID", "")
FEISHU_BOT_OPEN_ID = os.getenv("FEISHU_BOT_OPEN_ID", "")
FEISHU_BOT_NAME = os.getenv("FEISHU_BOT_NAME", "")
GITHUB_USERNAME = os.getenv("GH_USERNAME", "")
GITHUB_TOKEN = os.getenv("GH_TOKEN", "")
GITHUB_PRIVATE_REPOS = [r.strip() for r in os.getenv("GH_PRIVATE_REPOS", "").split(",") if r.strip()]
DEEPSEEK_API_KEY = os.getenv("DEEPSEEK_API_KEY", "")
DEEPSEEK_BASE_URL = os.getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com").rstrip("/")
DEEPSEEK_MODEL = os.getenv("DEEPSEEK_MODEL", "deepseek-chat")

OPEN_API = "https://open.feishu.cn/open-apis"
SHANGHAI = timezone(timedelta(hours=8))
_RECALLED_TEXTS = {"This message was recalled", "消息已撤回"}

# 状态文件：GitHub Actions 之间通过 actions/cache 恢复和保存
STATE_FILE = "actions_state.json"


def _get_token() -> str:
    """获取 tenant_access_token。"""
    import requests
    resp = requests.post(
        f"{OPEN_API}/auth/v3/tenant_access_token/internal",
        json={"app_id": FEISHU_APP_ID, "app_secret": FEISHU_APP_SECRET},
        timeout=30,
    )
    return resp.json()["tenant_access_token"]


def _load_state() -> dict:
    if os.path.exists(STATE_FILE):
        try:
            with open(STATE_FILE, "r") as f:
                return json.load(f)
        except Exception:
            pass
    return {"replied_message_ids": [], "pushed_event_ids": [], "pushed_event_fingerprints": []}


def _save_state(state: dict):
    with open(STATE_FILE, "w") as f:
        json.dump(state, f, ensure_ascii=False, indent=2)


def _is_bot_mention(mention: dict) -> bool:
    """判断一条 mention 是否指向当前飞书应用机器人。

    飞书消息事件里应用 mention 使用 mentioned_type=app；消息列表 REST
    的 mentions[].id 是被 @ 用户或机器人的 open_id 字符串。
    Actions 兜底要配置 FEISHU_BOT_OPEN_ID 才能精确判断 @ 机器人。
    """
    if not isinstance(mention, dict):
        return False

    mentioned_type = str(mention.get("mentioned_type", "")).lower()
    if mentioned_type in ("app", "bot"):
        return True

    mention_id = mention.get("id", "")
    if isinstance(mention_id, str):
        if FEISHU_BOT_OPEN_ID and mention_id == FEISHU_BOT_OPEN_ID:
            return True
        if FEISHU_BOT_NAME and mention.get("name") == FEISHU_BOT_NAME:
            return True
        return False

    app_id = mention_id.get("app_id") or mention_id.get("appId")
    if app_id and app_id == FEISHU_APP_ID:
        return True

    # Some API shapes identify an app mention by putting "app" in the id type.
    id_type = str(mention_id.get("id_type") or mention_id.get("type") or "").lower()
    if id_type == "app":
        return True

    open_id = str(mention_id.get("open_id", "")).lower()
    return open_id == "app"


def _message_mentions_bot(item: dict, content: str = "") -> bool:
    """群聊兜底：优先用 mentions 判断，缺字段时只兼容旧占位符文本。"""
    mentions = item.get("mentions") or []
    if mentions:
        return any(_is_bot_mention(m) for m in mentions)

    # 兼容少数旧消息列表没有 mentions 数组，但 text 里仍保留 @_user_N 占位符。
    return "@_user_" in content


# ---- 飞书消息读取 ----

def fetch_chat_messages(limit: int = 20) -> list[dict]:
    """读取群聊最近消息。"""
    import requests
    token = _get_token()
    resp = requests.get(
        f"{OPEN_API}/im/v1/messages",
        headers={"Authorization": f"Bearer {token}"},
        params={
            "container_id": FEISHU_CHAT_ID,
            "container_id_type": "chat",
            "sort_type": "ByCreateTimeDesc",
            "page_size": min(limit, 50),
        },
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        print(f"  [Actions] 读取消息失败: {data.get('msg')}", flush=True)
        return []

    items = data.get("data", {}).get("items", [])
    messages = []
    for item in reversed(items):  # 按时间正序
        sender = item.get("sender", {})
        sender_id = sender.get("id", "")
        sender_type = sender.get("sender_type", "")
        if sender_type != "user":
            continue

        msg_type = item.get("msg_type", "")
        body = item.get("body", {})
        content_raw = body.get("content", "") if body else ""
        content = _extract_text(msg_type, content_raw)
        if not content or content in _RECALLED_TEXTS:
            continue

        is_shushu = sender_id == FEISHU_SHUSHU_OPEN_ID
        is_sange = sender_id == FEISHU_SANGE_OPEN_ID
        if not is_shushu and not is_sange:
            continue

        # 群聊消息只有 @当前应用机器人 才回复。
        if not _message_mentions_bot(item, content):
            continue

        import re
        content = re.sub(r'@_user_\d+', '', content).strip()
        if not content:
            content = "只叫了你一声"

        messages.append({
            "message_id": item.get("message_id", ""),
            "content": content,
            "sender": "舒舒" if is_shushu else "三哥",
            "is_shushu": is_shushu,
        })

    return messages


def _extract_text(msg_type: str, content_raw: str) -> str:
    if not content_raw:
        return ""
    if msg_type == "text":
        try:
            c = json.loads(content_raw)
            if isinstance(c, dict) and "text" in c:
                return c["text"].strip()
        except (json.JSONDecodeError, TypeError):
            pass
        return content_raw.strip()
    if msg_type == "post":
        try:
            c = json.loads(content_raw)
        except (json.JSONDecodeError, TypeError):
            return ""
        texts = []
        for v in c.values():
            if isinstance(v, list):
                for line in v:
                    if isinstance(line, list):
                        for elem in line:
                            if isinstance(elem, dict) and elem.get("tag") == "text":
                                texts.append(elem.get("text", ""))
        return " ".join(texts).strip()
    return ""


# ---- 飞书发送消息 ----

def send_text(text: str, receive_id: str = "") -> bool:
    import requests
    text = sanitize_public_text(text)
    token = _get_token()
    target = receive_id or FEISHU_CHAT_ID
    resp = requests.post(
        f"{OPEN_API}/im/v1/messages",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        params={"receive_id_type": "chat_id"},
        json={
            "receive_id": target,
            "msg_type": "text",
            "content": json.dumps({"text": text}),
        },
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        print(f"  [Actions] 发送消息失败: {data.get('msg')}", flush=True)
        return False
    return True


def reply_text(text: str, message_id: str) -> bool:
    import requests
    text = sanitize_public_text(text)
    token = _get_token()
    resp = requests.post(
        f"{OPEN_API}/im/v1/messages/{message_id}/reply",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        json={
            "msg_type": "text",
            "content": json.dumps({"text": text}),
        },
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        print(f"  [Actions] 回复消息失败: {data.get('msg')}", flush=True)
        return False
    return True


def react_to_message(message_id: str, emoji_type: str) -> bool:
    import requests
    token = _get_token()
    resp = requests.post(
        f"{OPEN_API}/im/v1/messages/{message_id}/reactions",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        json={"reaction_type": {"emoji_type": emoji_type}},
        timeout=30,
    )
    data = resp.json()
    return data.get("code") == 0


# ---- DeepSeek 回复 ----

def generate_reply(messages: list[dict], is_shushu: bool = True) -> str:
    """调用 DeepSeek 生成回复。Actions 模式下没有本地应用检测能力。"""
    import requests

    chat_text = "\n".join(f"{m['sender']}: {m['content']}" for m in messages)

    # 检测是否在问"在干嘛"
    is_asking_activity = any(kw in chat_text.lower() for kw in [
        "在干嘛", "在干啥", "干嘛", "干啥", "忙什么", "忙啥",
        "在做什么", "在搞什么", "在弄什么", "最近在",
    ])

    relationship = relationship_context()
    owner = owner_name()
    target = target_name()
    role = bot_role()
    addressing = target_addressing_instruction()

    if is_shushu:
        system_prompt = f"""你是{role}，因为{owner}的本地机器人可能离线，云端兜底帮忙回复{target}的话。
不要冒充{owner}本人，不要用{owner}第一人称说话。可以说"{owner}可能...""我先帮忙看着"。语气轻松、自然，像日常聊天。
偶尔可以带颜文字或 emoji，但不要每条消息都带。不要显得很辛苦很累。
回复要简短，2-3句话就好，像发微信一样。
{addressing}
默认重点是回应对方的问题和情绪；不要老是提写代码、做项目、GitHub。
{relationship}
【注意】你是通过云端定时任务在回复，无法看到{owner}电脑当前打开了什么软件。
如果{target}问"在干嘛"，可以轻轻说本地状态暂时看不到；只有在对方明确问代码/进度时，才根据 GitHub 活动简短回答。"""
    else:
        system_prompt = f"""你是{role}，帮{owner}管理 GitHub 活动和飞书消息。
语气轻松、简洁，像个靠谱的朋友。回复2-3句话就好。
{owner}自己就在群里跟你说话时，不要冒充{target}；可以直接称呼{owner}。
{relationship}
【注意】你是通过云端定时任务在回复，无法看到{owner}电脑当前打开了什么软件。
如果{owner}问"我在干嘛"，根据已知 GitHub 活动简短回答，并说明本地状态暂时看不到。"""

    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": _build_reply_user_content(chat_text)},
        ],
        "temperature": 0.8,
        "max_tokens": 300,
    }

    resp = requests.post(
        f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
        headers=headers,
        json=payload,
        timeout=60,
    )
    resp.raise_for_status()
    return sanitize_public_text(resp.json()["choices"][0]["message"]["content"].strip())


def _build_reply_user_content(chat_text: str) -> str:
    content = f"最近对话:\n{chat_text}\n\n请生成回复:"
    call_notes_context = build_call_notes_context()
    if call_notes_context:
        content += (
            "\n\n--- 重要通话纪要上下文 ---\n"
            f"{call_notes_context}\n\n"
            "这些通话纪要是关系里的重要信息源。只在相关时自然使用，"
            "不要暴露为'我读取了纪要'。"
        )
    return content


# ---- GitHub 活动 ----

def fetch_github_events() -> list[dict]:
    import requests
    resp = requests.get(
        f"https://api.github.com/users/{GITHUB_USERNAME}/events/public",
        headers={"Authorization": f"token {GITHUB_TOKEN}"},
        timeout=30,
    )
    return resp.json() if resp.status_code == 200 else []


def fetch_private_repo_commits(repo: str) -> list[dict]:
    import requests
    resp = requests.get(
        f"https://api.github.com/repos/{repo}/commits",
        headers={"Authorization": f"token {GITHUB_TOKEN}"},
        params={"per_page": 10},
        timeout=30,
    )
    if resp.status_code != 200:
        return []
    commits = resp.json()
    events = []
    for c in commits:
        commit_data = c.get("commit", {})
        events.append({
            "id": c.get("sha", ""),
            "type": "PushEvent",
            "repo": {"name": repo},
            "created_at": commit_data.get("author", {}).get("date", ""),
            "payload": {
                "ref": f"refs/heads/main",
                "size": 1,
                "commits": [{"message": commit_data.get("message", "")}],
            },
        })
    return events


def event_fingerprint(ev: dict) -> str:
    etype = ev.get("type", "")
    payload = ev.get("payload", {}) or {}
    repo = (ev.get("repo", {}) or {}).get("name", "")
    if etype == "PushEvent":
        head = (payload.get("head") or "").strip()
        if head:
            return f"push:{head}"
        commit_shas = [
            str(c.get("sha") or c.get("id") or "").strip()
            for c in payload.get("commits", []) or []
            if c.get("sha") or c.get("id")
        ]
        if commit_shas:
            return "push:" + ",".join(commit_shas)
    ev_id = str(ev.get("id", "")).strip()
    if ev_id:
        return f"id:{ev_id}"
    raw = "|".join([etype, repo, str(ev.get("created_at", "")), str(payload)[:500]])
    return "hash:" + hashlib.sha1(raw.encode("utf-8")).hexdigest()


def dedupe_events(events: list[dict], seen_fingerprints: set[str] | None = None) -> list[dict]:
    seen = set(seen_fingerprints or set())
    unique = []
    for ev in events:
        fp = event_fingerprint(ev)
        if fp in seen:
            continue
        seen.add(fp)
        unique.append(ev)
    return unique


# 已知的仓库通俗描述（覆盖常见仓库）
_REPO_DESC_MAP = {
    "feishu-companion-bot": "飞书陪伴机器人",
    "bytedance-algorithm-roadmap": "字节跳动算法路线图，系统学习算法",
    "interview": "程序员面试题库，备战技术面试",
    "paddle": "百度飞桨深度学习框架",
    "mall": "电商系统实战项目（Spring Boot）",
    "MediaCrawler": "社交媒体数据爬虫工具",
    "electrobun": "跨平台桌面应用开发框架",
    "agentops": "AI Agent 运维监控工具",
}

# repo 描述缓存
_repo_desc_cache: dict[str, str] = {}


def fetch_repo_desc(repo: str) -> str:
    """获取仓库的通俗描述。优先内置映射，其次 DeepSeek 解释，最后 GitHub API。"""
    if not repo:
        return ""
    short = repo.split("/")[-1] if "/" in repo else repo
    if short in _repo_desc_cache:
        return _repo_desc_cache[short]

    # 1. 先查内置映射
    if short in _REPO_DESC_MAP:
        _repo_desc_cache[short] = _REPO_DESC_MAP[short]
        return _REPO_DESC_MAP[short]

    # 2. 查 GitHub API 的 description，然后用 DeepSeek 解释
    gh_desc = ""
    try:
        import requests
        resp = requests.get(
            f"https://api.github.com/repos/{repo}",
            headers={"Authorization": f"token {GITHUB_TOKEN}"},
            timeout=10,
        )
        gh_desc = resp.json().get("description", "") or ""
    except Exception:
        pass

    if gh_desc:
        # 用 DeepSeek 翻译成通俗中文
        explained = _explain_repo(short, gh_desc)
        _repo_desc_cache[short] = explained
        return explained

    # 3. 没有描述，用仓库名让 DeepSeek 猜
    explained = _explain_repo(short, "")
    _repo_desc_cache[short] = explained
    return explained


def _explain_repo(repo_name: str, gh_desc: str) -> str:
    """用 DeepSeek 把仓库描述翻译成通俗易懂的中文。"""
    if not DEEPSEEK_API_KEY:
        return gh_desc or repo_name

    try:
        import requests
        prompt = "你是一个项目解释器。给你一个 GitHub 仓库名和描述，用一句通俗的中文解释这个项目是干什么的，不超过20个字。只输出解释，不要多余的话。"
        user_msg = f"仓库名: {repo_name}\nGitHub描述: {gh_desc}" if gh_desc else f"仓库名: {repo_name}\nGitHub描述: (无)"
        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers={"Authorization": f"Bearer {DEEPSEEK_API_KEY}", "Content-Type": "application/json"},
            json={
                "model": DEEPSEEK_MODEL,
                "messages": [
                    {"role": "system", "content": prompt},
                    {"role": "user", "content": user_msg},
                ],
                "temperature": 0.1,
                "max_tokens": 50,
            },
            timeout=15,
        )
        result = resp.json()["choices"][0]["message"]["content"].strip()
        return result
    except Exception:
        return gh_desc or repo_name


def _parse_time(time_str: str):
    """解析 GitHub 时间字符串为 datetime。失败返回 None。"""
    if not time_str:
        return None
    try:
        return datetime.fromisoformat(time_str.replace("Z", "+00:00"))
    except ValueError:
        return None


def _format_time(time_str: str) -> str:
    """格式化时间字符串为 MM-DD HH:MM（上海时区）。"""
    dt = _parse_time(time_str)
    if not dt:
        return ""
    return dt.astimezone(SHANGHAI).strftime("%m-%d %H:%M")


def build_commit_card(activities: list[dict]) -> dict:
    """构建飞书卡片表格。Star 合并一行，同一项目1小时内提交合并一行。"""
    # 先把 Star 事件合并
    star_repos = []
    push_groups: list[tuple[str, list[dict]]] = []
    current_push_group_by_repo: dict[str, list[dict]] = {}
    other_activities = []
    for a in activities:
        atype = a.get("type", "")
        repo = a.get("repo", {}).get("name", "")
        if atype == "WatchEvent":
            if repo not in star_repos:
                star_repos.append(repo)
        elif atype == "PushEvent":
            # 检查是否可以合并到已有分组（同一仓库 + 组内总跨度 1 小时内）
            group = current_push_group_by_repo.get(repo)
            if group:
                first_time = _parse_time(group[0].get("created_at", ""))
                cur_time = _parse_time(a.get("created_at", ""))
                if first_time and cur_time and abs((cur_time - first_time).total_seconds()) <= 3600:
                    group.append(a)
                    continue

            group = [a]
            push_groups.append((repo, group))
            current_push_group_by_repo[repo] = group
        else:
            other_activities.append(a)

    table_rows = []
    # 合并后的 push 分组
    for repo, group in push_groups:
        if len(group) == 1:
            a = group[0]
            detail = a.get("payload", {})
            msgs = detail.get("commits", [])
            count = len(msgs)
            raw_msgs = [m.get("message", "") for m in msgs]
            brief = brief_commit_messages(raw_msgs, limit=3)
            content = f"提交 {count} 次" + (f": {brief}" if brief else "")
            time_str = _format_time(a.get("created_at", ""))
            desc = fetch_repo_desc(repo)
            summary = summarize_commit_activity(desc, raw_msgs, count)
        else:
            total_commits = sum(len(g.get("payload", {}).get("commits", [])) for g in group)
            all_msgs = []
            for g in group:
                for c in g.get("payload", {}).get("commits", []):
                    msg = c.get("message", "")
                    if msg:
                        all_msgs.append(msg)
            brief = brief_commit_messages(all_msgs, limit=3)
            content = f"提交 {total_commits} 次" + (f": {brief}" if brief else "")
            time_str = _format_time(group[0].get("created_at", ""))
            desc = fetch_repo_desc(repo)
            summary = summarize_commit_activity(desc, all_msgs, total_commits)
        table_rows.append({"time": time_str, "desc": desc, "content": content, "summary": summary})

    # 其他活动
    for a in other_activities[:10]:
        atype = a.get("type", "")
        repo = a.get("repo", {}).get("name", "")
        desc = fetch_repo_desc(repo)
        content = atype
        time_str = _format_time(a.get("created_at", ""))
        table_rows.append({"time": time_str, "desc": desc, "content": content})

    # Star 合并成一行
    if star_repos:
        star_items = []
        for r in star_repos:
            short = r.split("/")[-1] if "/" in r else r
            star_items.append((short, fetch_repo_desc(r)))
        star_descs = [f"{short}: {desc}" for short, desc in star_items]
        table_rows.append({
            "time": datetime.now(SHANGHAI).strftime("%m-%d %H:%M"),
            "desc": "; ".join(star_descs),
            "content": f"Star 收藏 {len(star_repos)} 个项目",
            "summary": summarize_star_activity(star_items),
        })

    # 构建 elements
    body_elements = [
        {
            "tag": "table",
            "columns": [
                {"data_type": "text", "name": "time", "display_name": "时间", "horizontal_align": "center", "width": "34%"},
                {"data_type": "text", "name": "activity", "display_name": "动态", "horizontal_align": "left", "width": "auto"},
            ],
            "rows": _compact_rows(table_rows),
            "row_height": "low",
            "header_style": {"background_style": "grey", "bold": True, "lines": 1},
            "page_size": min(len(table_rows), 20),
        },
        {
            "tag": "markdown",
            "content": f"📊 本次共 {len(activities)} 条活动",
        },
    ]

    return {
        "msg_type": "interactive",
        "card": {
            "schema": "2.0",
            "config": {"update_multi": True},
            "header": {
                "title": {"tag": "plain_text", "content": f"{owner_name()}最近的新活动"},
                "template": "turquoise",
            },
            "body": {
                "direction": "vertical",
                "padding": "12px 12px 12px 12px",
                "elements": body_elements,
            },
        },
    }

def _compact_rows(rows: list[dict]) -> list[dict]:
    """把项目介绍和操作合并，避免手机端把时间列压成省略号。"""
    compact = []
    for row in rows:
        summary = row.get("summary", "")
        if summary:
            compact.append({"time": row.get("time", ""), "activity": summary})
            continue
        desc = row.get("desc", "")
        content = row.get("content", "")
        activity = f"{desc} | {content}" if desc else content
        compact.append({"time": row.get("time", ""), "activity": activity})
    return compact


def send_card(card: dict, receive_id: str = "") -> bool:
    import requests
    card = sanitize_card(card)
    token = _get_token()
    target = receive_id or FEISHU_CHAT_ID
    resp = requests.post(
        f"{OPEN_API}/im/v1/messages",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        params={"receive_id_type": "chat_id"},
        json={
            "receive_id": target,
            "msg_type": "interactive",
            "content": json.dumps(card["card"], ensure_ascii=False),
        },
        timeout=30,
    )
    data = resp.json()
    return data.get("code") == 0


# ---- 表情选择 ----

def pick_emoji(content: str, is_shushu: bool = False) -> str:
    text = content.lower().strip()
    if is_shushu:
        if any(w in text for w in ["想你", "爱你", "喜欢", "亲", "抱", "贴", "宝贝", "老公"]):
            return "KISS"
        if any(w in text for w in ["哈哈", "开心", "好棒", "嘻嘻"]):
            return "LAUGH"
        if any(w in text for w in ["难过", "哭", "委屈", "呜", "累"]):
            return "COMFORT"
        if any(w in text for w in ["哇", "天呐", "真的吗"]):
            return "WOW"
        return "SMOOCH"
    if any(w in text for w in ["哈哈", "搞笑", "笑死"]):
        return "LOL"
    if any(w in text for w in ["牛", "厉害", "棒", "强", "nice"]):
        return "PRAISE"
    if any(w in text for w in ["谢谢", "感谢"]):
        return "THANKS"
    if any(w in text for w in ["累", "困", "烦", "崩溃"]):
        return "FACEPALM"
    if any(w in text for w in ["commit", "代码", "提交", "github"]):
        return "STRIVE"
    if any(w in text for w in ["好的", "好", "行", "ok", "嗯"]):
        return "DONE"
    import random
    return random.choice(["THUMBSUP", "SMILE", "WINK", "BLUSH"])


# ---- 主流程 ----

def main():
    print("=" * 50, flush=True)
    print("  GitHub Actions Bot Runner", flush=True)
    print(f"  时间: {datetime.now(SHANGHAI).strftime('%Y-%m-%d %H:%M:%S')}", flush=True)
    print("=" * 50, flush=True)

    state = _load_state()
    replied_ids = set(state.get("replied_message_ids", []))
    pushed_ids = set(state.get("pushed_event_ids", []))
    pushed_fingerprints = set(state.get("pushed_event_fingerprints", []))

    # ---- 1. 检查飞书消息并回复 ----
    print("\n[1/2] 检查飞书消息...", flush=True)
    try:
        messages = fetch_chat_messages(limit=20)
        new_messages = [m for m in messages if m["message_id"] not in replied_ids]

        if new_messages:
            print(f"  发现 {len(new_messages)} 条未回复消息", flush=True)
            for msg in new_messages:
                msg_id = msg["message_id"]
                content = msg["content"]
                is_shushu = msg["is_shushu"]
                sender = msg["sender"]
                print(f"  回复 {sender}: {content[:40]}...", flush=True)

                try:
                    # 加思考表情
                    react_to_message(msg_id, "THINKING")

                    # 生成回复
                    reply = generate_reply(messages, is_shushu=is_shushu)
                    if reply:
                        print(f"  回复内容: {reply[:50]}...", flush=True)
                        reply_text(reply, msg_id)

                    # 加内容表情
                    react_to_message(msg_id, pick_emoji(content, is_shushu))
                except Exception as e:
                    print(f"  回复失败: {e}", flush=True)

                replied_ids.add(msg_id)
        else:
            print("  没有新消息", flush=True)
    except Exception as e:
        print(f"  检查消息失败: {e}", flush=True)

    # ---- 2. 检查 GitHub 活动 ----
    print("\n[2/2] 检查 GitHub 活动...", flush=True)
    try:
        raw_events = fetch_github_events()
        for repo in GITHUB_PRIVATE_REPOS:
            raw_events.extend(fetch_private_repo_commits(repo))
        raw_events.sort(key=lambda e: e.get("created_at", ""), reverse=True)
        raw_events = dedupe_events(raw_events, pushed_fingerprints)

        new_events = [
            e for e in raw_events
            if e.get("id", "") not in pushed_ids and event_fingerprint(e) not in pushed_fingerprints
        ]
        if new_events:
            print(f"  发现 {len(new_events)} 条新 GitHub 活动", flush=True)
            card = build_commit_card(new_events)
            if send_card(card):
                print("  表格推送成功", flush=True)
            for e in new_events:
                pushed_ids.add(e.get("id", ""))
                pushed_fingerprints.add(event_fingerprint(e))
        else:
            print("  没有新活动", flush=True)
    except Exception as e:
        print(f"  检查 GitHub 失败: {e}", flush=True)

    # ---- 保存状态 ----
    # 只保留最近 200 条，防止无限增长
    state["replied_message_ids"] = list(replied_ids)[-200:]
    state["pushed_event_ids"] = list(pushed_ids)[-200:]
    state["pushed_event_fingerprints"] = list(pushed_fingerprints)[-200:]
    _save_state(state)

    print("\n完成!", flush=True)


if __name__ == "__main__":
    main()
