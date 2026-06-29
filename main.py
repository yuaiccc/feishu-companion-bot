"""
GitHub Activity Reporter — 主入口

架构：
  - 主线程：飞书长连接（WebSocket）实时接收群消息
  - 子线程：GitHub 轮询（每 10 分钟），有新活动就推 commit 表格
  - 长连接收到舒舒消息 → DeepSeek 回复 → webhook 发送 + ❤️ 表情

用法:
  python main.py             # 启动长连接 + GitHub 轮询
  python main.py --once       # 只检查一次 GitHub（不长连接）
  python main.py --test       # 用模拟数据测试消息卡片
  python main.py --reply-test # 测试回复舒舒逻辑
  python main.py --mem-test   # 测试记忆模块
"""
import sys
import threading
import time
from datetime import datetime

# 关键：子进程运行时 stdout 默认块缓冲，print 看不到
try:
    sys.stdout.reconfigure(line_buffering=True)
    sys.stderr.reconfigure(line_buffering=True)
except Exception:
    pass

from config import (
    GITHUB_USERNAME,
    GITHUB_TOKEN,
    GITHUB_PRIVATE_REPOS,
    POLL_INTERVAL_SECONDS,
    DRY_RUN,
    FEISHU_READ_MESSAGES,
    MEMORY_ENABLED,
    DEEPSEEK_API_KEY,
    DEEPSEEK_BASE_URL,
    DEEPSEEK_MODEL,
)
from github_client import fetch_github_events, fetch_private_repo_commits, parse_events
from summarizer import summarize_activities, reply_to_shushu, reply_to_shushu_stream
from notifier import build_message
from state import (
    load_state,
    filter_new_events,
    update_state,
    filter_new_shushu_messages,
    mark_shushu_messages_processed,
)
from feishu_api import (
    fetch_chat_messages,
    send_text,
    send_card,
    reply_text,
    reply_card,
    react_to_message,
    delete_reaction,
    pick_emoji,
    start_event_listener,
    send_streaming_reply,
    format_for_deepseek,
)
from memory import add_memories, search_memories, get_all_memories, format_for_deepseek as format_memories
from bitable_api import add_records as bitable_add_records
from local_apps import get_app_summary
from call_notes import build_call_notes_context


# ---- 模拟数据（用于 --test 模式） ----
_MOCK_EVENTS = [
    {
        "id": "mock-1",
        "type": "PushEvent",
        "repo": {"name": "yuaiccc/project-history"},
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
        "repo": {"name": "yuaiccc/lean-utils"},
        "created_at": "2026-06-29T14:10:00Z",
        "payload": {
            "action": "opened",
            "pull_request": {"title": "Add transformer attention proof", "html_url": ""},
        },
    },
    {
        "id": "mock-3",
        "type": "PushEvent",
        "repo": {"name": "yuaiccc/nn-verify"},
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


def _search_relevant_memories(query: str) -> list[str]:
    if not MEMORY_ENABLED:
        return []
    print("  正在搜索相关记忆...")
    memories = search_memories(query)
    if memories:
        print(f"  找到 {len(memories)} 条相关记忆:")
        for m in memories:
            print(f"    - {m}")
    else:
        print("  没有找到相关记忆")
    return memories


# ---- GitHub 轮询逻辑 ----

def check_github(receive_id: str = "", force: bool = False, with_summary: bool = True, reply_to: str = ""):
    """检查 GitHub 活动，有新的就推 commit 表格。
    receive_id: 指定发送目标（私聊 chat_id），为空则发到默认群聊
    force: True 时跳过去重，强制拉取最近活动并发送（用户主动查询时用）
    with_summary: 保留兼容参数；活动卡片现在始终尝试 DeepSeek 总结
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

        summary = _generate_activity_summary(activities)

        card = build_message(activities, summary=summary)
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
                summary = _generate_activity_summary(activities)
                card = build_message(activities, summary=summary)
                if reply_to:
                    reply_card(card, reply_to)
                else:
                    send_card(card, receive_id=receive_id)
            else:
                send_text("最近确实没有 GitHub 活动记录", receive_id=receive_id)


def _generate_activity_summary(activities: list[dict]) -> str:
    """活动卡片必须带总结：优先 DeepSeek，失败时给显式兜底文本。"""
    try:
        print("  正在生成 DeepSeek 总结...", flush=True)
        call_notes_context = build_call_notes_context()
        summary = summarize_activities(activities, call_notes_context=call_notes_context)
        if summary:
            print(f"  总结: {summary[:80]}...", flush=True)
            return summary
        print("  [警告] DeepSeek 总结为空，使用兜底总结", flush=True)
    except Exception as e:
        print(f"  [警告] DeepSeek 总结失败，使用兜底总结: {e}", flush=True)
    return _fallback_activity_summary(activities)


def _fallback_activity_summary(activities: list[dict]) -> str:
    repos = sorted({a.get("repo", "") for a in activities if a.get("repo")})
    repo_text = "、".join(r.split("/")[-1] for r in repos[:2]) if repos else "电脑这边"
    return (
        f"微里，秋酿这边刚刚有 {len(activities)} 条新动态，主要是 {repo_text} 这边留了一点记录。"
        "DeepSeek 总结刚刚没生成出来，但我还是先把时间线放下面给你看，心里一直惦记着你。"
    )


# ---- LLM 意图判断 ----

def _should_use_tools(content: str, sender: str = "") -> bool:
    """用 DeepSeek 判断这条消息是否需要调用工具（GitHub 数据 + 本地应用状态）。
    返回 True 表示需要调用工具，False 表示纯聊天即可。
    """
    import requests as req

    # 短消息快速跳过
    if len(content) <= 2:
        return False

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
                        "content": """判断用户消息是否在询问"某人最近在做什么""在干嘛""忙什么"等需要了解状态的问题。

以下情况回复 YES（需要查工具）：
- "在干嘛""在干什么""忙什么""最近在做什么"
- "最近怎么样""最近忙不忙"
- "看看代码""最近提交""commit""代码记录"
- "最近活动""最近进度"
- "你在干嘛""他在干嘛""三哥在干嘛"

以下情况回复 NO（纯聊天，不需要工具）：
- "你好""嗨""hi""早"
- "谢谢""拜拜""晚安"
- "想你""爱你""哈哈""好的"
- "今天天气""吃什么"

只回复 YES 或 NO。""",
                    },
                    {"role": "user", "content": f"用户消息: {content}"},
                ],
                "temperature": 0.0,
                "max_tokens": 5,
            },
            timeout=10,
        )
        result = resp.json()["choices"][0]["message"]["content"].strip().upper()
        return "YES" in result
    except Exception as e:
        print(f"  [意图判断] 失败，默认不调用工具: {e}", flush=True)
        return False


def _interpret_apps(app_summary: str) -> str:
    """让 DeepSeek 根据本地应用列表，用秋酿本人语气给微里一句状态。"""
    import requests as req

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
                        "content": """你是秋酿本人，根据电脑当前打开的应用列表，跟女朋友微里（舒烨）说一句你在干什么。

要求：
- 用第一人称，语气轻松可爱，像日常聊天
- 1-2句话就好，不要长篇大论
- 把英文名翻译成通俗中文，不要出现英文 app 名
- 根据前台应用和窗口标题推测秋酿可能在做什么，但不要把"写代码/做项目"当成默认重点
- 秋酿平时多数时间都在想微里；如果状态不明确，优先表达"刚刚在忙一下/在想你/马上来找你"，不要硬编技术内容
- 偶尔可以带个 emoji
- 不要说"我推测你在"，直接说"我在XXX"
- 可以自然带一句想微里、爱微里、等会儿来找微里；不要每次都用同一句

例子：
输入: 正在用 Terminal（main.py），旁边还开着: Claude, Feishu
输出: 刚刚在电脑前处理一点小事，但脑子里还是在想微里，等会儿来贴贴你～

输入: 正在用 Feishu
输出: 在看消息呢，看看微里有没有找我～
""",
                    },
                    {"role": "user", "content": f"应用状态: {app_summary}"},
                ],
                "temperature": 0.7,
                "max_tokens": 80,
            },
            timeout=15,
        )
        return resp.json()["choices"][0]["message"]["content"].strip()
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
        time.sleep(POLL_INTERVAL_SECONDS)


# ---- 长连接消息处理 ----

def on_message_received(msg_data: dict):
    """长连接收到消息时的回调。三哥和舒舒的消息都回复。"""
    try:
        # 存入记忆
        if MEMORY_ENABLED:
            try:
                _save_to_memory([msg_data])
            except Exception as e:
                print(f"  [警告] 存入记忆失败: {e}", flush=True)

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
            except Exception as e:
                print(f"  [警告] 添加思考表情失败: {e}", flush=True)

        # ---- 第1.5步：用 LLM 判断是否需要调用工具（GitHub 数据 + 本地应用状态）----
        needs_tools = _should_use_tools(content, sender_name)
        if needs_tools and not is_shushu:
            # 先发本地应用解读（DeepSeek 推测三哥在干什么）
            app_interpretation = ""
            try:
                app_summary = get_app_summary()
                if app_summary:
                    app_interpretation = _interpret_apps(app_summary)
                    if app_interpretation and message_id:
                        reply_text(app_interpretation, message_id)
                        print(f"  [本地应用] 已发送解读: {app_interpretation[:50]}...", flush=True)
                    else:
                        print(f"  [本地应用] DeepSeek 解读失败", flush=True)
                else:
                    print(f"  [本地应用] 未获取到应用状态", flush=True)
            except Exception as e:
                print(f"  [警告] 获取本地应用失败: {e}", flush=True)

            # 再拉 GitHub 数据 + 总结表格
            print(f"  [工具调用] LLM 判断需要调用工具，拉取 GitHub 数据 + 生成总结...", flush=True)
            try:
                check_github(receive_id=chat_id, force=True, with_summary=True, reply_to=message_id)
            except Exception as e:
                print(f"  [错误] GitHub 查询失败: {e}", flush=True)
                if message_id:
                    reply_text("GitHub 数据拉取失败，稍后再试试", message_id)
                else:
                    send_text("GitHub 数据拉取失败，稍后再试试", receive_id=chat_id)

            # 删掉思考表情，加 OK 表情
            if thinking_reaction_id and message_id:
                delete_reaction(message_id, thinking_reaction_id)
            if message_id:
                react_to_message(message_id, "DONE")
            return

        # ---- 第2步：读对话上下文 + 搜索记忆 + 生成回复 ----
        recent_messages = []
        try:
            recent_messages = _get_chat_messages(chat_id)
        except Exception as e:
            print(f"  [警告] 读取消息失败: {e}", flush=True)

        memories = []
        try:
            memories = _search_relevant_memories(f"{sender_name}最近说了什么")
        except Exception as e:
            print(f"  [警告] 搜索记忆失败: {e}", flush=True)

        call_notes_context = ""
        try:
            call_notes_context = build_call_notes_context()
            if call_notes_context:
                print("  [通话纪要] 已读取上下文", flush=True)
        except Exception as e:
            print(f"  [警告] 读取通话纪要失败: {e}", flush=True)

        try:
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

        # ---- 第3步：发送回复（引用回复，显示在原消息下方）----
        if reply:
            print("-" * 60, flush=True)
            print(f"  回复({chat_type}):\n{reply}", flush=True)
            print("-" * 60, flush=True)
            try:
                if message_id:
                    reply_text(reply, message_id)
                else:
                    send_text(reply, receive_id=chat_id)
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
            except Exception as e:
                print(f"  [警告] 添加内容表情失败: {e}", flush=True)

    except Exception as e:
        print(f"  [错误] on_message_received 整体异常: {e}", flush=True)
        import traceback
        traceback.print_exc()


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

    print("=" * 60)
    print("  GitHub Activity Reporter")
    print(f"  GitHub 用户: {GITHUB_USERNAME}")
    mode = "DRY RUN (本地测试, 不推送飞书)" if DRY_RUN else "PRODUCTION (推送飞书)"
    print(f"  模式: {mode}")
    print(f"  轮询间隔: {POLL_INTERVAL_SECONDS} 秒")
    print(f"  长连接: 开启")
    print(f"  读取消息: {'开启' if FEISHU_READ_MESSAGES else '关闭'}")
    print(f"  记忆模块: {'开启' if MEMORY_ENABLED else '关闭'}")
    print("=" * 60)

    # --once 模式：只检查一次 GitHub，不启动长连接
    if "--once" in sys.argv:
        check_github()
        return

    # 启动 GitHub 轮询子线程
    poll_thread = threading.Thread(target=github_poll_loop, daemon=True)
    poll_thread.start()
    print("  GitHub 轮询线程已启动")

    # 主线程：启动飞书长连接（阻塞）
    print("  启动飞书长连接事件监听...")
    start_event_listener(on_message_received=on_message_received)


if __name__ == "__main__":
    main()
