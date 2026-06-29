"""
GitHub Activity Reporter — 主入口

用法:
  python main.py             # 启动 10 分钟轮询循环
  python main.py --once       # 只检查一次并退出
  python main.py --test       # 用模拟数据测试消息卡片（不查 GitHub）
  python main.py --reply-test # 测试"无 GitHub 活动但回复舒舒"逻辑
  python main.py --mem-test   # 测试记忆模块
"""
import sys
import time
from datetime import datetime

from config import (
    GITHUB_USERNAME,
    GITHUB_TOKEN,
    POLL_INTERVAL_SECONDS,
    DRY_RUN,
    FEISHU_READ_MESSAGES,
    MEMORY_ENABLED,
)
from github_client import fetch_github_events, parse_events
from summarizer import summarize_activities, reply_to_shushu
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
    react_to_message,
    format_for_deepseek,
)
from memory import add_memories, search_memories, get_all_memories, format_for_deepseek as format_memories


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


def _get_chat_messages() -> list[dict]:
    """读取群聊消息（三哥 + 舒舒的完整对话），失败返回空列表。"""
    if not FEISHU_READ_MESSAGES:
        return []
    print("  正在读取群聊消息...")
    try:
        messages = fetch_chat_messages()
        if messages:
            print(f"  读到 {len(messages)} 条对话:")
            for m in messages:
                print(f"    [{m['time']}] {m['sender']}: {m['content']}")
        else:
            print("  没有读到消息")
        return messages
    except Exception as e:
        print(f"  读取消息失败: {e}")
        return []


def _save_to_memory(messages: list[dict]):
    """把对话存入记忆。"""
    if not MEMORY_ENABLED or not messages:
        return
    print("  正在存入记忆...")
    add_memories(messages)


def _search_relevant_memories(query: str) -> list[str]:
    """搜索相关记忆。"""
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


# ---- 测试模式 ----

def run_test_mode():
    """用模拟数据测试消息卡片构建。"""
    print("=" * 60)
    print("  TEST MODE - 使用模拟数据测试消息卡片")
    print("=" * 60)

    activities = parse_events(_MOCK_EVENTS, GITHUB_TOKEN)
    print(f"\n模拟活动 ({len(activities)} 条):")
    for a in activities:
        print(f"  [{a['type']}] {a['repo']} - {a['created_at']}")

    messages = _get_chat_messages()
    _save_to_memory(messages)
    memories = _search_relevant_memories("最近在聊什么")

    print("\n正在调用 DeepSeek 生成生活轨迹总结...\n")
    try:
        summary = summarize_activities(activities, messages, memories)
    except Exception as e:
        print(f"  DeepSeek 总结失败: {e}")
        summary = None

    if summary:
        print("-" * 60)
        print("DeepSeek 总结:")
        print("-" * 60)
        print(summary)
        print("-" * 60)
        card = build_message(activities, summary)
        send_card(card)
    else:
        card = build_message(activities)
        send_card(card)


def run_reply_test_mode():
    """测试"无 GitHub 活动但回复舒舒"逻辑。"""
    print("=" * 60)
    print("  REPLY TEST MODE - 测试无 GitHub 活动时回复舒舒")
    print("=" * 60)

    all_messages = _get_chat_messages()
    if not all_messages:
        print("\n  没有读到消息，无法测试")
        return

    _save_to_memory(all_messages)
    memories = _search_relevant_memories("舒舒最近说了什么")

    # 模拟全部都是新消息
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
        print("-" * 60)
        print(reply)
        print("-" * 60)
        send_text(reply)
        last_msg = new_messages[-1]
        if last_msg.get("message_id"):
            react_to_message(last_msg["message_id"], "HEART")


def run_mem_test_mode():
    """测试记忆模块。"""
    print("=" * 60)
    print("  MEMORY TEST MODE - 测试记忆模块")
    print("=" * 60)

    # 先读群聊
    messages = _get_chat_messages()
    if not messages:
        print("\n  没有读到消息")
        return

    # 存入记忆
    print("\n  正在存入记忆...")
    new_mems = add_memories(messages)

    # 搜索记忆
    print("\n  搜索记忆: '舒舒'")
    results = search_memories("舒舒")
    for m in results:
        print(f"    - {m}")

    print("\n  搜索记忆: '机器人'")
    results = search_memories("机器人")
    for m in results:
        print(f"    - {m}")

    # 列出所有记忆
    print("\n  所有记忆:")
    all_mems = get_all_memories()
    for m in all_mems:
        print(f"    - {m}")
    print(f"\n  总计 {len(all_mems)} 条记忆")


# ---- 主逻辑 ----

def check_once():
    """轮询主逻辑：有 GitHub 活动就汇报，没有但舒舒说话了就回复她。"""
    now = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
    print(f"\n[{now}] 正在检查...")

    state = load_state()

    # ---- Step 1: 检查 GitHub 活动 ----
    try:
        raw_events = fetch_github_events(GITHUB_USERNAME, GITHUB_TOKEN)
    except Exception as e:
        print(f"  获取 GitHub 事件失败: {e}")
        raw_events = []

    new_raw = filter_new_events(raw_events, state) if raw_events else []

    if new_raw:
        # ---- 有 GitHub 活动 → 汇报 + 附带对话上下文 + 记忆 ----
        print(f"  发现 {len(new_raw)} 条新 GitHub 活动!")
        activities = parse_events(new_raw, GITHUB_TOKEN)

        print("\n  新活动明细:")
        for a in activities:
            print(f"    [{a['type']}] {a['repo']} - {a['created_at']}")

        messages = _get_chat_messages()
        _save_to_memory(messages)
        memories = _search_relevant_memories("最近在做什么")

        print("\n  正在调用 DeepSeek 生成生活轨迹总结...\n")
        try:
            summary = summarize_activities(activities, messages, memories)
        except Exception as e:
            print(f"  DeepSeek 总结失败: {e}")
            summary = None

        if summary:
            print("-" * 60)
            print("  DeepSeek 总结:")
            print("-" * 60)
            print(summary)
            print("-" * 60)
            card = build_message(activities, summary)
            send_card(card)
        else:
            card = build_message(activities)
            send_card(card)

        update_state(state, new_raw)

        if messages:
            mark_shushu_messages_processed(state, messages)
        return

    # ---- 没有新 GitHub 活动 → 检查是否有新消息 ----
    print("  没有新的 GitHub 活动")

    if not FEISHU_READ_MESSAGES:
        print("  消息读取未开启，跳过")
        return

    all_messages = _get_chat_messages()
    new_messages = filter_new_shushu_messages(all_messages, state)

    if not new_messages:
        print("  也没有新消息，跳过")
        return

    print(f"\n  有 {len(new_messages)} 条新消息:")
    for m in new_messages:
        print(f"    [{m['time']}] {m['sender']}: {m['content']}")

    _save_to_memory(new_messages)
    memories = _search_relevant_memories("舒舒最近说了什么")

    print("\n  正在调用 DeepSeek 生成回复...\n")
    try:
        reply = reply_to_shushu(new_messages, memories)
    except Exception as e:
        print(f"  DeepSeek 回复失败: {e}")
        reply = None

    if reply:
        print("-" * 60)
        print("  DeepSeek 回复:")
        print("-" * 60)
        print(reply)
        print("-" * 60)
        send_text(reply)
        last_msg = new_messages[-1]
        if last_msg.get("message_id"):
            react_to_message(last_msg["message_id"], "HEART")

    mark_shushu_messages_processed(state, new_messages)


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
    print(f"  读取消息: {'开启' if FEISHU_READ_MESSAGES else '关闭'}")
    print(f"  记忆模块: {'开启' if MEMORY_ENABLED else '关闭'}")
    print("=" * 60)

    if "--once" in sys.argv:
        check_once()
        return

    while True:
        try:
            check_once()
        except Exception as e:
            print(f"  运行出错: {e}")
        print(f"\n  等待 {POLL_INTERVAL_SECONDS} 秒后再次检查...")
        time.sleep(POLL_INTERVAL_SECONDS)


if __name__ == "__main__":
    main()
