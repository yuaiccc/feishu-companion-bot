# feishu-companion-bot

> 一个飞书 / Lark **Agentic 陪伴机器人** —— owner 的私人小弟。  
> 长连接实时响应 + GitHub Actions 离线兜底，带 **OceanBase 向量混合记忆**、**跨渠道会话一体化**、**自主记忆沉淀**、LLM 意图分类和隐私脱敏。Go 实现。

机器人定位是 **owner 的助手 / 小弟**：帮忙解释、提醒、搜索、维护记忆，但 **不冒充 owner 本人**。默认配置为通用版本，不含任何真实人名。

---

## ✨ 亮点

### 🔗 高可用双通道
- **本机 WebSocket 长连接**：通过飞书 lark SDK 实时收发事件。
- **GitHub Actions 兜底**：本机离线时 cron 每 5 分钟轮询一次，确保消息不丢。
- 事件按 ID 幂等去重，跨 run 不重复推送；飞书重投的消息按 `message_id` 去重，不重复回复。

### 🧠 Agentic 三层记忆 + 自主沉淀
| 层级 | 来源 | 说明 |
|------|------|------|
| **短期工作记忆** | 飞书近期消息 | 最近 100 条对话上下文窗口，含 bot 自身回复和撤回占位 |
| **长期结构化记忆** | LLM 自主判定 + 自动整理 | DeepSeek 结合对话上下文判断是否值得记住 → 自动冲突消解/冗余去重 → 直接写入 OceanBase |
| **只读归档记忆** | 微信聊天 & 图片 OCR | 微信聊天记录 chunks + 图片 caption/OCR/embedding，作为跨渠道长期背景知识 |

- **全自主沉淀**：无需用户点击确认，机器人独立判断、整理、去重后直接入库。
- **记忆冲突消解**：新记忆写入前，LLM 自动对比已有记忆，执行 `ignore`（冗余）/ `delete`（冲突）/ `update`（融合）/ `none`（新增）四种操作。
- **上下文感知提取**：对 "对的"、"好的" 等简答消息，结合近 5 轮对话历史还原出完整事实再提炼记忆。
- **三级记忆分类**：`semantic`（稳定事实/偏好）、`relational`（相处方式/边界）、`episodic`（重要事件）。

### 🔍 OceanBase 向量混合检索 (Hybrid RAG)
- **向量语义检索**：基于 Ollama `nomic-embed-text` 生成 1024 维嵌入向量，通过 OceanBase `cosine_distance()` 召回语义相似记忆。
- **Ngram 全文检索**：基于 OceanBase ngram 分词全文索引进行关键词匹配召回。
- **Reciprocal Rank Fusion (RRF)**：双路并发召回后，在内存中以 RRF 框架（语义权重 0.65 + 关键词权重 0.35）混合重排，精度远超单路匹配。
- Schema 自动升级：首次启动自动执行 `ALTER TABLE bot_memories ADD COLUMN embedding vector(1024)`。

### 🤖 LLM 意图分类 (Agentic Intent Router)
抛弃了传统正则匹配，由 DeepSeek 大模型实时判定用户意图，支持 7 种意图路由：

| 意图 | 说明 | 示例 |
|------|------|------|
| `github` | GitHub 活动查询 | "查下最近的 commit" |
| `health` | 机器人自检 | "帮我自检一下服务状态" |
| `memory_audit` | 记忆审计 | "查看记忆面板" |
| `search` | 联网搜索 | "上网搜一下 2026 年流行的 AI 框架" |
| `recall` | 撤回消息 | "撤回刚才发错的" |
| `media` | 图片回忆 | "找张美食照片"、"换一张" |
| `none` | 普通聊天 | "你好啊小弟" |

### 💬 跨渠道会话一体化
- OceanBase 中存储的微信聊天归档、图片归档与当前飞书对话被视为 **同一条连贯时间线**。
- 机器人在回复时会自然地融入历史微信对话作为默契背景事实，绝不会说出 "根据我的记忆" 等机器化套话。
- 对用户来说，感受就是一个 **一直在身边、记忆清晰的贴心小弟**。

### 🖼️ 图片回忆
- 自然语言找图（"找张美食照片"），LLM 结合对话上下文理解查询意图并选最相关的一张发出来。
- "看全部" 发多张；坏图按文件头校验不发。
- 支持图片理解：收到图片消息时自动调用视觉模型生成描述，融入对话上下文。

### 🔒 隐私优先
- 手机号 / 邮箱 / 群 ID / 用户 ID / API key 进 LLM 前统一脱敏。
- 记忆可见性分 `private` / `owner_only` / `public_to_target` 三级。

### ⚡ 流式回复
- CardKit 打字机效果，逐步推送文字。
- 权限没开通时自动降级为「发文本消息 + 编辑」。

### 🌐 联网搜索合成
- 集成 DeerFlow / OpenClaw 搜索后端，搜索结果经 LLM 深度合成。
- 输出带 `[1]` `[2]` 参考文献锚点的有据可查解答。

---

## 📐 架构

```text
                 ┌─ 本机 WebSocket 长连接（lark SDK，实时）─┐
飞书事件 ───────┤                                          ├─→ LLM 意图分类
                 └─ GitHub Actions 兜底（5min cron）──────┘
                                                              │
                         ┌────────────────────────────────────┘
                         ▼
              ┌─────────────────────┐
              │   Agentic Router    │
              │  github | health    │
              │  search | media     │
              │  recall | memory    │
              │  audit  | chat      │
              └────────┬────────────┘
                       │
          ┌────────────┼────────────┐
          ▼            ▼            ▼
   OceanBase      DeepSeek       Ollama
   向量+全文       LLM 推理      嵌入+视觉
   混合检索        流式回复       本地模型
          │            │            │
          └────────────┼────────────┘
                       ▼
              CardKit 流式卡片回复
```

### 记忆处理流水线

```text
用户消息
   │
   ▼
shouldRememberViaLLM()          ← 结合近 5 轮对话上下文判断
   │ remember=true
   ▼
consolidateMemory()             ← 对比已有记忆：none / ignore / delete / update
   │ keep=true
   ▼
mem.Add() + 向量 embedding      ← 自动写入 OceanBase，无需人工确认
   │
   ▼
AddReaction("SUBMIT") ✅        ← 给原消息加上 ✅ 表情反馈
```

---

## 🛠️ 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.22+ |
| 飞书 SDK | 官方 lark SDK (WebSocket + REST) |
| 数据库 | OceanBase (向量索引 + ngram 全文索引) |
| LLM | DeepSeek (推理 + 流式) |
| 本地模型 | Ollama (nomic-embed-text 嵌入 + qwen2.5vl 视觉) |
| CI/CD | GitHub Actions (离线兜底 cron) |
| 索引器 | Python (Ollama 视觉模型图片 OCR/caption/embedding) |

---

## 📁 项目结构

```text
cmd/
  bot/           主入口（--actions 切换 GitHub Actions 单次兜底模式）
  smoke/         真实飞书链路冒烟验证
  memtool/       记忆维护 CLI
internal/
  feishu/        飞书 API、长连接、CardKit 流式卡片、撤回、图片上传
  memory/        三层记忆存储、OceanBase 向量混合检索、RRF 重排、Embedder
  llm/           DeepSeek client、SSE 流式、temperature/max_tokens 控制
  context/       对话上下文预算管理（token 精细分配）
  safety/        隐私脱敏（手机/邮箱/ID/密钥）
  profile/       人设模板、成员解析、角色识别
  search/        联网搜索后端（DeerFlow / OpenClaw）
  github/        GitHub 事件拉取 + 幂等去重
  health/        机器人自检（OceanBase/飞书/DeepSeek/Ollama 连通性）
  latency/       请求链路耗时追踪
  state/         持久化状态管理
  notes/         情书/情感笔记模块
  localapps/     本地应用集成
profiles/        人设模板 JSON（真实 profile 不提交）
docs/            进阶文档
```

---

## 🚀 快速开始

### 1. 编译

```bash
go build -o bot ./cmd/bot
```

### 2. 配置

```bash
cp .env.example .env
# 编辑 .env，填入飞书 / DeepSeek / GitHub / OceanBase 凭据
```

核心配置项：

| 变量 | 说明 |
|------|------|
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | 飞书应用凭据 |
| `FEISHU_BOT_OPEN_ID` | 机器人的 open_id |
| `FEISHU_OWNER_OPEN_ID` | owner 的 open_id |
| `DEEPSEEK_API_KEY` | DeepSeek API Key |
| `MEMORY_DATABASE_DSN` | OceanBase/MySQL DSN，如 `jdbc:mysql://127.0.0.1:2881/shuye_chat` |
| `OLLAMA_BASE_URL` / `OLLAMA_MODEL` | Ollama 嵌入模型地址 |
| `PROFILE_ID` | 使用的人设模板 ID（对应 `profiles/<id>.json`） |
| `DRY_RUN` | `true` 只打印不发消息；`false` 正式运行 |

### 3. 启动

```bash
# 前台运行
./bot

# 后台运行（推荐）
tmux new-session -d -s bot -c $(pwd) ./bot

# GitHub Actions 单次兜底模式
./bot --actions
```

### 4. 人设配置

在 `profiles/` 下创建 JSON 文件，示例：

```json
{
  "id": "my-profile",
  "name": "我的陪伴机器人",
  "bot_role": "我的小弟",
  "bot_name": "小弟",
  "owner_name": "老板",
  "target_name": "对象",
  "members": [
    {
      "open_id": "ou_xxx",
      "name": "老板",
      "role": "owner",
      "aliases": ["大哥"]
    }
  ],
  "config": {
    "persona": "自定义人设描述..."
  }
}
```

---

## 🧪 测试

```bash
# 运行全部测试（需配置 .env 中的 DeepSeek API Key 和 OceanBase）
go test -v ./cmd/bot

# 单独运行意图分类测试
go test -v ./cmd/bot -run TestLLMClassifyIntent

# 单独运行记忆整合测试
go test -v ./cmd/bot -run TestMemoryConsolidation

# 单独运行上下文感知记忆提取测试
go test -v ./cmd/bot -run TestContextAwareMemoryExtraction

# 冒烟测试：实际查询 OceanBase + DeepSeek 合成
go test -v ./cmd/bot -run TestSmokeQueryShushuFood
```

---

## 📊 OceanBase 表结构

机器人使用以下表存储和检索记忆：

### `bot_memories` — 长期结构化记忆

| 列 | 类型 | 说明 |
|----|------|------|
| `id` | VARCHAR(64) PK | 记忆唯一 ID |
| `profile_id` | VARCHAR(64) | 关联的 profile |
| `content` | TEXT | 记忆内容（自然语言） |
| `memory_type` | VARCHAR(32) | `semantic` / `relational` / `episodic` |
| `visibility` | VARCHAR(32) | `private` / `owner_only` / `public_to_target` |
| `source_type` | VARCHAR(32) | 来源标识 |
| `embedding` | VECTOR(1024) | nomic-embed-text 1024 维向量 |
| `created_at` | BIGINT | Unix 时间戳 |

### `chat_message_chunks` — 微信聊天归档（只读）

### `media_assets` — 图片/OCR 归档（只读）

---

## 📄 许可

MIT
