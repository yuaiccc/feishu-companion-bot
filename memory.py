"""记忆模块：基于 mem0 的长期记忆，永不过期。
- 使用 DeepSeek 作为 LLM（提取关键信息、生成记忆）
- 使用本地 Qdrant 文件存储（无需 Docker）
- 所有记忆永久保留
"""
import os
from config import (
    DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL,
    MEMORY_ENABLED, MEMORY_DIR,
)

_memory = None


def _init_memory():
    """初始化 mem0，配置 DeepSeek + 本地 Qdrant。"""
    global _memory
    if _memory is not None:
        return _memory

    MEMORY_DIR.mkdir(parents=True, exist_ok=True)

    from mem0 import Memory

    config = {
        # 用 DeepSeek 做记忆提取（OpenAI 兼容接口）
        "llm": {
            "provider": "openai",
            "config": {
                "model": DEEPSEEK_MODEL,
                "api_key": DEEPSEEK_API_KEY,
                "openai_base_url": f"{DEEPSEEK_BASE_URL}/v1",
                "temperature": 0.1,
            },
        },
        # 用本地文件存储的 Qdrant（无需 Docker）
        "vector_store": {
            "provider": "qdrant",
            "config": {
                "path": str(MEMORY_DIR / "qdrant"),
                "collection_name": "shushu_memories",
            },
        },
        # 默认 embedder 用 OpenAI，如果没有 OpenAI key 则用 HuggingFace 本地模型
        "embedder": {
            "provider": "huggingface",
            "config": {
                "model": "sentence-transformers/all-MiniLM-L6-v2",
            },
        },
    }

    _memory = Memory.from_config(config)
    return _memory


def add_memories(messages: list[dict], user_id: str = "shushu_chat") -> list:
    """把对话存入记忆。mem0 会自动提取关键信息，不存原文。
    返回新创建的记忆条目列表。
    """
    if not MEMORY_ENABLED or not messages:
        return []

    try:
        mem = _init_memory()

        # 转成 mem0 需要的 messages 格式
        chat_messages = []
        for m in messages:
            role = "assistant" if m.get("sender") == "三哥" else "user"
            chat_messages.append({
                "role": role,
                "content": f"[{m['time']}] {m['sender']}: {m['content']}",
            })

        result = mem.add(chat_messages, user_id=user_id)
        new_memories = result.get("results", [])
        if new_memories:
            print(f"  [memory] 新增 {len(new_memories)} 条记忆:")
            for m in new_memories:
                mem_text = m.get("memory", "")
                event = m.get("event", "")
                print(f"    [{event}] {mem_text}")
        return new_memories
    except Exception as e:
        print(f"  [memory] 存储记忆失败: {e}")
        return []


def search_memories(query: str, user_id: str = "shushu_chat", top_k: int = 5) -> list[str]:
    """搜索相关记忆。返回记忆文本列表。"""
    if not MEMORY_ENABLED:
        return []

    try:
        mem = _init_memory()
        results = mem.search(query, filters={"user_id": user_id}, top_k=top_k)
        memories = []
        for r in results.get("results", []):
            mem_text = r.get("memory", "")
            if mem_text:
                memories.append(mem_text)
        return memories
    except Exception as e:
        print(f"  [memory] 搜索记忆失败: {e}")
        return []


def get_all_memories(user_id: str = "shushu_chat") -> list[str]:
    """获取所有记忆（用于展示或调试）。"""
    if not MEMORY_ENABLED:
        return []

    try:
        mem = _init_memory()
        results = mem.get_all(filters={"user_id": user_id})
        memories = []
        for r in results.get("results", []):
            mem_text = r.get("memory", "")
            if mem_text:
                memories.append(mem_text)
        return memories
    except Exception as e:
        print(f"  [memory] 获取记忆失败: {e}")
        return []


def format_for_deepseek(memories: list[str]) -> str:
    """把记忆格式化成给 DeepSeek 看的文本。"""
    if not memories:
        return ""
    lines = ["--- 相关记忆 ---"]
    for m in memories:
        lines.append(f"  - {m}")
    return "\n".join(lines)
