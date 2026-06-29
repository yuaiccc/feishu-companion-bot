"""飞书 SDK 客户端：基于 lark-oapi，支持长连接事件订阅 + API 调用。
- 读消息、发消息（webhook）、表情回复
- 长连接实时接收群消息事件
"""
import json
import lark_oapi as lark
from lark_oapi.api.im.v1 import (
    ListMessageRequest,
    CreateMessageRequest,
    CreateMessageRequestBody,
    CreateMessageReactionRequest,
    CreateMessageReactionRequestBody,
    DeleteMessageReactionRequest,
    ReplyMessageRequest,
    ReplyMessageRequestBody,
    Reaction,
)

from config import (
    FEISHU_APP_ID, FEISHU_APP_SECRET,
    FEISHU_CHAT_ID, FEISHU_SHUSHU_OPEN_ID, DRY_RUN,
)

# ---- SDK Client ----

_client = None


def _get_client() -> lark.Client:
    """获取 lark-oapi client（单例）。"""
    global _client
    if _client is not None:
        return _client
    _client = (
        lark.Client.builder()
        .app_id(FEISHU_APP_ID)
        .app_secret(FEISHU_APP_SECRET)
        .build()
    )
    return _client


# ---- 读消息 ----

def fetch_chat_messages(chat_id: str = "", limit: int = 20) -> list[dict]:
    """读取群里最近的完整对话（三哥 + 舒舒）。
    返回 [{message_id, time, content, sender, is_shushu}]，按时间倒序。
    """
    chat_id = chat_id or FEISHU_CHAT_ID
    if not chat_id:
        print("  [feishu_api] 缺少 chat_id，跳过")
        return []

    client = _get_client()
    req = (
        ListMessageRequest.builder()
        .container_id(chat_id)
        .container_id_type("chat")
        .sort_type("ByCreateTimeDesc")
        .page_size(min(limit, 50))
        .build()
    )
    resp = client.im.v1.message.list(req)

    if not resp.success():
        print(f"  [feishu_api] 读取消息失败: code={resp.code} msg={resp.msg}")
        return []

    items = resp.data.items if resp.data and resp.data.items else []
    messages = []
    for item in items:
        sender = item.sender
        sender_id = sender.id if sender else ""
        sender_type = sender.sender_type if sender else ""

        # 跳过系统消息
        if sender_type != "user":
            continue

        msg_type = item.msg_type
        body_content = item.body.content if item.body else ""
        content = _extract_text(msg_type, body_content)
        if not content:
            continue

        time_str = _format_time(item.create_time)
        is_shushu = sender_id == FEISHU_SHUSHU_OPEN_ID

        messages.append({
            "message_id": item.message_id,
            "time": time_str,
            "content": content,
            "sender": "舒舒" if is_shushu else "三哥",
            "is_shushu": is_shushu,
        })

    return messages


def fetch_shushu_messages(chat_id: str = "", limit: int = 20) -> list[dict]:
    """只读舒舒的消息（兼容旧接口）。"""
    all_msgs = fetch_chat_messages(chat_id, limit)
    return [m for m in all_msgs if m["is_shushu"]]


# ---- 发消息（通过 SDK，以应用机器人身份）----

def send_text(text: str, receive_id: str = "") -> bool:
    """通过 SDK 机器人发文本消息。dry_run 时只打印。"""
    target = receive_id or FEISHU_CHAT_ID
    if DRY_RUN:
        print("\n  " + "=" * 56)
        print("  DRY RUN - 以下消息通过机器人发送（不会真正发送）")
        print("=" * 56)
        print(f"  [机器人回复] {text}")
        print("=" * 56 + "\n")
        return True

    client = _get_client()
    body = (
        CreateMessageRequestBody.builder()
        .msg_type("text")
        .content(json.dumps({"text": text}))
        .receive_id(target)
        .build()
    )
    req = (
        CreateMessageRequest.builder()
        .receive_id_type("chat_id")
        .request_body(body)
        .build()
    )
    resp = client.im.v1.message.create(req)
    if not resp.success():
        raise RuntimeError(f"发送文本消息失败: code={resp.code} msg={resp.msg}")
    return True


def reply_text(text: str, message_id: str) -> bool:
    """回复某条消息（引用回复，显示在原消息下方）。dry_run 时只打印。"""
    if DRY_RUN:
        print(f"\n  [回复消息] {text[:50]} -> {message_id[:20]}...", flush=True)
        return True

    client = _get_client()
    body = (
        ReplyMessageRequestBody.builder()
        .msg_type("text")
        .content(json.dumps({"text": text}))
        .build()
    )
    req = (
        ReplyMessageRequest.builder()
        .message_id(message_id)
        .request_body(body)
        .build()
    )
    resp = client.im.v1.message.reply(req)
    if not resp.success():
        raise RuntimeError(f"回复消息失败: code={resp.code} msg={resp.msg}")
    return True


def reply_card(card: dict, message_id: str) -> bool:
    """回复某条消息（卡片形式，引用回复）。dry_run 时只打印。"""
    target_card = card["card"]
    if DRY_RUN:
        print(f"\n  [回复卡片] -> {message_id[:20]}...", flush=True)
        return True

    client = _get_client()
    body = (
        ReplyMessageRequestBody.builder()
        .msg_type("interactive")
        .content(json.dumps(target_card, ensure_ascii=False))
        .build()
    )
    req = (
        ReplyMessageRequest.builder()
        .message_id(message_id)
        .request_body(body)
        .build()
    )
    resp = client.im.v1.message.reply(req)
    if not resp.success():
        raise RuntimeError(f"回复卡片失败: code={resp.code} msg={resp.msg}")
    return True


def send_card(card: dict, receive_id: str = "") -> bool:
    """通过 SDK 机器人发卡片消息。dry_run 时只打印。
    card 格式: {"msg_type": "interactive", "card": {...}}
    """
    target = receive_id or FEISHU_CHAT_ID
    if DRY_RUN:
        print("\n  " + "=" * 56)
        print("  DRY RUN - 以下卡片通过机器人发送（不会真正发送）")
        print("=" * 56)
        print(json.dumps(card, ensure_ascii=False, indent=2))
        print("=" * 56 + "\n")
        return True

    client = _get_client()
    card_content = json.dumps(card["card"])
    body = (
        CreateMessageRequestBody.builder()
        .msg_type("interactive")
        .content(card_content)
        .receive_id(target)
        .build()
    )
    req = (
        CreateMessageRequest.builder()
        .receive_id_type("chat_id")
        .request_body(body)
        .build()
    )
    resp = client.im.v1.message.create(req)
    if not resp.success():
        raise RuntimeError(f"发送卡片消息失败: code={resp.code} msg={resp.msg}")
    return True


# ---- 表情回复 ----

def react_to_message(message_id: str, emoji_type: str = "THUMBSUP") -> str | None:
    """给某条消息添加表情回复，返回 reaction_id（用于后续删除）。
    失败返回 None。dry_run 时返回 "dry-run"。
    """
    if DRY_RUN:
        print(f"  [表情回复] {emoji_type} -> {message_id[:20]}...", flush=True)
        return "dry-run"

    client = _get_client()
    reaction = Reaction.builder().emoji_type(emoji_type).build()
    body = CreateMessageReactionRequestBody.builder().reaction_type(reaction).build()
    req = (
        CreateMessageReactionRequest.builder()
        .message_id(message_id)
        .request_body(body)
        .build()
    )
    resp = client.im.v1.message_reaction.create(req)
    if resp.success() and resp.data:
        rid = resp.data.reaction_id
        print(f"  [表情回复] {emoji_type} -> reaction_id={rid}", flush=True)
        return rid
    print(f"  [表情回复] 失败: code={resp.code} msg={resp.msg}", flush=True)
    return None


def delete_reaction(message_id: str, reaction_id: str) -> bool:
    """删除某条消息上的表情回复。"""
    if not reaction_id or reaction_id == "dry-run":
        return True
    client = _get_client()
    req = (
        DeleteMessageReactionRequest.builder()
        .message_id(message_id)
        .reaction_id(reaction_id)
        .build()
    )
    resp = client.im.v1.message_reaction.delete(req)
    if resp.success():
        print(f"  [表情删除] reaction_id={reaction_id}", flush=True)
        return True
    print(f"  [表情删除] 失败: code={resp.code} msg={resp.msg}", flush=True)
    return False


def pick_emoji(content: str, is_shushu: bool = False) -> str:
    """根据消息内容选择合适的表情，充分利用飞书丰富的表情类型。"""
    text = content.lower().strip()

    # ---- 舒舒的消息：温柔甜蜜系 ----
    if is_shushu:
        # 甜蜜/想念/爱意
        if any(w in text for w in ["想你", "爱你", "喜欢", "亲", "抱", "贴", "宝贝", "老公", "老婆", "么么", "mua"]):
            return "KISS"
        if any(w in text for w in ["想你", "想", "好久没", "什么时候"]):
            return "OBSESSED"
        if any(w in text for w in ["爱你", "爱了", "心动", "心动了"]):
            return "LOVE"

        # 开心/开心
        if any(w in text for w in ["哈哈", "开心", "好棒", "嘻嘻", "嘿嘿", "咯咯", "笑死", "乐死"]):
            return "LAUGH"
        if any(w in text for w in ["耶", "好耶", "太好了", "棒", "厉害"]):
            return "JOYFUL"

        # 难过/委屈/撒娇
        if any(w in text for w in ["难过", "哭", "难受", "委屈", "呜", "不想", "哼", "生气", "气死"]):
            return "COMFORT"
        if any(w in text for w in ["累了", "累", "困", "想睡", "不想动"]):
            return "SOB"
        if any(w in text for w in ["哼", "不理你", "讨厌", "坏"]):
            return "SCOWL"
        if any(w in text for w in ["怕", "害怕", "吓", "不敢"]):
            return "INNOCENTSMILE"

        # 惊讶
        if any(w in text for w in ["哇", "天呐", "不是吧", "真的吗", "不会吧"]):
            return "WOW"

        # 鼓励/加油
        if any(w in text for w in ["加油", "你可以", "相信你", "冲"]):
            return "JIAYI"

        # 感谢
        if any(w in text for w in ["谢谢", "感谢", "谢了"]):
            return "THANKS"

        # 默认给飞吻
        return "SMOOCH"

    # ---- 三哥的消息：丰富多样 ----

    # 开心/搞笑/赞同
    if any(w in text for w in ["哈哈", "搞笑", "笑死", "乐死", "233", "lol"]):
        return "LOL"
    if any(w in text for w in ["牛", "厉害", "棒", "好耶", "nice", "cool", "强"]):
        return "PRAISE"
    if any(w in text for w in ["开心", "高兴", "爽", "nice"]):
        return "JOYFUL"
    if any(w in text for w in ["嘿嘿", "嘻嘻", "嘿嘿嘿"]):
        return "SMIRK"
    if any(w in text for w in ["机智", "聪明", "我太强了"]):
        return "SMART"
    if any(w in text for w in ["得意", "骄傲", "牛逼"]):
        return "PROUD"
    if any(w in text for w in ["有趣", "好玩", "有意思"]):
        return "WITTY"

    # 感谢
    if any(w in text for w in ["谢谢", "感谢", "谢了", "thanks", "thx", "3q"]):
        return "THANKS"

    # 疲惫/抱怨/吐槽
    if any(w in text for w in ["累了", "累", "困", "想睡", "不想动", "躺平"]):
        return "DULL"
    if any(w in text for w in ["难", "烦", "唉", "emo", "抑郁"]):
        return "SOB"
    if any(w in text for w in ["崩溃", "草", "操", "靠", "无语", "服了"]):
        return "FACEPALM"
    if any(w in text for w in ["气死", "生气", "怒", "火大"]):
        return "SLAP"
    if any(w in text for w in ["尴尬", "社死", "丢人"]):
        return "EMBARRASSED"
    if any(w in text for w in ["晕", "无语", "不是吧", "离谱"]):
        return "DIZZY"

    # 惊讶/震惊
    if any(w in text for w in ["哇", "天呐", "卧槽", "woc", "我去"]):
        return "WOW"
    if any(w in text for w in ["什么", "怎么会", "不可能"]):
        return "WHAT"
    if any(w in text for w in ["真的吗", "真假", "seriously"]):
        return "TRICK"

    # 思考/困惑
    if any(w in text for w in ["为什么", "怎么", "啥意思", "搞不懂", "不理解"]):
        return "THINKING"
    if any(w in text for w in ["想", "考虑", "纠结", "犹豫"]):
        return "THINKING"

    # 问问题
    if any(w in text for w in ["？", "?", "啥", "什么", "如何", "哪里", "哪个"]):
        return "THUMBSUP"

    # 赞同/确认
    if any(w in text for w in ["好的", "好", "行", "可以", "ok", "嗯", "对", "没问题"]):
        return "DONE"
    if any(w in text for w in ["是的", "对", "没错", "正确"]):
        return "OK"

    # 庆祝/鼓励
    if any(w in text for w in ["完成", "搞定", "done", "完工", "收工"]):
        return "DONE"
    if any(w in text for w in ["加油", "冲", "干", "干他", "上"]):
        return "MUSCLE"
    if any(w in text for w in ["庆祝", "恭喜", "太棒了"]):
        return "APPLAUSE"

    # GitHub/编程相关
    if any(w in text for w in ["commit", "代码", "提交", "github", "git", "push"]):
        return "STRIVE"
    if any(w in text for w in ["bug", "报错", "错误", "崩溃"]):
        return "ERROR"
    if any(w in text for w in ["部署", "上线", "发布"]):
        return "DONE"

    # 撒娇/亲昵
    if any(w in text for w in ["亲", "么么", "mua", "贴贴"]):
        return "KISS"

    # 其他
    if any(w in text for w in ["拜拜", "再见", "晚安", "睡了"]):
        return "WAVE"
    if any(w in text for w in ["666", "牛掰", "太强了"]):
        return "FINGERHEART"
    if any(w in text for w in ["吃", "饿", "宵夜", "外卖"]):
        return "DROOL"
    if any(w in text for w in ["钱", "发工资", "穷"]):
        return "MONEY"
    if any(w in text for w in ["无聊", "没意思"]):
        return "TEARS"
    if any(w in text for w in ["厉害", "respect", "瑞思拜"]):
        return "FISTBUMP"

    # 默认：根据消息长度随机一点
    import random
    defaults = ["THUMBSUP", "SMILE", "WINK", "YEAH", "BLUSH"]
    return random.choice(defaults)


# ---- 流式卡片 ----

import requests as _requests

_OPEN_API_BASE = "https://open.feishu.cn/open-apis"


def _get_token() -> str:
    """获取 tenant_access_token。"""
    resp = _requests.post(
        f"{_OPEN_API_BASE}/auth/v3/tenant_access_token/internal",
        json={"app_id": FEISHU_APP_ID, "app_secret": FEISHU_APP_SECRET},
        timeout=30,
    )
    return resp.json()["tenant_access_token"]


def create_streaming_card(title: str = "回复中...") -> str:
    """创建一个流式更新模式的卡片实体，返回 card_id。
    卡片包含一个 markdown 组件，element_id='reply_text'。
    """
    token = _get_token()

    card_json = {
        "schema": "2.0",
        "config": {
            "streaming_mode": True,
            "update_multi": True,
            "summary": {"content": "[生成中...]"},
        },
        "header": {
            "title": {"tag": "plain_text", "content": title},
            "template": "turquoise",
        },
        "body": {
            "elements": [
                {
                    "tag": "markdown",
                    "content": "",
                    "element_id": "reply_text",
                }
            ]
        },
    }

    resp = _requests.post(
        f"{_OPEN_API_BASE}/cardkit/v1/cards",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        json={
            "type": "card_json",
            "data": json.dumps(card_json, ensure_ascii=False),
        },
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"创建卡片实体失败: {data}")
    card_id = data["data"]["card_id"]
    print(f"  [流式卡片] 创建成功 card_id={card_id}")
    return card_id


def send_card_entity(card_id: str, receive_id: str = "") -> bool:
    """发送卡片实体到群聊或私聊。"""
    target = receive_id or FEISHU_CHAT_ID
    client = _get_client()
    content = json.dumps({"type": "card", "data": {"card_id": card_id}})
    body = (
        CreateMessageRequestBody.builder()
        .msg_type("interactive")
        .content(content)
        .receive_id(target)
        .build()
    )
    req = (
        CreateMessageRequest.builder()
        .receive_id_type("chat_id")
        .request_body(body)
        .build()
    )
    resp = client.im.v1.message.create(req)
    if not resp.success():
        raise RuntimeError(f"发送卡片实体失败: code={resp.code} msg={resp.msg}")
    return True


def update_streaming_text(card_id: str, full_text: str, sequence: int) -> bool:
    """流式更新卡片中的文本内容（打字机效果）。
    full_text 是累积的完整文本，sequence 必须严格递增。
    """
    token = _get_token()
    resp = _requests.put(
        f"{_OPEN_API_BASE}/cardkit/v1/cards/{card_id}/elements/reply_text/content",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        json={
            "content": full_text,
            "sequence": sequence,
        },
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        print(f"  [流式卡片] 更新失败: {data}")
        return False
    return True


def stop_streaming(card_id: str, sequence: int) -> bool:
    """关闭流式更新模式。"""
    token = _get_token()
    resp = _requests.patch(
        f"{_OPEN_API_BASE}/cardkit/v1/cards/{card_id}/settings",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        json={
            "settings": json.dumps({
                "config": {"streaming_mode": False}
            }),
            "sequence": sequence,
        },
        timeout=30,
    )
    data = resp.json()
    if data.get("code") != 0:
        print(f"  [流式卡片] 关闭流式失败: {data}")
        return False
    print(f"  [流式卡片] 流式更新已关闭")
    return True


def send_streaming_reply(full_text_generator, title: str = "回复", receive_id: str = "") -> str:
    """完整的流式回复流程：
    1. 创建流式卡片实体
    2. 发送到群聊
    3. 逐步更新文本（打字机效果）
    4. 关闭流式模式
    返回完整文本。
    """
    if DRY_RUN:
        full_text = ""
        print("\n  " + "=" * 56)
        print("  DRY RUN - 流式回复（不会真正发送）")
        print("=" * 56)
        for chunk in full_text_generator:
            full_text += chunk
            print(chunk, end="", flush=True)
        print("\n" + "=" * 56 + "\n")
        return full_text

    # 1. 创建卡片
    card_id = create_streaming_card(title=title)

    # 2. 发送卡片到目标聊天
    send_card_entity(card_id, receive_id=receive_id)

    # 3. 流式更新文本
    import time
    full_text = ""
    sequence = 1
    for chunk in full_text_generator:
        full_text += chunk
        update_streaming_text(card_id, full_text, sequence)
        sequence += 1
        # 控制更新频率，避免超过 10 QPS 限制
        time.sleep(0.15)

    # 4. 最终更新一次完整文本
    if full_text:
        update_streaming_text(card_id, full_text, sequence)
        sequence += 1

    # 5. 关闭流式模式
    stop_streaming(card_id, sequence)

    return full_text


# ---- 长连接事件订阅 ----

def start_event_listener(on_message_received=None):
    """启动长连接事件监听，实时接收群消息。
    on_message_received: 回调函数，参数为消息 dict {message_id, time, content, sender, is_shushu}
    """
    from lark_oapi.api.im.v1 import P2ImMessageReceiveV1

    _processed_ids = set()

    def _handle_message(data: P2ImMessageReceiveV1) -> None:
        """处理收到的消息事件。"""
        nonlocal _processed_ids
        try:
            event = data.event
            msg = event.message
            sender_info = event.sender

            # ---- 双重去重：event_id + message_id ----
            # 飞书"至少发送一次"策略可能重发，用 event_id 和 message_id 双重判断
            header = data.header if hasattr(data, "header") else None
            event_id = header.event_id if header and hasattr(header, "event_id") else ""
            message_id = msg.message_id if hasattr(msg, "message_id") else ""
            dedup_key = f"{event_id}:{message_id}" if event_id else message_id

            if dedup_key in _processed_ids:
                print(f"  [长连接] 跳过重复消息: event_id={event_id} message_id={message_id}", flush=True)
                return
            _processed_ids.add(dedup_key)
            # 保留最近 500 条，防止无限增长
            if len(_processed_ids) > 500:
                _processed_ids = set(list(_processed_ids)[-300:])

            print(f"\n  [长连接] 收到新消息: event_id={event_id} message_id={message_id}", flush=True)

            sender_id = sender_info.sender_id.open_id if sender_info and sender_info.sender_id else ""
            sender_type = sender_info.sender_type if sender_info else ""

            # 只处理用户消息，跳过机器人自己的消息避免死循环
            if sender_type != "user":
                print(f"  [长连接] 跳过非用户消息: sender_type={sender_type}")
                return

            # 调试：打印 chat_type
            chat_type = msg.chat_type if hasattr(msg, "chat_type") else ""
            chat_id = msg.chat_id if hasattr(msg, "chat_id") else ""
            print(f"  [长连接][调试] sender_type={sender_type} chat_type={chat_type} chat_id={chat_id} sender_id={sender_id}", flush=True)

            # 群聊消息：只有 @了机器人才回复；私聊（p2p）全部回复
            is_mentioned = False
            if chat_type == "p2p":
                is_mentioned = True
            else:
                # 检查 mentions 里有没有机器人
                mentions = msg.mentions if hasattr(msg, "mentions") and msg.mentions else []
                for m in mentions:
                    mentioned_type = m.mentioned_type if hasattr(m, "mentioned_type") else ""
                    # mentioned_type="app" 表示 @了机器人
                    if mentioned_type == "app":
                        is_mentioned = True
                        break

            if not is_mentioned:
                print(f"  [长连接] 群聊消息未@机器人，跳过", flush=True)
                return

            msg_type = msg.message_type
            content_raw = msg.content if msg.content else ""
            content = _extract_text(msg_type, content_raw)
            if not content:
                return

            # 去掉 @机器人 的标记（@_user_1 之类）
            import re
            content = re.sub(r'@_user_\d+', '', content).strip()
            if not content:
                content = "只叫了你一声"

            time_str = _format_time(msg.create_time)
            is_shushu = sender_id == FEISHU_SHUSHU_OPEN_ID

            msg_data = {
                "message_id": msg.message_id,
                "time": time_str,
                "content": content,
                "sender": "舒舒" if is_shushu else "三哥",
                "is_shushu": is_shushu,
                "chat_id": chat_id,
                "chat_type": chat_type,
            }

            print(f"\n  [长连接] 收到消息: [{time_str}] {msg_data['sender']}: {content[:50]}", flush=True)

            if on_message_received:
                on_message_received(msg_data)

        except Exception as e:
            print(f"  [长连接] 处理消息异常: {e}", flush=True)

    event_handler = (
        lark.EventDispatcherHandler.builder("", "")
        .register_p2_im_message_receive_v1(_handle_message)
        .build()
    )

    cli = lark.ws.Client(
        FEISHU_APP_ID,
        FEISHU_APP_SECRET,
        event_handler=event_handler,
        log_level=lark.LogLevel.DEBUG,
    )

    print("  [长连接] 启动飞书长连接事件监听...")
    cli.start()


# ---- 工具函数 ----

def _extract_text(msg_type: str, content_raw: str) -> str:
    """从消息体提取纯文本。"""
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
            if isinstance(v, dict):
                if v.get("title"):
                    texts.append(v["title"])
                for para in v.get("content", []):
                    if isinstance(para, list):
                        for elem in para:
                            if isinstance(elem, dict) and elem.get("tag") == "text":
                                texts.append(elem.get("text", ""))
        return " ".join(texts).strip()
    if msg_type == "interactive":
        return "[卡片消息]"
    if msg_type == "sticker":
        return "[表情]"
    return ""


def _format_time(create_time: str) -> str:
    """格式化时间（飞书返回毫秒级时间戳）。"""
    if not create_time:
        return ""
    try:
        ts = int(create_time)
        if ts > 1e12:
            ts = ts // 1000
        from datetime import datetime, timezone, timedelta
        shanghai = timezone(timedelta(hours=8))
        dt = datetime.fromtimestamp(ts, tz=shanghai)
        return dt.strftime("%m-%d %H:%M")
    except (ValueError, TypeError):
        return create_time


def format_for_deepseek(messages: list[dict]) -> str:
    """把对话格式化成给 DeepSeek 看的文本。"""
    if not messages:
        return ""
    lines = []
    for m in messages:
        lines.append(f"  [{m['time']}] {m['sender']}说: {m['content']}")
    return "\n".join(lines)
