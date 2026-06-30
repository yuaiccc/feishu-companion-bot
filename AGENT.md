# AGENT.md — 项目维护指南

> 本文档供 AI Agent 维护本项目时参考。请先通读全文再动手改代码。

## 项目概述

这是一个飞书**群聊应用**机器人（不是私聊机器人），配置在飞书群聊中。它的人设是"三哥的小弟"：秋酿/三哥（许君山，学生）本人不在时，帮忙照看群聊、给"舒舒/烨子"（舒烨）传话和解释状态，并在必要时汇报 GitHub 活动。它不是秋酿分身，不要冒充秋酿本人。

**重要**：机器人是飞书群聊中的应用，群聊消息只有 @机器人 才回复，私聊全部回复。其他 Agent 维护时请注意这个区别。

**核心功能：**
1. 本地 WebSocket 长连接，实时接收飞书消息并以三哥小弟的语气回复
2. GitHub 活动监控，有新 commit 自动推送飞书卡片
3. 本地应用检测（AppleScript），在被问到时告诉舒舒秋酿大概在做什么
4. DeepSeek 生成自然、亲密、偏陪伴的回复
5. GitHub Actions 云端轮询，电脑关机时也能 5 分钟内兜底回复

**人设优先级：**
- 秋酿/三哥 = 许君山；舒舒/烨子 = 舒烨。
- 群里直接称呼她时，只叫"舒舒"或"烨子"。
- 机器人是三哥的小弟，不是秋酿分身，也不要冒充秋酿本人。
- 对舒舒说话时用"三哥..."来转述状态和心意，不要用三哥第一人称说"我想你/我在干嘛"。
- 秋酿平时多数时间都在想舒舒。代码、项目、GitHub 只是状态线索，不是默认话题。
- 除非舒舒明确问代码/进度/GitHub，不要老是提"写代码""做项目"。
- 对舒舒回复时，优先回应她的情绪、问题和亲密关系；对秋酿本人回复时，不要冒充舒舒。

**两套运行模式：**
- **本地模式**：`python main.py`，WebSocket 实时连接，秒级回复
- **云端模式**：GitHub Actions cron 每 5 分钟轮询一次 REST API
- **macOS 常驻模式**：`launchd/com.xujunshan.github-activity-bot.plist` 通过 `caffeinate -dimsu` 运行 `.venv/bin/python main.py`，用于电脑开机时保持本地长连接在线

## 项目位置

```
/Users/xujunshan/Code/github_activity_generator
```

GitHub 仓库：`yuaiccc/project-history`（私有）

## 文件结构

```
.
├── main.py              # 入口：启动 WebSocket + GitHub 轮询线程
├── config.py            # 配置加载（从 .env 读）
├── feishu_api.py        # 飞书 API 封装（发消息、读消息、表情、卡片、WebSocket）
├── github_client.py     # GitHub Events API + 私有仓库 commits 拉取
├── summarizer.py        # DeepSeek 总结/回复生成（含所有 prompt）
├── notifier.py          # 飞书卡片表格构建
├── local_apps.py        # AppleScript 读取前台应用（只读）
├── memory.py            # 轻量 JSON 记忆系统
├── bitable_api.py       # 飞书多维表格写入
├── state.py             # 活动去重状态管理
├── actions_runner.py    # GitHub Actions 云端轮询脚本（无 WebSocket）
├── launchd/             # macOS LaunchAgent 常驻配置
├── .github/workflows/
│   └── bot.yml          # GitHub Actions workflow（5 分钟 cron）
├── .env                 # 本地密钥（gitignore，不提交）
├── .env.example         # 配置模板
└── memory_data/         # 记忆 + 多维表格状态持久化
    ├── memories.json
    └── bitable_state.json
```

## 配置

### .env 文件（本地运行）

位置：`/Users/xujunshan/Code/github_activity_generator/.env`

```env
# DeepSeek
DEEPSEEK_API_KEY=sk-xxx
DEEPSEEK_BASE_URL=https://api.deepseek.com
DEEPSEEK_MODEL=deepseek-chat

# 飞书自建应用
FEISHU_APP_ID=cli_xxx
FEISHU_APP_SECRET=xxx
FEISHU_WEBHOOK_URL=https://open.feishu.cn/open-apis/bot/v2/hook/xxx

# 飞书用户 ID
FEISHU_SHUSHU_OPEN_ID=ou_xxx    # 舒舒
FEISHU_SANGE_OPEN_ID=ou_xxx     # 三哥
FEISHU_BOT_OPEN_ID=ou_xxx       # 机器人 open_id，Actions 兜底判断 @ 需要
FEISHU_CHAT_ID=oc_xxx           # 群聊 ID
FEISHU_STATUS_CHAT_ID=oc_xxx    # 可选，三哥和机器人的单聊 ID，用于本地服务状态推送
FEISHU_READ_MESSAGES=true

# GitHub
GITHUB_USERNAME=yuaiccc
GITHUB_TOKEN=github_pat_xxx      # Fine-grained PAT
GITHUB_PRIVATE_REPOS=yuaiccc/project-history

# 运行
DRY_RUN=false
POLL_INTERVAL_SECONDS=300        # 5 分钟
MEMORY_ENABLED=true
MEMORY_DIR=memory_data
```

### GitHub Actions Secrets

配置页：https://github.com/yuaiccc/project-history/settings/secrets/actions

**重要**：Secrets 必须加到 **Environment `feishu`** 下，不是 Repository secrets！

| Secret 名 | 说明 |
|---|---|
| `FEISHU_APP_ID` | 飞书应用 ID |
| `FEISHU_APP_SECRET` | 飞书应用密钥 |
| `FEISHU_CHAT_ID` | 群聊 ID |
| `FEISHU_SHUSHU_OPEN_ID` | 舒舒的 open_id |
| `FEISHU_SANGE_OPEN_ID` | 三哥的 open_id |
| `FEISHU_BOT_OPEN_ID` | 机器人 open_id，用于 Actions 历史消息 API 精确判断 @ |
| `FEISHU_BOT_NAME` | 可选兜底，无法拿到 bot open_id 时使用，不如 open_id 稳定 |
| `GH_USERNAME` | GitHub 用户名（注意：不能以 GITHUB_ 开头） |
| `GH_TOKEN` | GitHub PAT（注意：不能以 GITHUB_ 开头） |
| `GH_PRIVATE_REPOS` | 私有仓库列表 |
| `DEEPSEEK_API_KEY` | DeepSeek API Key |

## 依赖安装

```bash
pip install -r requirements.txt
```

核心依赖：`requests`, `python-dotenv`, `lark-oapi`

## 运行

### 本地运行

```bash
cd /Users/xujunshan/Code/github_activity_generator
python3 main.py
```

### GitHub Actions 手动触发

```bash
gh workflow run bot.yml --repo yuaiccc/project-history
```

或到 https://github.com/yuaiccc/project-history/actions 点 "Run workflow"

## 踩坑记录

### 1. `nonlocal _processed_ids` 必须声明
`feishu_api.py` 的 `_handle_message` 内部重新赋值 `_processed_ids`（去重集合裁剪时），必须用 `nonlocal` 声明，否则 Python 把它当局部变量，导致 `referenced before assignment` 报错，所有消息处理直接跳过。

### 2. GitHub Actions Secret 名不能以 `GITHUB_` 开头
`GITHUB_USERNAME`、`GITHUB_TOKEN` 等名字是 GitHub 保留的，添加 Secret 会报错。改用 `GH_USERNAME`、`GH_TOKEN`。

### 3. Secrets 要加到 Environment 不是 Repository
workflow 里 `environment: feishu`，所以 Secrets 必须加到 Environment `feishu` 下，否则注入不进来（值为空）。

### 4. GitHub Token 需要权限
使用 Fine-grained PAT，需要 `repo` 读取权限。Token 过期会报 401，公开事件 API 拉不到数据。

### 5. 飞书应用权限
需要以下权限：`im:message`（发消息）、`im:message:send_as_bot`（机器人发消息）、`im:resource`（读消息）、`im:message.reactions:write`（表情回复）。缺少权限会导致对应功能静默失败。

### 6. AppleScript 权限
`local_apps.py` 用 AppleScript 读取前台应用，首次运行 macOS 会弹窗要求授权"自动化"权限。需要在 系统设置 → 隐私与安全性 → 自动化 里允许 Python/Terminal 控制 System Events。

### 7. 飞书卡片 table 组件
表格 `columns` 的 `data_type` 必须是 `text`，`display_name` 是列标题。`rows` 里每行的 key 必须和 `columns` 的 `name` 对应。

### 8. 去重集合不能 `clear()`
之前去重集合超过 200 条时 `clear()` 清空，导致旧消息 ID 丢失，飞书重发时重复处理。改为保留最近 500 条，超了裁剪到 300 条。

### 9. 飞书事件去重必须用 event_id + message_id 双重去重
飞书"至少发送一次"策略会在 3 秒内没收到 HTTP 200 时重发。必须用 `event_id`（事件唯一标识）+ `message_id`（消息唯一标识）做双重去重。`feishu_api.py` 的 `_handle_message` 里用 `nonlocal _processed_ids` 声明，否则重新赋值时报 `local variable referenced before assignment`。

### 10. 群聊 @机器人判断
群聊消息只有 @机器人才回复，私聊全部回复。检查 `mentions` 里是否匹配当前机器人。

注意：不同接口形态不完全一样。长连接事件可能给 `mentioned_type == "bot"` 或 `app`，也可能需要从 `mentions[].id.open_id` 判断；历史消息 REST API 的 `mentions[].id` 按官方文档和实测是被 @ 用户或机器人的 open_id 字符串。因此本地和 GitHub Actions 都必须配置 `FEISHU_BOT_OPEN_ID`，否则无法稳定区分“@机器人”和“@其他人”。

私聊 `p2p` 只依赖本地长连接实时事件；Actions 兜底只拉 `FEISHU_CHAT_ID` 群聊历史消息，不扫私聊。飞书官方接收消息事件文档要求：单聊消息需要 `im:message.p2p_msg` 或 `im:message.p2p_msg:readonly`，群聊 @ 机器人需要 group_at 相关权限。

长连接可能补发旧事件。`FEISHU_EVENT_MAX_AGE_SECONDS` 默认 600 秒，超过就跳过，避免回复已撤回/过期消息。飞书错误码 `230011`（消息已撤回）和 `231003`（消息不存在/不可见）要短路，不要继续调用 DeepSeek 或补表情。

维护飞书消息字段时以官方文档为准，不要凭猜测改字段结构：
https://open.feishu.cn/document/home/index

### 11. 状态查询和 GitHub 查询必须分开
`main.py` 的 `_classify_tool_intent()` 返回 `status`、`github`、`search` 或 `none`。问"在干嘛/最近活动/最近怎么样"默认只走本地窗口状态；明确问 GitHub、提交、commit、代码、仓库、PR、issue 才推 GitHub 卡片。不要再把普通状态问题自动带到 GitHub。

### 11b. 外部搜索只走本机 OpenClaw
`external_search.py` 通过 `openclaw infer web search --json` 调用本机 OpenClaw 的 `web.search` provider，再用 DeepSeek 做"短结论 + 表格 + 来源链接"卡片。`main.py` 的 `_classify_tool_intent()` 现在可能返回 `search`：问"搜索/查一下/最近B站哪些新番热门/新闻/热榜/新番"走外部搜索；问"三哥最近活动/在干嘛"仍然走本地窗口状态。

这个能力只在本地长连接模式可用，GitHub Actions 兜底不能访问三哥电脑上的 OpenClaw。飞书开放平台有 Open Search/站内资源搜索类能力，但不要把它当成机器人公网搜索接口；维护飞书接口仍以官方文档为准：https://open.feishu.cn/document/home/index

### 12. GitHub commit 卡片不生成 DeepSeek 总结
GitHub commit 活动卡片不生成 DeepSeek 总结，只展示表格和统计，避免自动开场太腻。

### 13. 同一项目 1 小时内提交合并
`notifier.py` 和 `actions_runner.py` 的表格构建逻辑只合并同一仓库且组内首尾时间跨度不超过 1 小时的 PushEvent。超过 1 小时必须新开一行，不能把同仓库所有提交无脑合并，也不能按相邻时间链式合并到超过 1 小时。

### 14. 活动卡片里的提交说明必须中文化
舒舒看卡片时不应该直接看到英文 commit message。`notifier.py` 和 `actions_runner.py` 都必须通过 `commit_text.brief_commit_messages()` 生成表格里的提交说明；优先 DeepSeek 改写成普通中文短句，失败时用本地规则兜底。

### 15. 飞书输出统一清洗
所有发往飞书的文本和卡片必须经过 `text_safety.py` 清洗。SDK 路径在 `feishu_api.py` 的发送函数里统一处理，Actions 路径在 `actions_runner.py` 的发送函数里统一处理。新增发送入口时要复用这层清洗，不要只靠 prompt 约束。

### 16. 回归测试
`tests/test_regressions.py` 覆盖三类容易回潮的问题：禁用称呼清洗、GitHub 卡片不出现顶部总结、状态/GitHub 意图区分。改 prompt、卡片或发送层后先跑：
`.venv/bin/python -m unittest tests.test_regressions`

### 17. 本地服务状态推送
`FEISHU_STATUS_CHAT_ID` 配成三哥和机器人的单聊 chat_id 后，`main.py` 会在本地服务启动/重启、GitHub 轮询异常、消息处理异常、飞书长连接退出时向单聊推送状态。进程崩溃后由 launchd 重启，新进程会发“已启动/重启”。电脑关机或系统睡死时本地进程无法主动推送，只能靠 GitHub Actions 兜底。

### 18. 通话纪要 / 妙记是重要信息源
秋酿和舒舒的通话纪要是比 GitHub 更重要的关系上下文。当前项目通过 `call_notes.py` 按已知 `minute_token` 读取飞书妙记：
- 获取妙记信息：`GET /open-apis/minutes/v1/minutes/:minute_token`
- 导出妙记文字记录：`GET /open-apis/minutes/v1/minutes/:minute_token/transcript`
- 官方文档入口：https://open.feishu.cn/llms-docs/zh-CN/llms-minutes.txt

默认关闭：`CALL_NOTES_ENABLED=false`。开启时配置 `FEISHU_MINUTE_TOKENS`。应用需要至少具备妙记基础信息和文字记录导出相关权限，并且对对应妙记有阅读/导出权限。会议维度的自动发现应从视频会议接口进入，例如 `GET /open-apis/vc/v1/meetings/:meeting_id` 可返回会议纪要/逐字稿相关产物；不要凭猜测写其他接口。

`call_notes.py` 不应该把 transcript 原文整段塞给回复模型。当前流程是读取 transcript 后先整理为短摘要并写入 `call_notes_cache.json`（gitignore），摘要只保留关系上下文：舒舒最近在意/担心/开心的事、秋酿答应过或应该记得的事、相处偏好和雷点。DeepSeek 摘要失败时使用关键词 fallback 摘取相关句子。

### 19. 记忆系统要轻量但可控
`memory.py` 继续使用 `memory_data/memories.json`，不要引入重型向量库作为默认依赖。新增记忆要包含 `category`、`importance`、`last_seen`、`seen_count`，同义或包含关系的重复事实应该合并，避免一问一答把同一件事刷成几十条。`MEMORY_MAX_ITEMS` 默认 200，裁剪时优先保留重要度高、重复出现、最近出现的记忆。

### 15. AppleScript 本地窗口状态也是重要信息源
`local_apps.py` 使用 AppleScript 读取秋酿电脑当前前台应用和窗口标题，这是判断“秋酿在干嘛”的重要来源。只有本地模式可用；GitHub Actions 兜底模式看不到电脑窗口状态。

## 情侣关系背景

- 三哥 = 秋酿 = 许君山，舒舒 = 烨子 = 舒烨（小名"火花十"）
- 2026年6月4日在一起
- 对机器人来说，舒舒按"大哥的老婆"理解；群里仍然只叫"舒舒"或"烨子"，不要直接喊"嫂子"
- 秋酿生日：2004年10月15日
- 舒舒生日：2004年11月5日
- 秋酿是学生，不是上班族，不要出现"同事""下班"等词
- 默认底色是秋酿想舒舒、在意舒舒、想让舒舒安心；不要每次都机械重复，但也不要把代码项目放在舒舒前面

## 飞书多维表格

地址：https://my.feishu.cn/base/U1s0bKGceaTx9As1xWKcc5JDnPp

用于持久化记录 GitHub 活动，`bitable_api.py` 负责写入。

## 修改代码后

1. 本地测试：`.venv/bin/python main.py` 看日志
2. 语法检查：`python3 -c "import <模块名>"`
3. Push 到 GitHub：`git add -A && git commit -m "xxx" && git push`
4. 重启本地机器人：`launchctl kickstart -k gui/501/com.xujunshan.github-activity-bot`
5. 如果改了 Actions 相关，手动触发一次：`gh workflow run bot.yml --repo yuaiccc/project-history`
