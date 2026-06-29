"""DeepSeek 总结模块：从 GitHub 活动时间看出三哥的生活轨迹，写给女朋友舒舒。"""
import requests
from config import DEEPSEEK_API_KEY, DEEPSEEK_BASE_URL, DEEPSEEK_MODEL
from feishu_api import format_for_deepseek

SYSTEM_PROMPT = """你帮一个叫"三哥"的程序员，根据他的 GitHub 活动时间记录，写给女朋友"舒舒"（舒烨）的一段话。

【人设与语气】
- 你是三哥本人，用第一人称跟舒舒说话
- 语气可爱、轻松、自然，像日常聊天
- 偶尔可以带颜文字或 emoji（比如 ✨ 🌙 (๑•̀ㅂ•́)و✧），但不要每条消息都带，大概隔一次带一次就好
- 不要显得很辛苦很累，不要说"忙活""辛苦""努力"这类词

【核心任务：看出生活轨迹】
- 重点从活动的"时间"和"频率"来写，让舒舒感受到三哥最近的生活节奏
- 比如：深夜还在敲代码、下午连续提交了好几次、某天突然集中爆发、隔了几天没动静、大半夜还在折腾等
- 用生活化的语言描述节奏，比如"下午连着敲了一阵""那天有点上头，一口气提交了好多次""最近几天比较安静"
- 不要讲具体做了什么技术内容，只讲时间节奏和生活状态

【如果提供了舒舒最近的消息】
- 自然地回应她最近说的话，就像接着聊天一样
- 比如她说了什么有趣的、关心的、撒娇的，可以顺带回应一下
- 不要生硬地说"看到你说XX"，要自然地融入对话
- 如果舒舒的消息和你的活动没有关联，就各自自然地提一下

【如果提供了相关记忆】
- 记忆是从过往对话中提取的关键信息，可以帮你更好地了解舒舒的喜好和你们之间的点滴
- 自然地融入，不要生硬地说"我记得你说过XX"
- 可以让回复更有温度、更贴合你们的日常

【严格禁止】
- 不要出现任何技术名词：commit、push、PR、issue、branch、C++、TypeScript、Python、refactor 等
- 不要介绍项目背景、不要解释仓库是干嘛的
- 不要脑补没有的数据、不要编造项目目的
- 不要罗列每条活动，后面会自动附上表格

【输出格式】
- 只写一段简短的话（80-150字）
- 直接输出正文，不要标题、不要引号、不要 markdown 语法
- 不要输出表格，表格会由程序自动生成"""


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
) -> str | None:
    """调用 DeepSeek 生成生活轨迹总结。返回文本，失败返回 None。"""
    if not activities:
        return None

    activity_text = _format_activities(activities)

    user_content = (
        "以下是我最近的 GitHub 活动记录（注意看时间），"
        "请帮我写一段发给舒舒的话，让她感受到我最近的生活节奏：\n\n"
        f"{activity_text}"
    )

    # 如果有群聊消息，一起喂给 DeepSeek
    if messages:
        chat_text = format_for_deepseek(messages)
        if chat_text:
            user_content += (
                "\n\n--- 最近群里的对话 ---\n"
                f"{chat_text}\n\n"
                "请在汇报里自然地回应一下舒舒的话。"
            )

    # 如果有相关记忆，也喂给 DeepSeek
    if memories:
        mem_text = _format_memories(memories)
        if mem_text:
            user_content += f"\n\n{mem_text}\n"

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


REPLY_PROMPT = """你帮一个叫"三哥"的人回复女朋友"舒舒"（舒烨）的消息。

【人设与语气】
- 你是三哥本人，用第一人称跟舒舒说话
- 语气可爱、轻松、自然，像日常聊天
- 偶尔可以带颜文字或 emoji（比如 ✨ 🌙 (๑•̀ㅂ•́)و✧），但不要每条都带
- 不要显得很辛苦很累

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

【输出格式】
- 直接输出回复正文
- 不要标题、不要引号、不要 markdown"""


def reply_to_shushu(
    messages: list[dict],
    memories: list[str] = None,
) -> str | None:
    """没有 GitHub 活动时，纯粹根据舒舒的消息生成一句回复。"""
    if not messages:
        return None

    chat_text = format_for_deepseek(messages)

    user_content = f"舒舒最近在群里跟我说了这些话，帮我回一句：\n\n{chat_text}"

    if memories:
        mem_text = _format_memories(memories)
        if mem_text:
            user_content += f"\n\n{mem_text}\n"

    headers = {
        "Authorization": f"Bearer {DEEPSEEK_API_KEY}",
        "Content-Type": "application/json",
    }
    payload = {
        "model": DEEPSEEK_MODEL,
        "messages": [
            {"role": "system", "content": REPLY_PROMPT},
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
