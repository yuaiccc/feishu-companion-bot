# feishu-companion-bot

一个通用、公开安全、隐私优先的飞书 / Lark 陪伴机器人框架，Go 实现。

机器人定位是 **owner 的小弟 / 助手**：在 owner 不在时帮忙解释、提醒、整理、搜索、维护记忆，但**不伪装成 owner 本人**。默认配置是通用版本（owner 称"老板"），不带任何真实人名。

## 功能

- **CardKit 官方流式回复** — 私聊走飞书 CardKit 流式卡片接口，打字机效果；权限没开通时自动降级为「发文本 + 编辑」
- **飞书长连接即时响应** — 本机 WebSocket 长连接，群聊 @ 机器人回复，私聊直接回复
- **GitHub Actions 兜底** — 本机离线时每 5 分钟轮询，`state` 走 `actions/cache` 持久化，幂等去重不重复推送
- **DeepSeek 总结** — GitHub 动态、主动话题等走轻量总结，一句话说重点
- **profile 隔离记忆** — `memory_data/<profile>/`，可见性分 `private` / `owner_only` / `public_to_target`
- **隐私脱敏** — 用户消息进 LLM 前过滤手机号 / 邮箱 / 群 ID / 用户 ID / API key
- **外部搜索** — DeerFlow / OpenClaw 后端可选
- **卡片交互** — 候选记忆确认、健康状态等用交互卡片

## 前置条件

- **Go 1.26+**（`go.mod` 声明 `go 1.26.2`）
- **飞书企业自建应用**，开通机器人能力
- **DeepSeek API key**
- 可选：**Ollama**（本地 embedding，不开通则用 hash fallback）

## 飞书应用配置

在飞书开放平台「权限管理」开通：

```
im:message
im:message:send_as_bot
im:message:readonly
im:message.reactions:write
im:resource
cardkit:card:write        # CardKit 流式回复需要，不开通则自动降级
```

事件订阅：

- `im.message.receive_v1` — 接收消息
- `card.action.trigger` — 卡片按钮回调

详细步骤见 [`docs/飞书配置.md`](docs/飞书配置.md)。

## 快速开始

```bash
# 编译
go build -o bot ./cmd/bot

# 配置
cp .env.example .env
# 编辑 .env 填入飞书 / DeepSeek / GitHub 配置

# DRY RUN（只打印，不真正发消息）
./bot

# 正式运行（本地长连接）
DRY_RUN=false ./bot
```

## 运行模式

### 本地长连接（默认）

```bash
./bot
```

WebSocket 长连接即时响应。群聊只有 @ 机器人时回复，私聊直接回复。这是日常使用的主模式。

### GitHub Actions 兜底

```bash
./bot --actions
```

单次 GitHub 轮询后退出，用于本机离线时兜底。`.github/workflows/bot.yml` 已配好每 5 分钟 `schedule`，`memory_data` 通过 `actions/cache` 跨 run 持久化，事件按 ID + commit sha 幂等去重。需在仓库 Settings → Secrets 配好 `FEISHU_*` / `DEEPSEEK_*` / `GH_*`。

## smoke 测试

真实飞书 API 验证，**发真实消息**，本地手动跑、不进 CI：

```bash
go run ./cmd/smoke               # 全部三项
go run ./cmd/smoke -mode stream  # CardKit 流式全链路
go run ./cmd/smoke -mode group   # 普通文本发送
go run ./cmd/smoke -mode github  # GitHub 一句话推送
```

读 `.env`，开始前 3 秒倒计时。cardkit 权限是否真开通，跑一次 `stream` 就知道。

## memtool — 记忆维护

```bash
go run ./cmd/memtool -list                # 列出所有记忆
go run ./cmd/memtool -search "关键词"      # 搜索
go run ./cmd/memtool -show-vis            # 显示可见性标签
go run ./cmd/memtool -clean               # 清洗重复/过时记忆
go run ./cmd/memtool -clean -dry-run=false # 真正执行删除
go run ./cmd/memtool -migrate-json        # 将 JSON 记忆迁移到数据库后端
```

## 项目结构

```text
cmd/
  bot/        本地长连接入口（--actions 单次兜底）
  memtool/    记忆维护 CLI
  smoke/      真实飞书链路验证
internal/
  config/     环境变量加载
  feishu/     飞书 API、WebSocket、CardKit 流式、消息卡片
  llm/        DeepSeek client、SSE 流式
  memory/     结构化记忆、embedding、可见性过滤
  context/    上下文预算管理
  github/     GitHub events、幂等去重
  safety/     隐私脱敏（SanitizeForLLM）
  state/      事件去重 state（HasSent/MarkSent/FilterNew）
  profile/    人设模板加载
  search/     外部搜索后端
  health/     服务健康检查
  notes/      云文档评论（通用工具包）
  localapps/  本机窗口/空闲状态读取（macOS）
profiles/     profile 模板（default / example-couple）
docs/         部署文档
```

## Profile

```bash
cp profiles/example-couple.json profiles/my-profile.json
# 编辑 my-profile.json（owner_name / target_name / bot_role 等）

PROFILE_ID=my-profile PROFILES_DIR=. ./bot
```

`profiles/default.json` 是通用占位（owner_name 为"老板"），可直接用。真实 profile 不会被提交（`.gitignore` 用白名单：忽略 `profiles/*`，只保留 `default.json` / `example-couple.json`）。

## 记忆系统

- 默认存储路径：`memory_data/<PROFILE_ID>/memories.json`
- 可选数据库后端：配置 `MEMORY_DATABASE_DSN` 后使用 OceanBase/MySQL 表 `bot_memories`
- 可选聊天归档源：`MEMORY_INCLUDE_CHAT_ARCHIVE=true` 时，将同库聊天归档表（默认 `chat_message_chunks`，可用 `MEMORY_CHAT_ARCHIVE_TABLE` / `MEMORY_CHAT_ARCHIVE_TEXT_COLUMN` / `MEMORY_CHAT_ARCHIVE_TIME_COLUMN` 配置）作为只读长期聊天记忆参与检索
- 可选图片归档源：`MEMORY_INCLUDE_MEDIA_ARCHIVE=true` 时，将同库图片 OCR/caption 表（默认 `media_assets`，可配置表名和列名）作为只读图片记忆参与检索；用户明确问图片、截图、照片、回忆时，机器人会先总结，再按 `MEMORY_MEDIA_SEND_IMAGE` 决定是否发回最相关的一张图
- 可见性：`private`（绝不进 prompt）/ `owner_only`（只给 owner 回复用）/ `public_to_target`（可对目标用户用）
- 候选记忆由 DeepSeek 判断，发卡片到 owner 私聊确认后落库
- 群聊消息不靠关键词直接写记忆

OceanBase 示例：

```env
MEMORY_DATABASE_DSN=jdbc:mysql://127.0.0.1:2881/companion_memory
MEMORY_INCLUDE_CHAT_ARCHIVE=true
MEMORY_CHAT_ARCHIVE_VISIBILITY=owner_only
# 聊天归档表/列名（接自己的聊天库时按 schema 改）
MEMORY_CHAT_ARCHIVE_TABLE=chat_message_chunks
MEMORY_CHAT_ARCHIVE_TEXT_COLUMN=chunk_text
MEMORY_CHAT_ARCHIVE_TIME_COLUMN=end_time
MEMORY_INCLUDE_MEDIA_ARCHIVE=true
MEMORY_MEDIA_ARCHIVE_VISIBILITY=owner_only
MEMORY_MEDIA_ARCHIVE_TABLE=media_assets
MEMORY_MEDIA_OCR_COLUMN=ocr_text
MEMORY_MEDIA_CAPTION_COLUMN=caption
MEMORY_MEDIA_TIME_COLUMN=sent_at
MEMORY_MEDIA_SENDER_COLUMN=sender
MEMORY_MEDIA_FILE_PATH_COLUMN=file_path
MEMORY_MEDIA_MSGID_COLUMN=msgid
MEMORY_MEDIA_SEND_IMAGE=true
```

`MEMORY_CHAT_ARCHIVE_VISIBILITY` 默认 `owner_only`，避免把私密聊天归档直接用于对外回复；确认双方都可见后再改为 `public_to_target`。
`MEMORY_MEDIA_ARCHIVE_VISIBILITY` 同样默认 `owner_only`。如果要让目标用户也能查到图片回忆，先确认图片内容适合共享，再改成 `public_to_target`。发图能力依赖飞书图片上传接口，需要应用具备图片上传和消息发送相关权限。

## 隐私脱敏

用户消息、近期对话、相关记忆进 DeepSeek prompt 前，统一过 `safety.SanitizeForLLM`：

- 手机号（11 位、带分隔符）
- 邮箱
- `oc.xxx`（群 ID）、`ou_xxx`（用户 ID）
- `sk-xxx`（API key）

匹配到的替换为 `[敏感信息]`，不会原样发到 DeepSeek。

## 环境变量

完整列表见 [`.env.example`](.env.example)，关键项：

```env
# 飞书
FEISHU_APP_ID=cli_xxx
FEISHU_APP_SECRET=xxx
FEISHU_CHAT_ID=oc_xxx              # 目标会话（群或私聊 chat_id）
FEISHU_BOT_OPEN_ID=ou_xxx
FEISHU_OWNER_OPEN_ID=ou_xxx        # owner 的 open_id，用于身份判断
FEISHU_TARGET_OPEN_ID=ou_xxx       # 可选，伴侣/目标用户 open_id；多人群聊推荐写到 profile.members

# DeepSeek
DEEPSEEK_API_KEY=sk-xxx
DEEPSEEK_BASE_URL=https://api.deepseek.com
DEEPSEEK_MODEL=deepseek-chat

# Ollama（可选，embedding）
OLLAMA_BASE_URL=http://localhost:11434
OLLAMA_MODEL=nomic-embed-text

# 记忆数据库（可选，OceanBase/MySQL）
MEMORY_DATABASE_DSN=
MEMORY_INCLUDE_CHAT_ARCHIVE=false
MEMORY_CHAT_ARCHIVE_VISIBILITY=owner_only
MEMORY_CHAT_ARCHIVE_TABLE=chat_message_chunks
MEMORY_CHAT_ARCHIVE_TEXT_COLUMN=chunk_text
MEMORY_CHAT_ARCHIVE_TIME_COLUMN=end_time
MEMORY_INCLUDE_MEDIA_ARCHIVE=false
MEMORY_MEDIA_ARCHIVE_VISIBILITY=owner_only
MEMORY_MEDIA_ARCHIVE_TABLE=media_assets
MEMORY_MEDIA_OCR_COLUMN=ocr_text
MEMORY_MEDIA_CAPTION_COLUMN=caption
MEMORY_MEDIA_TIME_COLUMN=sent_at
MEMORY_MEDIA_SENDER_COLUMN=sender
MEMORY_MEDIA_FILE_PATH_COLUMN=file_path
MEMORY_MEDIA_MSGID_COLUMN=msgid
MEMORY_MEDIA_SEND_IMAGE=true

# GitHub
GH_USERNAME=your_username
GH_TOKEN=ghp_xxx
GH_PRIVATE_REPOS=owner/repo1,owner/repo2

# 行为
DRY_RUN=true                       # true 只打印不发送
STREAMING_REPLY_ENABLED=true       # 私聊走 CardKit 流式（需 cardkit:card:write）
STREAMING_REPLY_UPDATE_INTERVAL_SECONDS=0.35
MEMORY_ENABLED=true
MEMORY_CONFIRMATION_ENABLED=true

# Profile
PROFILE_ID=default
PROFILES_DIR=.

# 轮询
POLL_INTERVAL_SECONDS=300
```

`GH_USERNAME` / `GH_TOKEN` / `GH_PRIVATE_REPOS` 也兼容 `GITHUB_USERNAME` / `GITHUB_TOKEN` / `GITHUB_PRIVATE_REPOS` 旧名（fallback）。

## 多人群聊身份

群里有多人时，不要把“非 owner”都当成 target。推荐在 profile 里配置固定成员：

```json
{
  "members": [
    {
      "open_id": "ou_owner_xxx",
      "name": "三哥",
      "role": "owner",
      "relation": "机器人主人",
      "aliases": ["秋酿"]
    },
    {
      "open_id": "ou_target_xxx",
      "name": "舒舒",
      "role": "target",
      "relation": "owner 的伴侣",
      "aliases": ["烨子"]
    },
    {
      "open_id": "ou_friend_xxx",
      "name": "朋友A",
      "role": "friend",
      "relation": "普通群友",
      "aliases": []
    }
  ]
}
```

身份规则：

- `owner` 可以读取 `owner_only` 和 `public_to_target` 记忆。
- `target` 只能读取 `public_to_target` 记忆。
- 未配置或普通 `friend` 默认是 `other`，不会读取私密聊天/图片记忆，也不会进入长期记忆候选。
- 固定小群可以只用 profile；大群或运行时自动识别成员，建议把成员表放到 OceanBase/MySQL，例如 `bot_participants(profile_id, chat_id, open_id, display_name, role, relation, aliases, confirmed_at, last_seen_at)`。

## 开源说明

- 机器人是 owner 的助手，不是 owner 本人的分身
- 默认 profile 是通用占位，不含真实人名 / 地址 / token
- `.env` / `state.json` / `memory_data/` / 真实 profile / 日志均不提交（见 `.gitignore`）
- 私有恋爱笔记逻辑已剥离，`internal/notes` 只保留通用云文档评论工具包

## 许可

MIT
