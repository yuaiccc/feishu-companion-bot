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

def check_github(receive_id: str = "", force: bool = False, with_summary: bool = False, reply_to: str = ""):
    """检查 GitHub 活动，有新的就推 commit 表格。
    receive_id: 指定发送目标（私聊 chat_id），为空则发到默认群聊
    force: True 时跳过去重，强制拉取最近活动并发送（用户主动查询时用）
    with_summary: True 时用 DeepSeek 生成总结放在表格上方
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

        # 可选：用 DeepSeek 生成总结
        summary = ""
        if with_summary:
            try:
                print("  正在生成总结...", flush=True)
                summary = summarize_activities(activities)
                if summary:
                    print(f"  总结: {summary[:80]}...", flush=True)
            except Exception as e:
                print(f"  生成总结失败: {e}", flush=True)

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
            send_text("最近没有新的 GitHub 活动哦", receive_id=receive_id)


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

        # ---- 第1.5步：检测是否在问 GitHub/commit/最近在干嘛，如果是直接拉真实数据 ----
        github_keywords = [
            "commit", "代码", "提交", "看看最近", "最近做", "github",
            "git", "仓库", "push", "写了什么", "最近写", "活动",
            "看看代码", "代码记录", "编程",
            "在干嘛", "在干啥", "干嘛", "干啥", "忙什么", "忙啥",
            "最近在", "最近搞", "最近弄", "最近看", "最近学",
            "在做什么", "在搞什么", "在弄什么",
        ]
        is_github_query = any(kw in content.lower() for kw in github_keywords)

        if is_github_query and not is_shushu:
            # 三哥问 commit/在干嘛 → 拉 GitHub 数据 + DeepSeek 总结 + 表格（引用回复）
            print(f"  [GitHub查询] 检测到关键词，拉取 GitHub 数据 + 生成总结...", flush=True)
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

        try:
            reply = reply_to_shushu(
                recent_messages, memories,
                is_shushu=is_shushu,
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
