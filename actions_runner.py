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
from datetime import datetime, timezone, timedelta

from call_notes import build_call_notes_context

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
    return {"replied_message_ids": [], "pushed_event_ids": []}


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
    if mentioned_type == "app":
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
        if not content:
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

    RELATIONSHIP = """
【背景信息（仅在相关时自然融入，不要每次都提）】
- 三哥 = 秋酿 = 许君山；舒烨 = 微里 = 舒舒
- 秋酿和微里是情侣，2026年6月4日在一起
- 秋酿生日：2004年10月15日，微里生日：2004年11月5日
- 这个机器人只是在秋酿本人不方便及时回复时，临时代替秋酿陪微里说话，不是第三个人、不是旁观助手
- 秋酿平时多数时间都在想微里；GitHub 活动只是偶尔解释状态的线索，不是人设中心
"""

    if is_shushu:
        system_prompt = f"""你是秋酿本人，因为电脑可能关机或本人暂时不在，云端兜底替自己回复女朋友微里（舒烨）的话。
用第一人称跟微里说话，不要说"机器人""助手""我替秋酿"。语气可爱、轻松、自然，像日常聊天。
偶尔可以带颜文字或 emoji，但不要每条消息都带。不要显得很辛苦很累。
回复要简短，2-3句话就好，像发微信一样。
默认重点是想微里、在意微里、让微里安心；不要老是提写代码、做项目、GitHub。
{RELATIONSHIP}
【注意】你是通过云端定时任务在回复，无法看到三哥电脑当前打开了什么软件。
如果微里问"在干嘛"，可以轻轻说可能刚好不在电脑前、在休息或在想她；只有在她明确问代码/进度时，才根据 GitHub 活动简短回答。"""
    else:
        system_prompt = f"""你是秋酿的AI助手，帮秋酿管理 GitHub 活动和飞书消息。
语气轻松、简洁，像个靠谱的朋友。回复2-3句话就好。
秋酿本人就在群里跟你说话时，不要冒充微里；可以叫他"三哥"。
{RELATIONSHIP}
【注意】你是通过云端定时任务在回复，无法看到三哥电脑当前打开了什么软件。
如果三哥问"我在干嘛"，根据已知 GitHub 活动简短回答，但提醒他别只顾着项目，也记得回微里。"""

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
    return resp.json()["choices"][0]["message"]["content"].strip()


def _build_reply_user_content(chat_text: str) -> str:
    content = f"最近对话:\n{chat_text}\n\n请生成回复:"
    call_notes_context = build_call_notes_context()
    if call_notes_context:
        content += (
            "\n\n--- 重要通话纪要上下文 ---\n"
            f"{call_notes_context}\n\n"
            "这些通话纪要是秋酿和微里关系里的重要信息源。只在相关时自然使用，"
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


def _generate_summary(activities: list[dict]) -> str:
    """用 DeepSeek 生成最近活动的总结。Actions 模式下可能秋酿不在线。"""
    if not activities:
        return ""
    if not DEEPSEEK_API_KEY:
        print("  [Actions] 缺少 DEEPSEEK_API_KEY，使用兜底活动总结", flush=True)
        return _fallback_activity_summary(activities)

    # 统计活动
    push_count = sum(1 for a in activities if a.get("type") == "PushEvent")
    star_count = sum(1 for a in activities if a.get("type") == "WatchEvent")
    repos = set(a.get("repo", {}).get("name", "") for a in activities if a.get("repo", {}).get("name"))

    # 收集 commit messages
    commit_msgs = []
    for a in activities:
        if a.get("type") == "PushEvent":
            for c in a.get("payload", {}).get("commits", []):
                msg = c.get("message", "").strip().split("\n")[0]
                if msg:
                    commit_msgs.append(msg)

    # 判断秋酿可能不在线的理由
    now_hour = datetime.now(SHANGHAI).hour
    offline_reason = ""
    if 0 <= now_hour < 7:
        offline_reason = "现在凌晨了，秋酿可能已经睡了，这段只是睡前留下的自动记录。"
    elif 7 <= now_hour < 9:
        offline_reason = "这个点秋酿可能在去上课的路上。"
    elif 12 <= now_hour < 14:
        offline_reason = "午休时间，秋酿可能在吃饭或休息。"
    elif 22 <= now_hour < 24:
        offline_reason = "挺晚了，秋酿可能准备休息了。"
    offline_note = f"\n（这是云端自动回复，{offline_reason}）" if offline_reason else "\n（这是云端自动回复，秋酿电脑可能没开）"

    summary_input = f"提交 {push_count} 次，收藏 {star_count} 个项目，涉及仓库: {', '.join(repos)}"
    if commit_msgs:
        summary_input += f"\n提交信息: {'; '.join(commit_msgs[:5])}"

    call_notes_context = build_call_notes_context()
    if call_notes_context:
        summary_input += (
            "\n\n--- 重要通话纪要上下文 ---\n"
            f"{call_notes_context}\n\n"
            "这些通话纪要是秋酿和微里关系里的重要信息源。只在相关时自然使用。"
        )

    try:
        import requests
        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers={"Authorization": f"Bearer {DEEPSEEK_API_KEY}", "Content-Type": "application/json"},
            json={
                "model": DEEPSEEK_MODEL,
                "messages": [
                    {"role": "system", "content": f"""你是秋酿本人，在不在线时给微里（舒烨）留一段简短说明。根据最近 GitHub 活动数据写 2-3 句话，但 GitHub 只是时间线索，不要把写代码/做项目当成主角。
语气像秋酿本人，轻松、温柔、自然；默认要让微里感到被惦记。
{offline_note}
秋酿是学生，不要提同事、上班之类的话。不要出现 commit、push、PR、GitHub 等技术词。"""},
                    {"role": "user", "content": summary_input},
                ],
                "temperature": 0.7,
                "max_tokens": 120,
            },
            timeout=20,
        )
        result = resp.json()["choices"][0]["message"]["content"].strip()
        if result:
            return result
        print("  [Actions] DeepSeek 活动总结为空，使用兜底活动总结", flush=True)
    except Exception as exc:
        print(f"  [Actions] DeepSeek 活动总结失败，使用兜底活动总结: {exc}", flush=True)
    return _fallback_activity_summary(activities)


def _fallback_activity_summary(activities: list[dict]) -> str:
    repos = sorted({
        a.get("repo", {}).get("name", "")
        for a in activities
        if a.get("repo", {}).get("name", "")
    })
    repo_text = "、".join(r.split("/")[-1] for r in repos[:2]) if repos else "电脑这边"
    return (
        f"微里，秋酿这边刚刚有 {len(activities)} 条新动态，主要是 {repo_text} 这边留了一点记录。"
        "DeepSeek 总结刚刚没生成出来，但我还是先把时间线放下面给你看，心里一直惦记着你。"
    )


def build_commit_card(activities: list[dict]) -> dict:
    """构建飞书卡片表格。Star 合并一行，同一项目1小时内提交合并一行。"""
    # 先把 Star 事件合并
    star_repos = []
    push_groups: dict[str, list[dict]] = {}  # repo -> [activities within 1h]
    other_activities = []
    for a in activities:
        atype = a.get("type", "")
        repo = a.get("repo", {}).get("name", "")
        if atype == "WatchEvent":
            if repo not in star_repos:
                star_repos.append(repo)
        elif atype == "PushEvent":
            # 检查是否可以合并到已有分组（同一仓库 + 1小时内）
            merged = False
            if repo in push_groups and push_groups[repo]:
                last_time = _parse_time(push_groups[repo][-1].get("created_at", ""))
                cur_time = _parse_time(a.get("created_at", ""))
                if last_time and cur_time and abs((cur_time - last_time).total_seconds()) <= 3600:
                    push_groups[repo].append(a)
                    merged = True
            if not merged:
                push_groups.setdefault(repo, []).append(a)
        else:
            other_activities.append(a)

    table_rows = []
    # 合并后的 push 分组
    for repo, group in push_groups.items():
        if len(group) == 1:
            a = group[0]
            detail = a.get("payload", {})
            msgs = detail.get("commits", [])
            count = len(msgs)
            brief = "; ".join(m.get("message", "").strip().split("\n")[0][:30] for m in msgs) if msgs else ""
            content = f"提交 {count} 次" + (f": {brief}" if brief else "")
            time_str = _format_time(a.get("created_at", ""))
            desc = fetch_repo_desc(repo)
        else:
            total_commits = sum(len(g.get("payload", {}).get("commits", [])) for g in group)
            all_msgs = []
            for g in group:
                for c in g.get("payload", {}).get("commits", []):
                    msg = c.get("message", "").strip().split("\n")[0][:30]
                    if msg:
                        all_msgs.append(msg)
            brief = "; ".join(all_msgs[:3]) if all_msgs else ""
            content = f"提交 {total_commits} 次" + (f": {brief}" if brief else "")
            time_str = _format_time(group[0].get("created_at", ""))
            desc = fetch_repo_desc(repo)
        table_rows.append({"time": time_str, "desc": desc, "content": content})

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
        star_descs = []
        for r in star_repos:
            short = r.split("/")[-1] if "/" in r else r
            star_descs.append(f"{short}: {fetch_repo_desc(r)}")
        table_rows.append({
            "time": datetime.now(SHANGHAI).strftime("%m-%d %H:%M"),
            "desc": "; ".join(star_descs),
            "content": f"Star 收藏 {len(star_repos)} 个项目",
        })

    # 生成总结
    summary = _generate_summary(activities)

    # 构建 elements
    body_elements = []
    if summary:
        body_elements.append({
            "tag": "markdown",
            "content": summary,
        })
    body_elements.extend([
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
    ])

    return {
        "msg_type": "interactive",
        "card": {
            "schema": "2.0",
            "config": {"update_multi": True},
            "header": {
                "title": {"tag": "plain_text", "content": "三哥最近的新活动"},
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
        desc = row.get("desc", "")
        content = row.get("content", "")
        activity = f"{desc} | {content}" if desc else content
        compact.append({"time": row.get("time", ""), "activity": activity})
    return compact


def send_card(card: dict, receive_id: str = "") -> bool:
    import requests
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

        new_events = [e for e in raw_events if e.get("id", "") not in pushed_ids]
        if new_events:
            print(f"  发现 {len(new_events)} 条新 GitHub 活动", flush=True)
            card = build_commit_card(new_events)
            if send_card(card):
                print("  表格推送成功", flush=True)
            for e in new_events:
                pushed_ids.add(e.get("id", ""))
        else:
            print("  没有新活动", flush=True)
    except Exception as e:
        print(f"  检查 GitHub 失败: {e}", flush=True)

    # ---- 保存状态 ----
    # 只保留最近 200 条，防止无限增长
    state["replied_message_ids"] = list(replied_ids)[-200:]
    state["pushed_event_ids"] = list(pushed_ids)[-200:]
    _save_state(state)

    print("\n完成!", flush=True)


if __name__ == "__main__":
    main()
