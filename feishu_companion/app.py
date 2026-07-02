"""
飞书陪伴机器人 — 本地长连接主入口

架构：
  - 主线程：飞书长连接（WebSocket）实时接收群消息
  - 子线程：GitHub 轮询（每 10 分钟），有新活动就推 commit 表格
  - 长连接收到消息 → 意图路由 → DeepSeek/工具调用 → 飞书回复

用法:
  python main.py             # 启动长连接 + GitHub 轮询
  python -m feishu_companion # 启动长连接 + GitHub 轮询
  python main.py --once       # 只检查一次 GitHub（不长连接）
  python main.py --test       # 用模拟数据测试消息卡片
  python main.py --reply-test # 测试回复逻辑
  python main.py --mem-test   # 测试记忆模块
"""
import sys
import threading
import time
import importlib
import hashlib
import re
import json
from datetime import datetime

import requests

# 关键：子进程运行时 stdout 默认块缓冲，print 看不到
try:
    sys.stdout.reconfigure(line_buffering=True)
    sys.stderr.reconfigure(line_buffering=True)
except Exception:
    pass

from feishu_companion.config import (
    GITHUB_USERNAME,
    GITHUB_TOKEN,
    GITHUB_PRIVATE_REPOS,
    POLL_INTERVAL_SECONDS,
    PROACTIVE_TOPIC_CHECK_INTERVAL_SECONDS,
    PROACTIVE_TOPIC_ENABLED,
    STREAMING_REPLY_ENABLED,
    STREAMING_REPLY_UPDATE_INTERVAL_SECONDS,
    DRY_RUN,
    FEISHU_READ_MESSAGES,
    FEISHU_STATUS_CHAT_ID,
    STATUS_NOTIFY_COOLDOWN_SECONDS,
    MEMORY_ENABLED,
    MEMORY_CONFIRMATION_ENABLED,
    LOCAL_DAILY_JOB_MODULE,
    LOCAL_DAILY_JOB_RUN_AT,
    DEEPSEEK_API_KEY,
    DEEPSEEK_BASE_URL,
    DEEPSEEK_MODEL,
)
from feishu_companion.github_client import dedupe_events, fetch_github_events, fetch_private_repo_commits, parse_events
from feishu_companion.latency import LatencyTrace
from feishu_companion.summarizer import reply_to_shushu, reply_to_shushu_stream, sanitize_public_text
from feishu_companion.notifier import build_message
from feishu_companion.profile import bot_role, owner_name, target_addressing_instruction, target_name
from feishu_companion.state import (
    load_state,
    save_state,
    filter_new_events,
    update_state,
    filter_new_shushu_messages,
    mark_shushu_messages_processed,
    remember_streaming_reply_context,
    get_streaming_reply_context,
    is_streaming_callback_processed,
    mark_streaming_callback_processed,
    is_memory_confirmation_seen,
    mark_memory_confirmation_seen,
)
from feishu_companion.feishu_api import (
    fetch_chat_messages,
    send_text,
    send_card,
    send_card_message,
    reply_text,
    reply_card,
    react_to_message,
    delete_reaction,
    pick_emoji,
    start_event_listener,
    send_streaming_reply,
    send_streaming_text_reply,
    format_for_deepseek,
    FeishuMessageUnavailable,
)
from feishu_companion.memory import (
    add_memories,
    add_manual_memory,
    clean_memory_store,
    forget_recent_memories_for_texts,
    search_memories,
    get_all_memories,
    format_for_deepseek as format_memories,
)
from feishu_companion.bitable_api import add_records as bitable_add_records
from feishu_companion.local_apps import get_local_status_summary
from feishu_companion.call_notes import build_call_notes_context
from feishu_companion.external_search import (
    answer_external_search,
    remember_search_interaction,
    search_web,
    summarize_search_results,
)
from feishu_companion.passive_assistant import PassiveAssistant
from feishu_companion.health import build_health_card
from feishu_companion.memory_audit import build_memory_audit_card
from feishu_companion.proactive_topic import maybe_send_proactive_topic


# ---- 模拟数据（用于 --test 模式） ----
_MOCK_EVENTS = [
    {
        "id": "mock-1",
        "type": "PushEvent",
        "repo": {"name": "example/feishu-companion-bot"},
        "created_at": "2026-06-29T15:30:00Z",
        "payload": {
            "ref": "refs/heads/main",
            "size": 2,
            "commits": [
                {"message": "feat: add daily report generator"},
                {"message": "fix: correct timezone handling in scheduler"},
            ],
        },
    },
    {
        "id": "mock-2",
        "type": "PullRequestEvent",
        "repo": {"name": "example/lean-utils"},
        "created_at": "2026-06-29T14:10:00Z",
        "payload": {
            "action": "opened",
            "pull_request": {"title": "Add transformer attention proof", "html_url": ""},
        },
    },
    {
        "id": "mock-3",
        "type": "PushEvent",
        "repo": {"name": "example/nn-verify"},
        "created_at": "2026-06-29T12:00:00Z",
        "payload": {
            "ref": "refs/heads/dev",
            "size": 3,
            "commits": [
                {"message": "add Marabou robustness check for conv layer"},
                {"message": "refactor Z3 solver interface"},
                {"message": "update test fixtures"},
            ],
        },
    },
]

_PASSIVE_ASSISTANT = PassiveAssistant()


def _get_chat_messages(chat_id: str = "") -> list[dict]:
    """读取聊天消息（三哥 + 舒舒的完整对话），失败返回空列表。"""
    if not FEISHU_READ_MESSAGES:
        return []
    print(f"  正在读取消息 (chat_id={chat_id[:12]}...)...", flush=True)
    try:
        messages = fetch_chat_messages(chat_id)
        if messages:
            print(f"  读到 {len(messages)} 条对话:", flush=True)
            for m in messages:
                print(f"    [{m['time']}] {m['sender']}: {m['content']}", flush=True)
        else:
            print("  没有读到消息", flush=True)
        return messages
    except Exception as e:
        print(f"  读取消息失败: {e}", flush=True)
        return []


def _save_to_memory(messages: list[dict]):
    if not MEMORY_ENABLED or not messages:
        return
    print("  正在存入记忆...")
    add_memories(messages)


_JSON_OBJECT_RE = re.compile(r"\{.*\}", re.S)


def _memory_candidate_from_message(msg_data: dict) -> dict:
    """Return a DeepSeek-approved memory candidate, or {} when it should not be remembered."""
    content = sanitize_public_text((msg_data.get("content") or "").strip())
    if not content or len(content) < 2 or len(content) > 500:
        return {}
    decision = _decide_memory_candidate_with_deepseek(content, bool(msg_data.get("is_shushu")))
    if not decision.get("remember"):
        return {}
    sender_name = "舒舒" if msg_data.get("is_shushu") else "三哥"
    candidate = sanitize_public_text(decision.get("memory") or f"{sender_name}提到：{content}")
    candidate_hash = hashlib.sha1(candidate.encode("utf-8")).hexdigest()
    return {
        "content": candidate,
        "raw": content,
        "sender": sender_name,
        "hash": candidate_hash,
    }


def _decide_memory_candidate_with_deepseek(content: str, is_shushu: bool) -> dict:
    """Ask DeepSeek whether a message deserves owner-confirmed long-term memory."""
    if not DEEPSEEK_API_KEY:
        return {"remember": False, "memory": "", "reason": "missing_api_key"}
    sender_name = "舒舒" if is_shushu else "三哥"
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {
                "role": "system",
                "content": """你是飞书陪伴机器人的记忆管家。判断一条聊天消息是否值得进入长期记忆候选。
只返回 JSON，不要解释。格式：
{"remember": true/false, "memory": "一句自然中文记忆", "reason": "极短原因"}

判断标准：
- 记住稳定偏好、重要事实、长期习惯、关系边界、称呼方式、明确承诺、重要计划。
- 不记普通寒暄、即时情绪、重复废话、临时闲聊、表情语气、已经过时的细枝末节。
- 不要编造消息里没有的信息。
- memory 要适合给 owner 私聊确认，简短、克制、可长期复用。""",
            },
            {
                "role": "user",
                "content": f"发送者：{sender_name}\n消息：{content}",
            },
        ],
        "temperature": 0,
        "max_tokens": 160,
        "response_format": {"type": "json_object"},
    }
    try:
        resp = requests.post(
            f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
            headers={
                "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
                "Content-Type": "application/json",
            },
            json=payload,
            timeout=20,
        )
        resp.raise_for_status()
        text = resp.json()["choices"][0]["message"]["content"].strip()
        match = _JSON_OBJECT_RE.search(text)
        data = json.loads(match.group(0) if match else text)
        return {
            "remember": bool(data.get("remember")),
            "memory": sanitize_public_text(str(data.get("memory") or "").strip()),
            "reason": sanitize_public_text(str(data.get("reason") or "").strip()),
        }
    except Exception as e:
        print(f"  [记忆确认] DeepSeek 判断失败: {e}", flush=True)
        return {"remember": False, "memory": "", "reason": "deepseek_error"}


def _build_memory_confirmation_card(candidate: dict) -> dict:
    content = sanitize_public_text(candidate.get("content", ""))
    return {
        "msg_type": "interactive",
        "card": {
            "schema": "2.0",
            "config": {"update_multi": True},
            "header": {
                "title": {"tag": "plain_text", "content": "候选记忆"},
                "template": "turquoise",
                "padding": "12px 12px 12px 12px",
            },
            "body": {
                "direction": "vertical",
                "padding": "12px 12px 12px 12px",
                "elements": [
                    {"tag": "markdown", "content": f"这条可能值得长期记住：\n\n{content}"},
                    {
                        "tag": "button",
                        "text": {"tag": "plain_text", "content": "记住"},
                        "type": "primary",
                        "width": "default",
                        "name": "memory_confirm_remember",
                        "value": {"action": "remember_candidate"},
                    },
                    {
                        "tag": "button",
                        "text": {"tag": "plain_text", "content": "不要记"},
                        "type": "danger",
                        "width": "default",
                        "name": "memory_confirm_dismiss",
                        "value": {"action": "dismiss_candidate"},
                    },
                ],
            },
        },
    }


def _maybe_send_memory_confirmation(msg_data: dict) -> None:
    if not (MEMORY_ENABLED and MEMORY_CONFIRMATION_ENABLED and FEISHU_STATUS_CHAT_ID):
        return
    candidate = _memory_candidate_from_message(msg_data)
    if not candidate:
        return
    state = load_state()
    candidate_hash = candidate.get("hash", "")
    if is_memory_confirmation_seen(state, candidate_hash):
        return
    try:
        card = _build_memory_confirmation_card(candidate)
        message_id = send_card_message(card, receive_id=FEISHU_STATUS_CHAT_ID)
        if message_id:
            remember_streaming_reply_context(state, message_id, {
                "candidate_memory": candidate.get("content", ""),
                "candidate_hash": candidate_hash,
            })
            mark_memory_confirmation_seen(state, candidate_hash)
        print("  [记忆确认] 已私聊发送候选记忆", flush=True)
    except Exception as e:
        print(f"  [记忆确认] 发送失败: {e}", flush=True)


def _search_relevant_memories(query: str, audience: str = "target") -> list[str]:
    if not MEMORY_ENABLED:
        return []
    print("  正在搜索相关记忆...")
    memories = search_memories(query, audience=audience)
    if memories:
        print(f"  找到 {len(memories)} 条相关记忆:")
        for m in memories:
            print(f"    - {m}")
    else:
        print("  没有找到相关记忆")
    return memories


def _notify_status(text: str, key: str = "status", force: bool = False) -> None:
    """Push local service status to Sange's p2p chat when configured."""
    if not FEISHU_STATUS_CHAT_ID:
        return
    try:
        now = time.time()
        state = load_state()
        sent_at = state.setdefault("status_notifications", {})
        last = float(sent_at.get(key, 0) or 0)
        if not force and now - last < STATUS_NOTIFY_COOLDOWN_SECONDS:
            return
        send_text(f"本地机器人状态：{text}", receive_id=FEISHU_STATUS_CHAT_ID)
        sent_at[key] = now
        save_state(state)
    except Exception as e:
        print(f"  [状态推送] 失败: {e}", flush=True)


# ---- GitHub 轮询逻辑 ----

def check_github(receive_id: str = "", force: bool = False, with_summary: bool = True, reply_to: str = ""):
    """检查 GitHub 活动，有新的就推 commit 表格。
    receive_id: 指定发送目标（私聊 chat_id），为空则发到默认群聊
    force: True 时跳过去重，强制拉取最近活动并发送（用户主动查询时用）
    with_summary: 保留兼容参数；GitHub 活动卡片不再生成 DeepSeek 总结
    reply_to: 指定 message_id 时用引用回复（在原消息下方回复），否则用 send_card
    """
    state = load_state()

    # 拉公开事件
    try:
        raw_events = fetch_github_events(GITHUB_USERNAME, GITHUB_TOKEN)
    except Exception as e:
        print(f"  获取 GitHub 公开事件失败: {e}")
        raw_events = []

    # 补充 private 仓库 commits
    for repo in GITHUB_PRIVATE_REPOS:
        try:
            private_events = fetch_private_repo_commits(repo, GITHUB_TOKEN)
            if private_events:
                print(f"  从 private 仓库 {repo} 拉到 {len(private_events)} 条提交")
                raw_events.extend(private_events)
        except Exception as e:
            print(f"  拉取 private 仓库 {repo} 失败: {e}")

    raw_events.sort(key=lambda e: e.get("created_at", ""), reverse=True)
    raw_events = dedupe_events(raw_events)

    if force:
        # 强制模式：跳过去重，直接取最近的发
        new_raw = raw_events[:10] if raw_events else []
    else:
        new_raw = filter_new_events(raw_events, state) if raw_events else []

    if new_raw:
        print(f"  发现 {len(new_raw)} 条 GitHub 活动{'(强制)' if force else '(新)'}!", flush=True)
        activities = parse_events(new_raw, GITHUB_TOKEN)
        print("\n  活动明细:", flush=True)
        for a in activities:
            print(f"    [{a['type']}] {a['repo']} - {a['created_at']}", flush=True)

        card = build_message(activities)
        if reply_to:
            reply_card(card, reply_to)
        else:
            send_card(card, receive_id=receive_id)

        # 写入多维表格（持久化记录）
        try:
            bitable_add_records(activities)
        except Exception as e:
            print(f"  [bitable] 写入失败: {e}", flush=True)

        if not force:
            update_state(state, new_raw)
    else:
        print("  没有 GitHub 活动", flush=True)
        if force:
            # force 模式下没有新活动，但仍展示最近的活动记录
            if raw_events:
                print(f"  [force] 展示最近 {min(5, len(raw_events))} 条活动", flush=True)
                recent_raw = raw_events[:5]
                activities = parse_events(recent_raw, GITHUB_TOKEN)
                card = build_message(activities)
                if reply_to:
                    reply_card(card, reply_to)
                else:
                    send_card(card, receive_id=receive_id)
            else:
                send_text("最近确实没有 GitHub 活动记录", receive_id=receive_id)


# ---- LLM 意图判断 ----

_STATUS_KEYWORDS = (
    "在干嘛", "在干啥", "干嘛", "干啥", "忙什么", "忙啥",
    "在做什么", "在搞什么", "在弄什么", "最近怎么样", "最近忙不忙",
    "你在干嘛", "他在干嘛", "三哥在干嘛", "最近活动", "最近在",
    "最近进度", "电脑活动", "窗口",
)
_GITHUB_KEYWORDS = (
    "github", "git hub", "提交", "commit", "代码", "项目进度", "最近提交",
    "代码记录", "仓库", "push", "pr", "issue",
)
_SEARCH_KEYWORDS = (
    "搜索", "搜一下", "查一下", "查查", "帮我查", "网上", "外部",
    "最新", "热门", "热榜", "排行", "排行榜", "新闻", "资讯",
    "b站", "B站", "哔哩", "bilibili", "新番", "番剧", "动漫",
)
_HEALTH_KEYWORDS = (
    "健康检查", "服务状态", "自检", "机器人状态", "状态面板",
    "ollama状态", "openclaw状态", "deepseek状态",
)
_MEMORY_AUDIT_KEYWORDS = (
    "记忆审计", "记忆面板", "记忆状态", "记忆检查", "审计记忆",
)


def _classify_tool_intent(content: str, sender: str = "") -> str:
    """Return a tool intent for optional tool use."""
    import requests as req

    # 短消息快速跳过
    if len(content) <= 2:
        return "none"

    lower = content.lower()
    if any(keyword in content for keyword in _MEMORY_AUDIT_KEYWORDS):
        return "memory_audit"
    if any(keyword in content for keyword in _HEALTH_KEYWORDS) or any(keyword in lower for keyword in _HEALTH_KEYWORDS):
        return "health"
    if any(keyword in lower for keyword in _GITHUB_KEYWORDS):
        return "github"
    if any(keyword in content for keyword in _SEARCH_KEYWORDS) or any(keyword in lower for keyword in _SEARCH_KEYWORDS):
        if "最近活动" not in content and "电脑活动" not in content:
            return "search"
    if any(keyword in content for keyword in _STATUS_KEYWORDS):
        return "status"

    try:
        resp = req.post(
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
                        "content": """判断用户消息是否需要调用工具。只回复 STATUS、GITHUB、SEARCH、HEALTH、MEMORY_AUDIT 或 NONE。

回复 MEMORY_AUDIT：用户要看记忆审计、记忆面板、记忆状态、记忆检查。
回复 HEALTH：用户要看机器人服务状态、健康检查、自检、状态面板、Ollama/OpenClaw/DeepSeek/飞书连接状态。
回复 STATUS：用户在问 owner 当前/最近状态、在干嘛、忙不忙、电脑活动、当前窗口。
回复 GITHUB：用户明确问 GitHub、提交、commit、代码记录、仓库、PR、issue、项目进度。
回复 SEARCH：用户要查外部实时信息、网上资料、热榜、B站、新番、新闻、最近热门内容。
回复 NONE：普通聊天，不需要查工具。

注意：
- "最近活动"如果没有代码/GitHub/提交语境，按 STATUS。
- "在干嘛""最近怎么样"按 STATUS，不要按 GITHUB。
- 只有明确出现代码/GitHub/提交/仓库相关含义才按 GITHUB。
- "最近B站哪些新番热门""查一下新闻""搜索资料"按 SEARCH。

NONE 例子：
- "你好""嗨""hi""早"
- "谢谢""拜拜""晚安"
- "想你""爱你""哈哈""好的"
- "今天天气""吃什么"
""",
                    },
                    {"role": "user", "content": f"用户消息: {content}"},
                ],
                "temperature": 0.0,
                "max_tokens": 8,
            },
            timeout=10,
        )
        result = resp.json()["choices"][0]["message"]["content"].strip().upper()
        if "MEMORY_AUDIT" in result:
            return "memory_audit"
        if "GITHUB" in result:
            return "github"
        if "HEALTH" in result:
            return "health"
        if "SEARCH" in result:
            return "search"
        if "STATUS" in result:
            return "status"
        return "none"
    except Exception as e:
        print(f"  [意图判断] 失败，默认不调用工具: {e}", flush=True)
        return "none"


def _interpret_apps(app_summary: str) -> str:
    """让 DeepSeek 根据本地状态列表，用 profile 语气给目标用户一句状态。"""
    import requests as req
    owner = owner_name()
    target = target_name()
    role = bot_role()
    addressing = target_addressing_instruction()

    try:
        resp = req.post(
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
                        "content": f"""你是{role}，根据{owner}电脑当前本地状态，跟{target}说一句{owner}大概在不在电脑前、可能在做什么。

要求：
- 你不是{owner}本人，不要用{owner}第一人称说话
- 可以说"{owner}刚刚...""{owner}这会儿...""我先帮忙看着呢"
- 语气轻松可爱，像日常聊天
- 1-2句话就好，不要长篇大论
- 把英文名翻译成通俗中文，不要出现英文 app 名
- 根据键鼠空闲时间、锁屏状态、前台应用和窗口标题推测{owner}是否在电脑前；这只是推测，不要说得像确定事实
- 如果状态显示键鼠刚刚有活动，可以说{owner}大概率在电脑前；如果锁屏或空闲很久，可以说可能离开电脑了
- 不要把"写代码/做项目"当成默认重点
- {addressing}
- 如果状态不明确，优先表达"刚刚在忙一下/稍后会回来"，不要硬编技术内容
- 偶尔可以带个 emoji
- 不要说"我在XXX"，要说"{owner}在/可能在XXX"

例子：
输入: 键鼠刚刚有活动（空闲约 12 秒），{owner}大概率在电脑前；正在用 Terminal（main.py），旁边还开着: Claude, Feishu
输出: {owner}刚刚在电脑前处理一点小事，我先帮你看着，等会儿他应该就会回消息～

输入: 键鼠很久没动（空闲约 41 分钟），{owner}大概率不在电脑前；正在用 Feishu
输出: {owner}这会儿可能不在电脑前，我先帮你看着，等他回来再提醒他～

输入: 正在用 Feishu
输出: {owner}像是在看消息，我帮你盯着，等他冒泡就提醒他～
""",
                    },
                    {"role": "user", "content": f"应用状态: {app_summary}"},
                ],
                "temperature": 0.7,
                "max_tokens": 80,
            },
            timeout=15,
        )
        return sanitize_public_text(resp.json()["choices"][0]["message"]["content"].strip())
    except Exception as e:
        print(f"  [应用解读] 失败: {e}", flush=True)
        return ""


def github_poll_loop():
    """GitHub 轮询子线程：每 POLL_INTERVAL_SECONDS 秒检查一次。"""
    while True:
        try:
            now = datetime.now().strftime("%H:%M:%S")
            print(f"\n  [{now}] GitHub 轮询检查...")
            check_github()
        except Exception as e:
            print(f"  GitHub 轮询出错: {e}")
            _notify_status(f"GitHub 轮询出错：{e}", key="github_poll_error")
        time.sleep(POLL_INTERVAL_SECONDS)


def _load_local_daily_job():
    """Load a private local daily extension module when configured."""
    if not LOCAL_DAILY_JOB_MODULE:
        return None
    try:
        return importlib.import_module(LOCAL_DAILY_JOB_MODULE)
    except Exception as e:
        print(f"  [本地每日扩展] 加载失败: {e}", flush=True)
        _notify_status(f"本地每日扩展加载失败：{e}", key="local_daily_job_import_error")
        return None


def _run_local_daily_job(force: bool = False) -> str:
    module = _load_local_daily_job()
    if not module:
        return "未配置本地每日扩展。"
    runner = getattr(module, "run_daily_job", None)
    if not callable(runner):
        raise RuntimeError("本地每日扩展缺少 run_daily_job(force=False) 函数")
    return runner(force=force)


def _preview_local_daily_job() -> str:
    module = _load_local_daily_job()
    if not module:
        return "未配置本地每日扩展。"
    previewer = getattr(module, "preview_daily_job", None)
    if not callable(previewer):
        raise RuntimeError("本地每日扩展缺少 preview_daily_job() 函数")
    return previewer()


def local_daily_job_loop():
    """Run a private local daily extension without committing its implementation."""
    last_checked_minute = ""
    while True:
        try:
            now = datetime.now()
            minute_key = now.strftime("%Y-%m-%d %H:%M")
            if minute_key != last_checked_minute and now.strftime("%H:%M") == LOCAL_DAILY_JOB_RUN_AT:
                last_checked_minute = minute_key
                print(f"\n  [{now.strftime('%H:%M:%S')}] 开始执行本地每日扩展...", flush=True)
                result = _run_local_daily_job()
                print(f"  [本地每日扩展] {result[:200]}", flush=True)
        except Exception as e:
            print(f"  [本地每日扩展] 执行失败: {e}", flush=True)
            _notify_status(f"本地每日扩展执行失败：{e}", key="local_daily_job_error")
        time.sleep(20)


def proactive_topic_loop():
    """主动话题子线程：每天最多一次，只在群聊冷场时 @ 两个人。"""
    while True:
        try:
            result = maybe_send_proactive_topic()
            if result == "sent":
                print("  [主动话题] 已在冷场时发起话题", flush=True)
        except Exception as e:
            print(f"  [主动话题] 检查失败: {e}", flush=True)
            _notify_status(f"主动话题检查失败：{e}", key="proactive_topic_error")
        time.sleep(PROACTIVE_TOPIC_CHECK_INTERVAL_SECONDS)


# ---- 长连接消息处理 ----

def on_message_received(msg_data: dict):
    """长连接收到消息时的回调。三哥和舒舒的消息都回复。"""
    trace = LatencyTrace("chat_reply")
    try:
        # 跳过自己（机器人）发的消息，避免死循环
        sender = msg_data.get("sender", "")
        if sender not in ("三哥", "舒舒"):
            return

        sender_name = "舒舒" if msg_data.get("is_shushu") else "三哥"
        chat_id = msg_data.get("chat_id", "")
        chat_type = msg_data.get("chat_type", "")
        message_id = msg_data.get("message_id", "")
        is_shushu = msg_data.get("is_shushu", True)
        content = msg_data.get("content", "")
        print(f"\n  [回复] 收到{sender_name}消息(来自{chat_type}): {content[:50]}", flush=True)

        # ---- 第1步：加"思考中"表情，让用户知道机器人在处理 ----
        thinking_reaction_id = None
        if message_id:
            try:
                thinking_reaction_id = react_to_message(message_id, "THINKING")
            except FeishuMessageUnavailable as e:
                print(f"  [跳过] 消息不可回复: {e}", flush=True)
                return
            except Exception as e:
                print(f"  [警告] 添加思考表情失败: {e}", flush=True)

        # ---- 第1.5步：判断是否需要调用状态/GitHub 工具 ----
        tool_intent = _classify_tool_intent(content, sender_name)
        if tool_intent == "status":
            app_interpretation = ""
            try:
                app_summary = get_local_status_summary()
                if app_summary:
                    app_interpretation = _interpret_apps(app_summary)
                    if app_interpretation and message_id:
                        try:
                            reply_text(app_interpretation, message_id)
                        except FeishuMessageUnavailable as e:
                            print(f"  [跳过] 消息不可回复: {e}", flush=True)
                            return
                        print(f"  [本地应用] 已发送解读: {app_interpretation[:50]}...", flush=True)
                    else:
                        print(f"  [本地应用] DeepSeek 解读失败", flush=True)
                else:
                    print(f"  [本地应用] 未获取到应用状态", flush=True)
            except Exception as e:
                print(f"  [警告] 获取本地应用失败: {e}", flush=True)

            if thinking_reaction_id and message_id:
                delete_reaction(message_id, thinking_reaction_id)
            if message_id:
                try:
                    react_to_message(message_id, "DONE")
                except FeishuMessageUnavailable:
                    pass
            if app_interpretation:
                return

        if tool_intent == "github":
            print("  [工具调用] 用户明确询问 GitHub/提交，拉取 GitHub 数据...", flush=True)
            try:
                check_github(receive_id=chat_id, force=True, with_summary=True, reply_to=message_id)
            except FeishuMessageUnavailable as e:
                print(f"  [跳过] 消息不可回复: {e}", flush=True)
                if thinking_reaction_id and message_id:
                    delete_reaction(message_id, thinking_reaction_id)
                return
            except Exception as e:
                print(f"  [错误] GitHub 查询失败: {e}", flush=True)
                if message_id:
                    try:
                        reply_text("GitHub 数据拉取失败，稍后再试试", message_id)
                    except FeishuMessageUnavailable as unavailable:
                        print(f"  [跳过] 消息不可回复: {unavailable}", flush=True)
                        return
                else:
                    send_text("GitHub 数据拉取失败，稍后再试试", receive_id=chat_id)

            # 删掉思考表情，加 OK 表情
            if thinking_reaction_id and message_id:
                delete_reaction(message_id, thinking_reaction_id)
            if message_id:
                try:
                    react_to_message(message_id, "DONE")
                except FeishuMessageUnavailable:
                    pass
            return

        if tool_intent == "health":
            print("  [工具调用] 用户请求机器人健康自检...", flush=True)
            try:
                card = build_health_card()
                if message_id:
                    reply_card(card, message_id)
                else:
                    send_card(card, receive_id=chat_id)
            except FeishuMessageUnavailable as e:
                print(f"  [跳过] 消息不可回复: {e}", flush=True)
                if thinking_reaction_id and message_id:
                    delete_reaction(message_id, thinking_reaction_id)
                return
            except Exception as e:
                print(f"  [错误] 健康检查失败: {e}", flush=True)
                if message_id:
                    try:
                        reply_text(f"健康检查失败：{e}", message_id)
                    except FeishuMessageUnavailable:
                        return
            if thinking_reaction_id and message_id:
                delete_reaction(message_id, thinking_reaction_id)
            if message_id:
                try:
                    react_to_message(message_id, "DONE")
                except FeishuMessageUnavailable:
                    pass
            return

        if tool_intent == "memory_audit":
            print("  [工具调用] 用户请求记忆审计面板...", flush=True)
            try:
                audience = "owner" if (not is_shushu and chat_type != "group") else "target"
                card = build_memory_audit_card(audience=audience)
                if message_id:
                    reply_card(card, message_id)
                else:
                    send_card(card, receive_id=chat_id)
            except FeishuMessageUnavailable as e:
                print(f"  [跳过] 消息不可回复: {e}", flush=True)
                if thinking_reaction_id and message_id:
                    delete_reaction(message_id, thinking_reaction_id)
                return
            except Exception as e:
                print(f"  [错误] 记忆审计失败: {e}", flush=True)
                if message_id:
                    try:
                        reply_text(f"记忆审计失败：{e}", message_id)
                    except FeishuMessageUnavailable:
                        return
            if thinking_reaction_id and message_id:
                delete_reaction(message_id, thinking_reaction_id)
            if message_id:
                try:
                    react_to_message(message_id, "DONE")
                except FeishuMessageUnavailable:
                    pass
            return

        if tool_intent == "search":
            print("  [工具调用] 用户询问外部实时信息，调用本地搜索后端...", flush=True)
            try:
                results = search_web(content)
                search_reply = summarize_search_results(content, results)
                if message_id:
                    reply_text(search_reply, message_id)
                else:
                    send_text(search_reply, receive_id=chat_id)
                remember_search_interaction(content, results, actor=sender_name)
            except FeishuMessageUnavailable as e:
                print(f"  [跳过] 消息不可回复: {e}", flush=True)
                if thinking_reaction_id and message_id:
                    delete_reaction(message_id, thinking_reaction_id)
                return
            except Exception as e:
                print(f"  [错误] 外部搜索失败: {e}", flush=True)
                try:
                    fallback = answer_external_search(content)
                except Exception:
                    fallback = "小弟这边外部搜索暂时没接通，等三哥电脑上的本地搜索服务稳一下再查。"
                if message_id:
                    try:
                        reply_text(fallback, message_id)
                    except FeishuMessageUnavailable as unavailable:
                        print(f"  [跳过] 消息不可回复: {unavailable}", flush=True)
                        return
                else:
                    send_text(fallback, receive_id=chat_id)

            if thinking_reaction_id and message_id:
                delete_reaction(message_id, thinking_reaction_id)
            if message_id:
                try:
                    react_to_message(message_id, "DONE")
                except FeishuMessageUnavailable:
                    pass
            return

        # ---- 第2步：读对话上下文 + 搜索记忆 + 生成回复 ----
        recent_messages = []
        with trace.span("read_messages"):
            try:
                recent_messages = _get_chat_messages(chat_id)
            except Exception as e:
                print(f"  [警告] 读取消息失败: {e}", flush=True)

        memories = []
        with trace.span("search_memory"):
            try:
                audience = "target" if is_shushu else "owner"
                memory_query = content or format_for_deepseek(recent_messages[-5:])
                memories = _search_relevant_memories(memory_query, audience=audience)
            except Exception as e:
                print(f"  [警告] 搜索记忆失败: {e}", flush=True)

        call_notes_context = ""
        with trace.span("call_notes"):
            try:
                call_notes_context = build_call_notes_context()
                if call_notes_context:
                    print("  [通话纪要] 已读取上下文", flush=True)
            except Exception as e:
                print(f"  [警告] 读取通话纪要失败: {e}", flush=True)

        reply = ""
        replied_via_stream = False
        if STREAMING_REPLY_ENABLED and chat_id:
            try:
                generator = reply_to_shushu_stream(
                    recent_messages, memories,
                    is_shushu=is_shushu,
                    call_notes_context=call_notes_context,
                )
                first_token_seen = False
                stream_message_id = ""

                def _on_stream_sent(card_id: str, message_id: str) -> None:
                    nonlocal stream_message_id
                    stream_message_id = message_id

                def _trace_stream(gen):
                    nonlocal first_token_seen
                    for chunk in gen:
                        if not first_token_seen:
                            trace.mark("deepseek_first_token")
                            first_token_seen = True
                        yield chunk

                if chat_type == "group":
                    reply = send_streaming_text_reply(
                        _trace_stream(generator),
                        receive_id=chat_id,
                        initial_text="正在输入...",
                        update_interval=STREAMING_REPLY_UPDATE_INTERVAL_SECONDS,
                    )
                else:
                    reply = send_streaming_reply(
                        _trace_stream(generator),
                        title="回复",
                        receive_id=chat_id,
                        initial_text="正在输入...",
                        update_interval=STREAMING_REPLY_UPDATE_INTERVAL_SECONDS,
                        on_sent=_on_stream_sent,
                    )
                trace.mark("reply_sent")
                if stream_message_id and reply:
                    state = load_state()
                    remember_streaming_reply_context(state, stream_message_id, {
                        "chat_id": chat_id,
                        "is_shushu": is_shushu,
                        "messages": recent_messages[-12:],
                        "memories": memories[:8],
                        "call_notes_context": call_notes_context[:1600],
                        "reply": reply,
                    })
                replied_via_stream = bool(reply)
            except Exception as e:
                print(f"  [流式回复] 失败，回退普通回复: {e}", flush=True)
                reply = ""
                replied_via_stream = False

        if not reply:
            try:
                with trace.span("deepseek_full"):
                    reply = reply_to_shushu(
                        recent_messages, memories,
                        is_shushu=is_shushu,
                        call_notes_context=call_notes_context,
                    )
            except Exception as e:
                print(f"  DeepSeek 回复失败: {e}", flush=True)
                import traceback
                traceback.print_exc()
                # 回复失败也要删掉思考表情
                if thinking_reaction_id and message_id:
                    delete_reaction(message_id, thinking_reaction_id)
                return

        # ---- 第3步：发送回复（非流式时引用回复，流式时已经发卡片并逐步更新）----
        if reply:
            print("-" * 60, flush=True)
            print(f"  回复({chat_type}):\n{reply}", flush=True)
            print("-" * 60, flush=True)
            if not replied_via_stream:
                try:
                    if message_id:
                        reply_text(reply, message_id)
                    else:
                        send_text(reply, receive_id=chat_id)
                except FeishuMessageUnavailable as e:
                    print(f"  [跳过] 消息不可回复: {e}", flush=True)
                    if thinking_reaction_id and message_id:
                        delete_reaction(message_id, thinking_reaction_id)
                    return
                except Exception as e:
                    print(f"  [错误] 发送消息失败: {e}", flush=True)
                    import traceback
                    traceback.print_exc()

        # ---- 第4步：删掉"思考中"表情，加上内容匹配的表情 ----
        if thinking_reaction_id and message_id:
            try:
                delete_reaction(message_id, thinking_reaction_id)
            except Exception as e:
                print(f"  [警告] 删除思考表情失败: {e}", flush=True)

        if message_id and reply:
            try:
                emoji = pick_emoji(content, is_shushu=is_shushu)
                react_to_message(message_id, emoji)
            except FeishuMessageUnavailable:
                pass
            except Exception as e:
                print(f"  [警告] 添加内容表情失败: {e}", flush=True)
        if reply:
            _maybe_send_memory_confirmation(msg_data)
        trace.log()

    except Exception as e:
        print(f"  [错误] on_message_received 整体异常: {e}", flush=True)
        import traceback
        traceback.print_exc()
        _notify_status(f"处理飞书消息异常：{e}", key="message_handler_error")
        trace.log()


def on_card_action(action_data: dict) -> str:
    state = load_state()
    callback_id = "|".join([
        action_data.get("message_id", ""),
        action_data.get("operator_id", ""),
        action_data.get("action", ""),
    ])
    if is_streaming_callback_processed(state, callback_id):
        return "这次点击已经处理过了"
    mark_streaming_callback_processed(state, callback_id)

    action = action_data.get("action", "")
    message_id = action_data.get("message_id", "")
    context = get_streaming_reply_context(state, message_id)
    if not context:
        return "这条回复太早了，已经找不到上下文"

    if action == "remember_candidate":
        content = context.get("candidate_memory", "")
        entry = add_manual_memory(content, category="note", visibility="owner_only", source_type="owner_confirmed")
        return "已写入记忆" if entry else "这条内容价值不高，没写入"

    if action == "dismiss_candidate":
        return "好，这条不记"

    chat_id = context.get("chat_id") or action_data.get("chat_id") or FEISHU_CHAT_ID
    messages = context.get("messages") or []
    memories = context.get("memories") or []
    call_notes_context = context.get("call_notes_context") or ""
    is_shushu = bool(context.get("is_shushu", True))
    original_reply = context.get("reply", "")
    latest_user_text = messages[-1].get("content", "") if messages else ""

    if action == "rephrase":
        trace = LatencyTrace("card_rephrase")
        with trace.span("deepseek_full"):
            reply = reply_to_shushu(
                messages,
                memories,
                is_shushu=is_shushu,
                call_notes_context=call_notes_context,
                extra_instruction=f"请把上一版回复换一种更自然的说法，不要增加新事实。上一版：{original_reply}",
            )
        if reply:
            send_text(reply, receive_id=chat_id)
        trace.log()
        return "换了一版"

    if action == "continue":
        trace = LatencyTrace("card_continue")
        with trace.span("deepseek_full"):
            reply = reply_to_shushu(
                messages,
                memories,
                is_shushu=is_shushu,
                call_notes_context=call_notes_context,
                extra_instruction=f"请基于上一版再补充一小句，仍然自然克制，不要超过80字。上一版：{original_reply}",
            )
        if reply:
            send_text(reply, receive_id=chat_id)
        trace.log()
        return "补充了一句"

    if action == "remember":
        content = f"这次对话里用户希望机器人留意：{latest_user_text or original_reply}"
        entry = add_manual_memory(content, category="note", visibility="owner_only", source_type="card_button")
        return "已写入记忆" if entry else "这条内容价值不高，没写入"

    if action == "forget":
        removed = forget_recent_memories_for_texts([latest_user_text, original_reply], minutes=15)
        return f"已尽量移除这次相关记忆 {removed} 条" if removed else "已标记这次不应写入记忆"

    return "未知按钮"


# ---- 测试模式 ----

def run_test_mode():
    print("=" * 60)
    print("  TEST MODE - 使用模拟数据测试消息卡片")
    print("=" * 60)
    activities = parse_events(_MOCK_EVENTS, GITHUB_TOKEN)
    print(f"\n模拟活动 ({len(activities)} 条):")
    for a in activities:
        print(f"  [{a['type']}] {a['repo']} - {a['created_at']}")
    card = build_message(activities)
    send_card(card)


def run_reply_test_mode():
    print("=" * 60)
    print("  REPLY TEST MODE - 测试回复舒舒")
    print("=" * 60)
    all_messages = _get_chat_messages()
    if not all_messages:
        print("\n  没有读到消息，无法测试")
        return
    _save_to_memory(all_messages)
    memories = _search_relevant_memories("舒舒最近说了什么")
    new_messages = all_messages
    print(f"\n  舒舒新消息 ({len(new_messages)} 条):")
    for m in new_messages:
        print(f"    [{m['time']}] {m['content']} (id: {m.get('message_id', '')})")
    print("\n  正在调用 DeepSeek 生成回复...\n")
    try:
        reply = reply_to_shushu(new_messages, memories)
    except Exception as e:
        print(f"  DeepSeek 回复失败: {e}")
        reply = None
    if reply:
        print("-" * 60)
        print("  DeepSeek 回复:")
        print(reply)
        print("-" * 60)
        send_text(reply)
        last_msg = new_messages[-1]
        if last_msg.get("message_id"):
            react_to_message(last_msg["message_id"], "HEART")


def run_mem_test_mode():
    print("=" * 60)
    print("  MEMORY TEST MODE - 测试记忆模块")
    print("=" * 60)
    messages = _get_chat_messages()
    if not messages:
        print("\n  没有读到消息")
        return
    print("\n  正在存入记忆...")
    add_memories(messages)
    print("\n  搜索记忆: '舒舒'")
    results = search_memories("舒舒")
    for m in results:
        print(f"    - {m}")
    print("\n  搜索记忆: '机器人'")
    results = search_memories("机器人")
    for m in results:
        print(f"    - {m}")
    print("\n  所有记忆:")
    all_mems = get_all_memories()
    for m in all_mems:
        print(f"    - {m}")
    print(f"\n  总计 {len(all_mems)} 条记忆")


def run_mem_clean_mode(dry_run: bool = True):
    print("=" * 60)
    print("  MEMORY CLEAN MODE - 记忆清洗")
    print("=" * 60)
    result = clean_memory_store(dry_run=dry_run)
    print(f"  dry_run: {result['dry_run']}")
    print(f"  before: {result['before']}")
    print(f"  after: {result['after']}")
    print(f"  removed: {result['removed']}")
    print(f"  merged: {result['merged']}")
    if result.get("sample_removed"):
        print("  sample_removed:")
        for item in result["sample_removed"]:
            print(f"    - {item[:80]}")


def run_local_daily_job_test_mode():
    print("=" * 60)
    print("  LOCAL DAILY JOB TEST MODE")
    print("=" * 60)
    result = _run_local_daily_job(force=True)
    print(result)


def run_local_daily_job_preview_mode():
    print("=" * 60)
    print("  LOCAL DAILY JOB PREVIEW MODE")
    print("=" * 60)
    result = _preview_local_daily_job()
    print(result)


def run_proactive_topic_test_mode():
    print("=" * 60)
    print("  PROACTIVE TOPIC TEST MODE")
    print("=" * 60)
    result = maybe_send_proactive_topic()
    print(result)


# ---- 主入口 ----

def main():
    if "--test" in sys.argv:
        run_test_mode()
        return
    if "--reply-test" in sys.argv:
        run_reply_test_mode()
        return
    if "--mem-test" in sys.argv:
        run_mem_test_mode()
        return
    if "--mem-clean-preview" in sys.argv:
        run_mem_clean_mode(dry_run=True)
        return
    if "--mem-clean" in sys.argv:
        run_mem_clean_mode(dry_run=False)
        return
    if "--local-daily-job-test" in sys.argv:
        run_local_daily_job_test_mode()
        return
    if "--local-daily-job-preview" in sys.argv:
        run_local_daily_job_preview_mode()
        return
    if "--proactive-topic-test" in sys.argv:
        run_proactive_topic_test_mode()
        return

    print("=" * 60)
    print("  飞书陪伴机器人")
    print(f"  GitHub 用户: {GITHUB_USERNAME}")
    mode = "DRY RUN (本地测试, 不推送飞书)" if DRY_RUN else "PRODUCTION (推送飞书)"
    print(f"  模式: {mode}")
    print(f"  轮询间隔: {POLL_INTERVAL_SECONDS} 秒")
    print(f"  长连接: 开启")
    print(f"  读取消息: {'开启' if FEISHU_READ_MESSAGES else '关闭'}")
    print(f"  记忆模块: {'开启' if MEMORY_ENABLED else '关闭'}")
    print(f"  本地每日扩展: {'开启' if LOCAL_DAILY_JOB_MODULE else '关闭'}")
    print(f"  主动话题: {'开启' if PROACTIVE_TOPIC_ENABLED else '关闭'}")
    print("=" * 60)
    _notify_status("已启动/重启，本地长连接正在保持在线。", key="startup")

    # --once 模式：只检查一次 GitHub，不启动长连接
    if "--once" in sys.argv:
        check_github()
        return

    # 启动 GitHub 轮询子线程
    poll_thread = threading.Thread(target=github_poll_loop, daemon=True)
    poll_thread.start()
    print("  GitHub 轮询线程已启动")

    if LOCAL_DAILY_JOB_MODULE:
        daily_thread = threading.Thread(target=local_daily_job_loop, daemon=True)
        daily_thread.start()
        print(f"  本地每日扩展线程已启动: {LOCAL_DAILY_JOB_RUN_AT}")

    if PROACTIVE_TOPIC_ENABLED:
        proactive_thread = threading.Thread(target=proactive_topic_loop, daemon=True)
        proactive_thread.start()
        print("  主动话题线程已启动")

    # 主线程：启动飞书长连接（阻塞）
    print("  启动飞书长连接事件监听...")
    try:
        start_event_listener(
            on_message_received=on_message_received,
            on_passive_message=_PASSIVE_ASSISTANT.on_message,
            on_card_action=on_card_action,
        )
    except Exception as e:
        print(f"  [致命] 飞书长连接退出: {e}", flush=True)
        _notify_status(f"飞书长连接退出：{e}", key="websocket_exit", force=True)
        raise


if __name__ == "__main__":
    main()
