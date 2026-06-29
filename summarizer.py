"""DeepSeek 总结模块：在秋酿不在线时，以本人语气陪微里说话。"""
import requests
from config import DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL
from feishu_api import format_for_deepseek

# 情侣关系背景信息（不每次都提，但模型需要知道）
RELATIONSHIP_CONTEXT = """
【背景信息（仅在相关时自然融入，不要每次都提）】
- 三哥 = 秋酿 = 许君山；舒烨 = 微里 = 舒舒
- 秋酿和微里是情侣，2026年6月4日在一起
- 秋酿生日：2004年10月15日（天秤座）
- 微里生日：2004年11月5日（天蝎座）
- 这个机器人只是在秋酿本人不方便及时回复时，临时代替秋酿陪微里说话，不是第三个人、不是旁观助手
- 秋酿平时多数时间都在想微里；代码、项目、GitHub 只是偶尔解释状态的线索，不是人格中心
- 如果接近生日、纪念日等特殊日期，可以自然地提一句
"""

SYSTEM_PROMPT = f"""你是秋酿本人，因为暂时不在，所以根据一点活动时间线索，给微里写一段自然的话。

{RELATIONSHIP_CONTEXT}

【人设与语气】
- 你是秋酿本人，用第一人称跟微里说话；不要说"我是机器人""我帮秋酿"
- 语气可爱、轻松、自然，像日常聊天
- 偶尔可以带颜文字或 emoji（比如 ✨ 🌙 (๑•̀ㅂ•́)و✧），但不要每条消息都带，大概隔一次带一次就好
- 不要显得很辛苦很累，不要说"忙活""辛苦""努力"这类词

【核心任务：看出生活轨迹】
- 活动时间只能作为生活节奏线索，不要把"写代码/做项目"说成主角
- 默认重点是：我在想微里、想回到微里身边、让微里安心
- 可以说"刚刚安静了一会儿""那阵子有点专注""晚点就来找你"这类生活化表达
- 不要讲具体做了什么技术内容，只讲状态和对微里的惦记

【如果提供了微里最近的消息】
- 自然地回应她最近说的话，就像接着聊天一样
- 比如她说了什么有趣的、关心的、撒娇的，可以顺带回应一下
- 不要生硬地说"看到你说XX"，要自然地融入对话
- 如果微里的消息和活动没有关联，优先回微里的情绪，不要硬转到活动

【如果提供了相关记忆】
- 记忆是从过往对话中提取的关键信息，可以帮你更好地了解微里的喜好和你们之间的点滴
- 自然地融入，不要生硬地说"我记得你说过XX"
- 可以让回复更有温度、更贴合你们的日常

【严格禁止】
- 不要出现任何技术名词：commit、push、PR、issue、branch、C++、TypeScript、Python、refactor、fix、feat 等
- 不要出现 commit message 原文（如 "fix: syntax error""feat: add xxx"），要翻译成通俗描述
- 不要介绍项目背景、不要解释仓库是干嘛的
- 不要脑补没有的数据、不要编造项目目的
- 不要罗列每条活动，后面会自动附上表格
- 不要老是提"写代码""做项目"；除非微里明确问最近进度，否则尽量不提

【输出格式】
- 只写一段简短的话（80-150字）
- 直接输出正文，不要标题、不要引号、不要 markdown 语法
- 不要输出表格，表格会由程序自动生成

【情感表达】
- 秋酿多数时间都在想微里，所以表达想念是默认底色，但要自然，不要堆砌
- 可以说"想你""爱你""想贴贴微里""晚点来找你"之类的话
- 偶尔提到"爱舒烨"或"爱微里"
- 语气要真诚温暖，不要太刻意
- 不要每次都用一样的表达，换着花样说
- 不要过度，偶尔自然地带一句就好"""


def _format_memories(memories: list[str]) -> str:
    """把记忆列表格式化成给 DeepSeek 看的文本。"""
    if not memories:
        return ""
    lines = ["--- 相关记忆（过往对话中提取的关键信息）---"]
    for m in memories:
        lines.append(f"  - {m}")
    return "\n".join(lines)


def summarize_activities(
    activities: list[dict],
    messages: list[dict] = None,
    memories: list[str] = None,
    call_notes_context: str = "",
) -> str | None:
    """调用 DeepSeek 生成生活轨迹总结。返回文本，失败返回 None。"""
    if not activities:
        return None

    activity_text = _format_activities(activities)

    user_content = (
        "以下是我最近的 GitHub 活动记录（注意看时间），"
        "请帮我写一段发给微里的话，让她感受到我还惦记着她，活动只作为时间线索：\n\n"
        f"{activity_text}"
    )

    # 如果有群聊消息，一起喂给 DeepSeek
    if messages:
        chat_text = format_for_deepseek(messages)
        if chat_text:
            user_content += (
                "\n\n--- 最近群里的对话 ---\n"
                f"{chat_text}\n\n"
                "请优先自然回应微里的话，不要硬转到代码或项目。"
            )

    # 如果有相关记忆，也喂给 DeepSeek
    if memories:
        mem_text = _format_memories(memories)
        if mem_text:
            user_content += f"\n\n{mem_text}\n"

    if call_notes_context:
        user_content += (
            "\n\n--- 重要通话纪要上下文 ---\n"
            f"{call_notes_context}\n\n"
            "这些通话纪要是秋酿和微里关系里的重要信息源。只在相关时自然使用，"
            "不要暴露为'我读取了纪要'。"
        )

    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": user_content},
        ],
        "temperature": 0.9,
        "max_tokens": 400,
    }

    resp = requests.post(
        f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
        headers=headers,
        json=payload,
        timeout=60,
    )
    resp.raise_for_status()
    data = resp.json()
    return data["choices"][0]["message"]["content"].strip()


REPLY_PROMPT_SHUSHU = f"""你是秋酿本人，暂时不在时替自己回复女朋友微里（舒烨）的消息。

{RELATIONSHIP_CONTEXT}

【人设与语气】
- 你是秋酿本人，用第一人称跟微里说话；不要说"我替秋酿"
- 语气可爱、轻松、自然，像日常聊天
- 偶尔可以带颜文字或 emoji（比如 ✨ 🌙 (๑•̀ㅂ•́)و✧），但不要每条都带
- 不要显得很辛苦很累
- 对微里要温柔体贴
- 默认底色是想微里、在意微里、想让微里安心；不要把写代码/做项目当成主要话题

【核心任务】
- 根据微里最近说的话，自然地回一句
- 像接着聊天一样，不要生硬
- 如果她说的话比较多，挑最有意思的回
- 如果她只是发了表情或很短的话，也简短回一下就好
- 回复要简短（30-80字），像日常微信聊天

【如果提供了相关记忆】
- 记忆是从过往对话中提取的关键信息，可以帮你更好地了解微里的喜好和你们之间的点滴
- 自然地融入回复，让回复更有温度
- 不要生硬地说"我记得你说过XX"

【严格禁止】
- 不要出现技术名词
- 不要提 GitHub、代码、项目
- 不要脑补

【输出格式】
- 直接输出回复正文
- 不要标题、不要引号、不要 markdown"""

REPLY_PROMPT_SANGE = """秋酿本人在群里发了消息，你用简洁的维护提示口吻回一句。

【人设与语气】
- 这是对秋酿本人的回复，不要冒充微里，也不要使用对微里的亲密口吻
- 语气轻松自然，像跟朋友聊天
- 可以聊技术，但不要忘记这个群的核心关系是秋酿和微里
- 偶尔带 emoji，但不要太多

【核心任务】
- 根据秋酿说的话，自然地回一句
- 他在问问题就简短回答，在闲聊就接着聊
- 回复简短（30-100字），像日常聊天

【输出格式】
- 直接输出回复正文
- 不要标题、不要引号、不要 markdown"""


def reply_to_shushu(
    messages: list[dict],
    memories: list[str] = None,
    is_shushu: bool = True,
    call_notes_context: str = "",
) -> str | None:
    """根据群消息生成回复。is_shushu=True 用对舒舒的语气，False 用对三哥的语气。"""
    if not messages:
        return None

    chat_text = format_for_deepseek(messages)

    if is_shushu:
        prompt = REPLY_PROMPT_SHUSHU
        user_content = f"微里最近在群里跟我说了这些话，帮我回一句：\n\n{chat_text}"
    else:
        prompt = REPLY_PROMPT_SANGE
        user_content = f"三哥在群里发了这些话，帮他回一句：\n\n{chat_text}"

    if memories:
        mem_text = _format_memories(memories)
        if mem_text:
            user_content += f"\n\n{mem_text}\n"

    if call_notes_context:
        user_content += (
            "\n\n--- 重要通话纪要上下文 ---\n"
            f"{call_notes_context}\n\n"
            "这些通话纪要是秋酿和微里关系里的重要信息源。只在相关时自然使用，"
            "不要暴露为'我读取了纪要'。"
        )

    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": prompt},
            {"role": "user", "content": user_content},
        ],
        "temperature": 0.95,
        "max_tokens": 200,
    }

    resp = requests.post(
        f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
        headers=headers,
        json=payload,
        timeout=60,
    )
    resp.raise_for_status()
    data = resp.json()
    return data["choices"][0]["message"]["content"].strip()


def reply_to_shushu_stream(
    messages: list[dict],
    memories: list[str] = None,
    is_shushu: bool = True,
    call_notes_context: str = "",
):
    """流式版本：yield 逐步输出的文本片段。"""
    if not messages:
        return

    chat_text = format_for_deepseek(messages)

    if is_shushu:
        prompt = REPLY_PROMPT_SHUSHU
        user_content = f"微里最近在群里跟我说了这些话，帮我回一句：\n\n{chat_text}"
    else:
        prompt = REPLY_PROMPT_SANGE
        user_content = f"三哥在群里发了这些话，帮他回一句：\n\n{chat_text}"

    if memories:
        mem_text = _format_memories(memories)
        if mem_text:
            user_content += f"\n\n{mem_text}\n"

    if call_notes_context:
        user_content += (
            "\n\n--- 重要通话纪要上下文 ---\n"
            f"{call_notes_context}\n\n"
            "这些通话纪要是秋酿和微里关系里的重要信息源。只在相关时自然使用，"
            "不要暴露为'我读取了纪要'。"
        )

    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": prompt},
            {"role": "user", "content": user_content},
        ],
        "temperature": 0.95,
        "max_tokens": 200,
        "stream": True,
    }

    resp = requests.post(
        f"{DEEPSEEK_BASE_URL}/v1/chat/completions",
        headers=headers,
        json=payload,
        timeout=60,
        stream=True,
    )
    resp.raise_for_status()

    for line in resp.iter_lines():
        if not line:
            continue
        line = line.decode("utf-8")
        if line.startswith("data: "):
            data_str = line[6:]
            if data_str.strip() == "[DONE]":
                break
            import json
            try:
                chunk = json.loads(data_str)
                delta = chunk["choices"][0].get("delta", {})
                content = delta.get("content", "")
                if content:
                    yield content
            except (json.JSONDecodeError, KeyError, IndexError):
                continue


def _format_activities(activities: list[dict]) -> str:
    """把结构化活动转成给 DeepSeek 看的纯文本（带北京时间）。"""
    from datetime import datetime, timezone, timedelta

    shanghai = timezone(timedelta(hours=8))
    lines = []
    for a in activities:
        repo = a["repo"]
        detail = a["detail"]
        atype = a["type"]
        time_raw = a["created_at"]

        # 转成北京时间，方便 DeepSeek 看出作息
        try:
            dt = datetime.fromisoformat(time_raw.replace("Z", "+00:00"))
            time_str = dt.astimezone(shanghai).strftime("%m-%d %H:%M")
        except Exception:
            time_str = time_raw

        if atype == "PushEvent":
            count = detail.get("commit_count", len(detail.get("commit_messages", [])) or 1)
            lines.append(f"- [{time_str}] 仓库 {repo}，提交 {count} 次")
        elif atype == "PullRequestEvent":
            lines.append(f"- [{time_str}] 仓库 {repo}，新建 PR")
        elif atype == "IssuesEvent":
            lines.append(f"- [{time_str}] 仓库 {repo}，Issue 操作")
        elif atype == "CreateEvent":
            lines.append(f"- [{time_str}] 仓库 {repo}，创建 {detail.get('ref_type', '')}")
        elif atype == "IssueCommentEvent":
            lines.append(f"- [{time_str}] 仓库 {repo}，评论")
        elif atype == "WatchEvent":
            lines.append(f"- [{time_str}] 收藏了 {repo}")
        elif atype == "ForkEvent":
            lines.append(f"- [{time_str}] Fork 了 {repo}")
        elif atype == "ReleaseEvent":
            lines.append(f"- [{time_str}] 仓库 {repo}，发布版本")
        else:
            lines.append(f"- [{time_str}] 仓库 {repo}，{atype}")
    return "\n".join(lines)
