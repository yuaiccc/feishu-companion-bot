# feishu-companion-bot

飞书陪伴机器人 — Go 重写版。

一个通用、公开安全、隐私优先的飞书陪伴机器人框架。支持：

- 飞书长连接即时响应
- GitHub Actions 兜底轮询
- DeepSeek 总结
- 本地 profile 隔离记忆
- 外部搜索（DeerFlow / OpenClaw）

## 快速开始

```bash
cd go
go build -o bot ./cmd/bot

# 配置环境变量
cp .env.example .env
# 编辑 .env 填入飞书和 DeepSeek 配置

# DRY RUN 模式（不真正发消息）
./bot

# 正式运行
DRY_RUN=false ./bot
```

## 项目结构

```text
go/
  cmd/
    bot/           本地长连接入口
                  也支持 --actions 单次兜底轮询
    memtool/       记忆维护 CLI
  internal/
    config/        环境变量加载
    feishu/        飞书 API、长连接、消息卡片
    llm/           DeepSeek client、流式
    memory/        结构化记忆、可见性过滤
    context/       上下文预算管理
    github/        GitHub events、幂等去重
    health/        服务健康检查
    latency/       延迟 span 日志
    profile/       人设模板加载
  profiles/        profile 模板
  docs/            部署文档
```

## Profile

```bash
cd go
cp profiles/example-couple.json profiles/my-profile.json
# 编辑 my-profile.json
export PROFILE_ID=my-profile
export PROFILES_DIR=.
# 运行
./bot
```

## 记忆系统

- `memory_data/<PROFILE_ID>/memories.json`
- 可见性：`private` / `owner_only` / `public_to_target`
- 候选记忆由 DeepSeek 判断，owner 私聊确认后落库

## 环境变量

```env
# 飞书
FEISHU_APP_ID=cli_xxx
FEISHU_APP_SECRET=xxx
FEISHU_CHAT_ID=oc_xxx
FEISHU_BOT_OPEN_ID=ou_xxx

# DeepSeek
DEEPSEEK_API_KEY=sk-xxx
DEEPSEEK_BASE_URL=https://api.deepseek.com
DEEPSEEK_MODEL=deepseek-chat

# GitHub
GH_USERNAME=your_username
GH_TOKEN=ghp_xxx
GH_PRIVATE_REPOS=owner/repo1,owner/repo2

# 行为
DRY_RUN=true
MEMORY_ENABLED=true
MEMORY_CONFIRMATION_ENABLED=true
```
