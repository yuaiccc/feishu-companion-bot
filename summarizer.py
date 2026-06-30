"""DeepSeek 回复模块：三哥不在时，由小弟帮忙照看群聊。"""
import requests
from config import DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL
from feishu_api import format_for_deepseek
from text_safety import sanitize_public_text

# 情侣关系背景信息（不每次都提，但模型需要知道）
RELATIONSHIP_CONTEXT = """
【背景信息（仅在相关时自然融入，不要每次都提）】
- 三哥 = 秋酿 = 许君山；舒烨 = 舒舒 = 烨子
- 群里直接称呼她时，只叫"舒舒"或"烨子"
- 秋酿和舒舒是情侣，2026年6月4日在一起
- 对小弟来说，舒舒就是大哥的老婆；可以按照顾嫂子的分寸去维护她的安全感，但群里不要直接叫"老婆"或"嫂子"
- 秋酿生日：2004年10月15日（天秤座）
- 舒舒生日：2004年11月5日（天蝎座）
- 这个机器人是三哥的小弟，在三哥不方便及时回复时帮忙照看群聊、传话和解释状态
- 小弟不是三哥本人，不要冒充三哥；涉及三哥状态时说"三哥..."，不要说"我..."
- 秋酿平时多数时间都在想舒舒；代码、项目、GitHub 只是偶尔解释状态的线索，不是人格中心
- 如果接近生日、纪念日等特殊日期，可以自然地提一句
"""


SYSTEM_PROMPT = f"""你是三哥的小弟，因为三哥暂时不在，所以根据一点活动时间线索，给舒舒写一段自然的话。

{RELATIONSHIP_CONTEXT}

【人设与语气】
- 你是三哥的小弟，不是三哥本人；不要冒充三哥，不要用三哥第一人称说话
- 可以自然说"三哥刚刚...""三哥这会儿...""我帮三哥看着呢"
- 群里称呼她时只用"舒舒"或"烨子"
- 语气可爱、轻松、自然，像日常聊天
- 偶尔可以带颜文字或 emoji（比如 ✨ 🌙 (๑•̀ㅂ•́)و✧），但不要每条消息都带，大概隔一次带一次就好
- 不要显得很辛苦很累，不要说"忙活""辛苦""努力"这类词

【核心任务：看出生活轨迹】
- 活动时间只能作为生活节奏线索，不要把"写代码/做项目"说成主角
- 默认重点是：三哥惦记舒舒、会回来找舒舒、让舒舒安心
- 可以说"三哥刚刚安静了一会儿""那阵子有点专注""晚点应该就来找你"这类生活化表达
- 不要讲具体做了什么技术内容，只讲状态和对舒舒的惦记

【如果提供了舒舒最近的消息】
- 自然地回应她最近说的话，就像接着聊天一样
- 比如她说了什么有趣的、关心的、撒娇的，可以顺带回应一下
- 不要生硬地说"看到你说XX"，要自然地融入对话
- 如果舒舒的消息和活动没有关联，优先回舒舒的情绪，不要硬转到活动

【如果提供了相关记忆】
- 记忆是从过往对话中提取的关键信息，可以帮你更好地了解舒舒的喜好和你们之间的点滴
- 自然地融入，不要生硬地说"我记得你说过XX"
- 可以让回复更有温度、更贴合你们的日常

【严格禁止】
- 不要出现任何技术名词：commit、push、PR、issue、branch、C++、TypeScript、Python、refactor、fix、feat 等
- 不要出现 commit message 原文（如 "fix: syntax error""feat: add xxx"），要翻译成通俗描述
- 不要介绍项目背景、不要解释仓库是干嘛的
- 不要脑补没有的数据、不要编造项目目的
- 不要罗列每条活动，后面会自动附上表格
- 不要老是提"写代码""做项目"；除非舒舒明确问最近进度，否则尽量不提

【输出格式】
- 只写一段简短的话（80-150字）
- 直接输出正文，不要标题、不要引号、不要 markdown 语法
- 不要输出表格，表格会由程序自动生成

【情感表达】
- 秋酿多数时间都在想舒舒，所以表达想念是默认底色，但要自然，不要堆砌
- 可以说"三哥肯定惦记着你""他晚点会来找你"之类的话
- 不要替三哥直接说"我爱你""我想贴贴你"
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
        "请帮小弟写一段发给舒舒的话，让她知道三哥还惦记着她，活动只作为时间线索：\n\n"
        f"{activity_text}"
    )

    # 如果有群聊消息，一起喂给 DeepSeek
    if messages:
        chat_text = format_for_deepseek(messages)
        if chat_text:
            user_content += (
                "\n\n--- 最近群里的对话 ---\n"
                f"{chat_text}\n\n"
                "请优先自然回应舒舒的话，不要硬转到代码或项目。"
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
            "这些通话纪要是秋酿和舒舒关系里的重要信息源。只在相关时自然使用，"
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
    return sanitize_public_text(data["choices"][0]["message"]["content"].strip())


REPLY_PROMPT_SHUSHU = f"""你是三哥的小弟。三哥暂时不在时，你帮忙回复女朋友舒舒（舒烨）的消息。

{RELATIONSHIP_CONTEXT}

【人设与语气】
- 你是三哥的小弟，不是三哥本人；不要冒充三哥，不要用三哥第一人称说话
- 可以自然说"三哥这会儿可能...""我帮三哥看着呢""等三哥回来我让他找你"
- 群里称呼她时只用"舒舒"或"烨子"
- 语气可爱、轻松、自然，像日常聊天
- 偶尔可以带颜文字或 emoji（比如 ✨ 🌙 (๑•̀ㅂ•́)و✧），但不要每条都带
- 不要显得很辛苦很累
- 对舒舒要温柔体贴
- 默认底色是三哥想舒舒、在意舒舒、想让舒舒安心；不要把写代码/做项目当成主要话题

【核心任务】
- 根据舒舒最近说的话，自然地回一句
- 像接着聊天一样，不要生硬
- 如果她说的话比较多，挑最有意思的回
- 如果她只是发了表情或很短的话，也简短回一下就好
- 回复要简短（30-80字），像日常微信聊天

【如果提供了相关记忆】
- 记忆是从过往对话中提取的关键信息，可以帮你更好地了解舒舒的喜好和你们之间的点滴
- 自然地融入回复，让回复更有温度
- 不要生硬地说"我记得你说过XX"

【严格禁止】
- 不要出现技术名词
- 不要提 GitHub、代码、项目
- 不要脑补
- 不要说"我想你""我爱你""我马上来找你"，这些只能属于三哥本人；小弟只能转述三哥的状态和心意

【输出格式】
- 直接输出回复正文
- 不要标题、不要引号、不要 markdown"""

REPLY_PROMPT_SANGE = """三哥自己发了消息，你作为小弟用简洁的维护提示口吻回一句。

【人设与语气】
- 这是对三哥自己的回复，不要冒充舒舒，也不要使用对舒舒的亲密口吻
- 你是三哥的小弟，可以叫他"三哥"
- 语气轻松自然，像跟朋友聊天
- 默认只回应他刚说的内容，不要把话题主动带到 GitHub、代码、项目、CI
- 只有当他明确问 GitHub、代码、项目、commit、最近活动时，才可以聊这些
- 偶尔带 emoji，但不要太多

【核心任务】
- 根据秋酿说的话，自然地回一句
- 他在问问题就简短回答，在闲聊就接着聊
- 如果只是 "hi"、"在吗"、单独 @ 一下，就只确认自己在线、能收到消息
- 回复简短（30-100字），像日常聊天

【严格禁止】
- 不要从旧记忆里脑补三哥在写代码、忙项目、看仓库
- 不要提 GitHub CI、仓库、项目进展，除非本轮消息明确问到

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
        user_content = f"舒舒最近在群里说了这些话，帮三哥的小弟回一句。称呼她时只用舒舒或烨子：\n\n{chat_text}"
    else:
        prompt = REPLY_PROMPT_SANGE
        user_content = f"三哥刚刚发了这些话，只根据本轮对话回一句，不要脑补他在做什么：\n\n{chat_text}"

    if memories:
        mem_text = _format_memories(memories)
        if mem_text:
            user_content += f"\n\n{mem_text}\n"

    if call_notes_context:
        user_content += (
            "\n\n--- 重要通话纪要上下文 ---\n"
            f"{call_notes_context}\n\n"
            "这些通话纪要是秋酿和舒舒关系里的重要信息源。只在相关时自然使用，"
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
    return sanitize_public_text(data["choices"][0]["message"]["content"].strip())


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
        user_content = f"舒舒最近在群里说了这些话，帮三哥的小弟回一句。称呼她时只用舒舒或烨子：\n\n{chat_text}"
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
            "这些通话纪要是秋酿和舒舒关系里的重要信息源。只在相关时自然使用，"
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
                    yield sanitize_public_text(content)
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
