"""GitHub Actions 轮询脚本：无 WebSocket，纯 REST API 轮询。
每次 Actions 运行时执行：
1. 读飞书群最近消息，找出没回复过的，生成回复并发送
2. 检查 GitHub 新活动，有就推 commit 表格
3. 状态保存在 GitHub Actions Cache 或 Bitable 中

用法: python actions_runner.py
环境变量（从 GitHub Actions Secrets 注入）:
  FEISHU_APP_ID, FEISHU_APP_SECRET, FEISHU_CHAT_ID,
  FEISHU_SHUSHU_OPEN_ID, FEISHU_SANGE_OPEN_ID,
  GITHUB_USERNAME, GITHUB_TOKEN, GITHUB_PRIVATE_REPOS,
  DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL
  GITHUB_RUN_ID (GitHub Actions 自动注入，用于检测本地是否在跑)
"""
import os
import sys
import json
import time
from datetime import datetime, timezone, timedelta

# 确保输出不缓冲
sys.stdout.reconfigure(line_buffering=True)
sys.stderr.reconfigure(line_buffering=True)

# ---- 配置 ----
FEISHU_APP_ID = os.getenv("FEISHU_APP_ID", "")
FEISHU_APP_SECRET = os.getenv("FEISHU_APP_SECRET", "")
FEISHU_CHAT_ID = os.getenv("FEISHU_CHAT_ID", "")
FEISHU_SHUSHU_OPEN_ID = os.getenv("FEISHU_SHUSHU_OPEN_ID", "")
FEISHU_SANGE_OPEN_ID = os.getenv("FEISHU_SANGE_OPEN_ID", "")
GITHUB_USERNAME = os.getenv("GH_USERNAME", "")
GITHUB_TOKEN = os.getenv("GH_TOKEN", "")
GITHUB_PRIVATE_REPOS = [r.strip() for r in os.getenv("GH_PRIVATE_REPOS", "").split(",") if r.strip()]
DEEPSEEK_API_KEY = os.getenv("DEEPSEEK_API_KEY", "")
DEEPSEEK_BASE_URL = os.getenv("DEEPSEEK_BASE_URL", "https://api.deepseek.com").rstrip("/")
DEEPSEEK_MODEL = os.getenv("DEEPSEEK_MODEL", "deepseek-chat")

OPEN_API = "https://open.feishu.cn/open-apis"
SHANGHAI = timezone(timedelta(hours=8))

# 状态文件（Actions 之间用 artifact 传递，这里用本地临时文件）
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

        # 检查是否 @了机器人
        mentions = item.get("mentions", [])
        is_mentioned = False
        if mentions:
            for m in mentions:
                if m.get("id", {}).get("open_id") == "app":
                    is_mentioned = True
                    break
        # 如果没有 mentions 信息，默认不要求 @（兼容旧消息）
        # 但如果有 mentions 且没 @机器人，跳过
        if mentions and not is_mentioned:
            continue

        import re
        content = re.sub(r'@_user_\d+', '', content).strip()
        if not content:
            continue

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
    """调用 DeepSeek 生成回复。"""
    import requests

    chat_text = "\n".join(f"{m['sender']}: {m['content']}" for m in messages)

    if is_shushu:
        system_prompt = """你帮一个叫"三哥"的程序员，根据他的 GitHub 活动时间记录，写给女朋友"舒舒"（舒烨）的一段话。
你是三哥本人，用第一人称跟舒舒说话。语气可爱、轻松、自然，像日常聊天。
偶尔可以带颜文字或 emoji，但不要每条消息都带。
不要显得很辛苦很累，不要说"忙活""辛苦""努力"这类词。
回复要简短，2-3句话就好，像发微信一样。"""
    else:
        system_prompt = """你是三哥的AI助手，帮三哥管理 GitHub 活动和飞书消息。
语气轻松、简洁，像个靠谱的朋友。回复2-3句话就好。
三哥是你的主人，你叫他"三哥"。"""

    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": system_prompt},
            {"role": "user", "content": f"最近对话:\n{chat_text}\n\n请生成回复:"},
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


def fetch_repo_desc(repo: str) -> str:
    import requests
    try:
        resp = requests.get(
            f"https://api.github.com/repos/{repo}",
            headers={"Authorization": f"token {GITHUB_TOKEN}"},
            timeout=10,
        )
        return resp.json().get("description", "") or ""
    except Exception:
        return ""


def build_commit_card(activities: list[dict]) -> dict:
    """构建飞书卡片表格。"""
    table_rows = []
    for a in activities[:10]:
        atype = a.get("type", "")
        repo = a.get("repo", {}).get("name", "")
        short_repo = repo.split("/")[-1] if "/" in repo else repo
        desc = fetch_repo_desc(repo)

        # 格式化操作
        detail = a.get("payload", {})
        if atype == "PushEvent":
            msgs = detail.get("commits", [])
            count = len(msgs)
            brief = "; ".join(m.get("message", "").strip().split("\n")[0][:30] for m in msgs) if msgs else ""
            content = f"提交 {count} 次" + (f": {brief}" if brief else "")
        elif atype == "WatchEvent":
            content = "Star 收藏"
        else:
            content = atype

        time_str = ""
        created = a.get("created_at", "")
        try:
            dt = datetime.fromisoformat(created.replace("Z", "+00:00"))
            time_str = dt.astimezone(SHANGHAI).strftime("%m-%d %H:%M")
        except Exception:
            time_str = created[:16]

        table_rows.append({"time": time_str, "desc": desc, "content": content})

    return {
        "msg_type": "interactive",
        "card": {
            "schema": "2.0",
            "config": {"update_multi": True},
            "header": {
                "title": {"tag": "plain_text", "content": "三哥的 GitHub 进度汇报"},
                "template": "turquoise",
            },
            "body": {
                "direction": "vertical",
                "padding": "12px 12px 12px 12px",
                "elements": [
                    {
                        "tag": "table",
                        "columns": [
                            {"data_type": "text", "name": "time", "display_name": "时间", "horizontal_align": "center", "width": "20%"},
                            {"data_type": "text", "name": "desc", "display_name": "项目介绍", "horizontal_align": "left", "width": "35%"},
                            {"data_type": "text", "name": "content", "display_name": "操作", "horizontal_align": "left", "width": "auto"},
                        ],
                        "rows": table_rows,
                        "row_height": "low",
                        "header_style": {"background_style": "grey", "bold": True, "lines": 1},
                        "page_size": min(len(table_rows), 20),
                    },
                    {
                        "tag": "markdown",
                        "content": f"📊 本次共 {len(activities)} 条活动",
                    },
                ],
            },
        },
    }


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
