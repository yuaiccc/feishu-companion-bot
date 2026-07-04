# feishu-companion-bot

一个飞书 / Lark 陪伴机器人——owner 的私人小弟，长连接实时响应 + GitHub Actions 离线兜底，带结构化记忆、聊天 / 图片归档检索和隐私脱敏。Go 实现。

机器人定位是 **owner 的助手**：帮忙解释、提醒、搜索、维护记忆，但**不冒充 owner 本人**。默认配置是通用版本，不含任何真实人名。

## 亮点

- **高可用双通道**：本机 WebSocket 长连接实时收发；本机离线时 GitHub Actions 每 5 分钟轮询兜底。事件按 ID 幂等去重，跨 run 不重复推送；飞书重投的消息按 `message_id` 去重，不重复回复。
- **三层记忆**：结构化长期记忆（DeepSeek 判断候选 → 卡片确认落库）+ 只读聊天归档 + 图片归档（OCR / caption / embedding）。OceanBase 向量 + ngram 全文混合检索，可见性分 `private` / `owner_only` / `public_to_target`。
- **图片回忆**：自然语言找图（"找张美食照片"），LLM 结合对话上下文理解查询并选最相关的一张发出来；"看全部"发多张；坏图按文件头校验不发。
- **对话上下文工程**：100 条上下文窗口含 bot 自己的回复和撤回占位，发送者 `open_id` 解析成名字，当前消息从历史中隔离并标注为"请回复这条"，避免 bot 去回历史消息。
- **隐私优先**：手机号 / 邮箱 / 群 ID / 用户 ID / API key 进 LLM 前统一脱敏。
- **流式回复**：CardKit 打字机效果，权限没开通自动降级为「发文本 + 编辑」。
- **自我纠错**：bot 能撤回自己发错的消息。

## 架构

```text
                 ┌─ 本机长连接（lark SDK，实时）─┐
飞书事件 ───────┤                                ├─→ 意图 / LLM 回复决策 ─→ 记忆检索 ─→ 流式回复
                 └─ GitHub Actions 兜底（5min）─┘         │
                                                          ↓
                          离线索引器（Python + Ollama）→ OceanBase
                          bot_memories · chat_message_chunks · media_assets
```

- **在线**：长连接收事件 → 幂等去重 → 意图分类 / LLM 回复决策 → 记忆检索 → CardKit 流式回复。
- **兜底**：Actions cron 单次轮询，幂等 state 走 `actions/cache` 跨 run 持久化。
- **离线**：Python 脚本用 Ollama 视觉模型给图片做 OCR / caption / embedding，写回 OceanBase。

## 技术栈

**Go** · 飞书官方 lark SDK · **OceanBase**（向量 + ngram 全文）· **DeepSeek**（LLM）· **Ollama**（本地视觉 + embedding）· GitHub Actions · 索引器 Python

## 项目结构

```text
cmd/
  bot/       长连接入口（--actions 单次兜底）
  smoke/     真实飞书链路验证
  memtool/   记忆维护 CLI
internal/
  feishu/    飞书 API、长连接、CardKit 流式、撤回
  memory/    三层记忆 + embedding + 可见性过滤
  llm/       DeepSeek client、SSE 流式
  context/   上下文预算管理
  safety/    隐私脱敏
  profile/   人设与成员解析
  github/    事件拉取 + 幂等去重
profiles/    人设模板（真实 profile 不提交）
```

## 快速开始

```bash
go build -o bot ./cmd/bot
cp .env.example .env   # 填飞书 / DeepSeek / GitHub 凭据
./bot                  # DRY_RUN 默认只打印；DRY_RUN=false 正式跑
```

进阶配置（记忆后端、聊天 / 图片归档、profile 成员等）见 [`.env.example`](.env.example) 与 `docs/`。

## 许可

MIT
