# 飞书陪伴机器人

一个使用 Go 编写、可自行部署的飞书陪伴机器人。机器人通过飞书长连接实时接收消息，由大模型决定是否回应、如何回应以及是否检索记忆；也可按需接入 OceanBase SeekDB、Ollama、图片归档、外部搜索、飞书云文档和 GitHub 动态。

项目提供的是通用底座。人物设定、成员关系、称呼、记忆边界和可选能力均通过配置完成，仓库不包含个人聊天记录、媒体文件、访问令牌或私有 profile。

## 主要能力

- 飞书群聊与私聊：长连接收取事件，区分群成员，无需固定关键词触发
- 上下文 Planner：由模型决定是否回应、检索问题、数据源、Top-K、近期消息数量、上下文预算及回复长度
- 流式反馈：使用飞书 CardKit 流式卡片，失败时自动降级为普通消息
- 分层记忆：短期对话状态、长期事实记忆、聊天归档和图片归档
- 混合召回：OceanBase 全文检索与向量检索通过加权 RRF 融合
- 隐私边界：按 profile 和可见性过滤记忆，发给模型前执行敏感信息脱敏
- 图片能力：macOS Apple Vision 本地 OCR、飞书 OCR 兜底、本地视觉模型和内容寻址媒体库
- 外部能力：可选 DeerFlow/OpenClaw 搜索、GitHub 活动摘要、飞书文档评论
- 可观测性：健康检查、分阶段延迟日志、记忆审计与关系图谱面板

## 工作流程

```text
飞书事件
  -> 幂等与时效检查
  -> 成员身份和最近对话状态
  -> DeepSeek 上下文 Planner
  -> 按需检索记忆 / 图片 / 外部信息
  -> 上下文裁剪与隐私脱敏
  -> DeepSeek 生成
  -> 飞书流式回复或文本降级
```

本地进程在线时，飞书事件通过长连接即时到达。GitHub Actions 仅用于机器离线时的轮询兜底，不能代替实时消息长连接。

## 环境要求

- Go 1.26.2（以 go.mod 为准）
- 一个已发布的飞书企业自建应用
- DeepSeek 或兼容 OpenAI Chat Completions 的模型服务
- 可选：OceanBase SeekDB/MySQL 兼容数据库
- 可选：Ollama 与 qwen3-embedding:0.6b

## 快速开始

```bash
git clone https://github.com/yuaiccc/feishu-companion-bot.git
cd feishu-companion-bot
cp .env.example .env
make test
make build
go run ./cmd/doctor -online
./bin/bot
```

默认启用 DRY_RUN=true，只打印而不发送消息。完成飞书配置并通过 smoke 测试后，再将其改为 false。

最低限度配置：

```env
FEISHU_APP_ID=cli_xxx
FEISHU_APP_SECRET=xxx
FEISHU_BOT_OPEN_ID=ou_xxx
FEISHU_CHAT_ID=oc_xxx
FEISHU_OWNER_OPEN_ID=ou_xxx

DEEPSEEK_API_KEY=sk_xxx
DEEPSEEK_BASE_URL=https://api.deepseek.com
DEEPSEEK_MODEL=deepseek-chat

PROFILE_ID=default
DRY_RUN=true
```

飞书权限、事件订阅和 CardKit 配置见 docs/飞书配置.md，本地常驻与 GitHub Actions 说明见 docs/开源部署.md。

## 人物与成员配置

复制 profiles/default.json 或 profiles/example-couple.json 创建自己的 profile。多人群聊应为每位成员配置 open_id、名称、角色和别名，避免模型混淆说话者。

私有 profile 默认被 .gitignore 排除；仓库只跟踪两个示例文件。

---

## ⚙️ 详细配置与高级工具 (按需展开)

<details>
<summary><b>🗄️ 1. OceanBase 记忆库与混合检索评测</b></summary>

不配置数据库时，记忆保存在本地 memory_data/<PROFILE_ID>/memories.json。配置 SeekDB/OceanBase 后，机器人会把自身记忆写入 bot_memories，并可只读接入已有聊天归档和媒体归档。

### 数据库环境变量配置
```env
MEMORY_ENABLED=true
MEMORY_DATABASE_DSN=user:password@tcp(127.0.0.1:2881)/companion_memory?charset=utf8mb4&parseTime=True&loc=Local
OLLAMA_BASE_URL=http://localhost:11434
OLLAMA_MODEL=qwen3-embedding:0.6b
MEMORY_EMBEDDING_DIMENSION=1024

MEMORY_INCLUDE_CHAT_ARCHIVE=false
MEMORY_CHAT_ARCHIVE_TABLE=chat_message_chunks
MEMORY_CHAT_ARCHIVE_TEXT_COLUMN=chunk_text
MEMORY_CHAT_ARCHIVE_TIME_COLUMN=end_time

MEMORY_INCLUDE_MEDIA_ARCHIVE=false
MEMORY_MEDIA_ARCHIVE_TABLE=media_assets
MEMORY_MEDIA_ROOT=/absolute/path/to/media
MEMORY_MEDIA_VAULT=memory_data/default/media
```

MEMORY_EMBEDDING_DIMENSION 必须与 embedding 模型输出一致。服务会在启动时补齐当前 profile 中缺失的记忆向量；同一轮检索只生成一次查询向量，并短时缓存重复查询。当前实现对向量结果和全文结果使用加权 RRF，避免直接混合两种不可比的原始分数。

### 数据库与记忆管理指令
*   **诊断表映射、覆盖率和索引：**
    ```bash
    go run ./cmd/memtool -diagnose
    ```
*   **记忆清洗与优化建议：**
    ```bash
    go run ./cmd/memtool -quality-audit
    ```
    ```bash
    go run ./cmd/memtool -quality-apply -quality-threshold 0.98
    ```
*   **RAG 检索质量与评测分析：**
    ```bash
    cp eval/sample.jsonl memory_data/default/retrieval-eval.jsonl
    go run ./cmd/rageval -input memory_data/default/retrieval-eval.jsonl
    ```
</details>

<details>
<summary><b>🖼️ 2. 图片 OCR 识别与媒体内容寻址</b></summary>

macOS 默认先调用系统 Vision 框架的 VNRecognizeTextRequest，失败后再按配置回退到飞书 OCR；其他系统会直接使用飞书兜底。本地 OCR 不会把图片发送给第三方。

### 编译与验证本地 OCR 辅助工具
```bash
# 编译 Apple Vision OCR 助手和机器人
make build

# 直接命令行验证图片 OCR
bin/macos-vision-ocr /absolute/path/to/image.png zh-Hans,en-US
```

### 媒体库内容寻址与归档导入
机器人接收的新图片会以 SHA-256 内容哈希存进 MEMORY_MEDIA_VAULT，目录权限为 0700，文件权限为 0600。消息 ID 是数据库幂等键，同一消息重复投递不会产生重复资产。

已有媒体归档可重新扫描并复制到受管媒体库。命令默认只预览；旧文件已删除时会明确统计为“缺失”，不会伪造图片：
```bash
go run ./cmd/mediactl -sources "/path/to/export,/path/to/photos"
go run ./cmd/mediactl -sources "/path/to/export,/path/to/photos" -apply
```

提示：数据量较小时使用精确余弦检索。只有在数据明显增长并经过延迟测试后，才建议按 OceanBase 官方文档增加 HNSW 及 APPROXIMATE 查询，不应只为“用了向量库”而创建近似索引。
</details>

<details>
<summary><b>💘 3. 恋爱笔记与其他扩展选项</b></summary>

恋爱笔记评论任务默认关闭。启用后首次运行只建立文档块基线，之后仅处理新增内容，并按每日上限控制评论数量：

```env
LOVE_NOTE_ENABLED=false
LOVE_NOTE_DOC_TOKEN=
LOVE_NOTE_WIKI_TOKEN=
LOVE_NOTE_MAX_DAILY_COMMENTS=2
```

外部搜索、主动话题、图片理解和 GitHub 轮询的开关与路径均在 .env.example 中说明。
</details>

<details>
<summary><b>🛠️ 4. 验证、诊断与隐私自检</b></summary>

### 编译与单元测试
```bash
make test
make build
```

### 环境健康检查（自检依赖与飞书连接）
```bash
go run ./cmd/doctor
go run ./cmd/doctor -online
```

### 飞书端真实链路测试（向指定会话发消息/图片）
```bash
go run ./cmd/smoke -mode stream
go run ./cmd/smoke -mode image -image /absolute/path/to/test.png
```

### 记忆查看与全文搜索
```bash
go run ./cmd/memtool -list
go run ./cmd/memtool -search "查询内容"
```

### 隐私泄露自检
仓库已忽略 .env、日志、二进制、运行状态、媒体文件、私有 profile 和本地扩展脚本。公开或部署前建议执行：
```bash
git status --short
git diff --check
git grep -nE 'sk-|cli_[A-Za-z0-9]+|ou_[A-Za-z0-9]+|password@tcp'
```

不要将聊天归档、照片、评测报告、真实人物 profile、数据库口令或飞书应用密钥提交到公开仓库。生产部署建议为数据库账号只授予所需库表权限，并让外部搜索、文档评论和媒体发送保持显式开关。完整威胁边界见 SECURITY.md。
</details>

---

## 📂 项目结构

```text
bin/              所有编译生成的本地二进制执行程序 (如 bot, macos-vision-ocr)
cmd/bot/          机器人主进程与 GitHub Actions 模式
cmd/memtool/      记忆迁移、清洗、检索与数据库诊断
cmd/mediactl/     旧媒体扫描、修复与幂等重新入库
cmd/rageval/      检索质量、隐私边界与延迟评测
cmd/doctor/       本地依赖、数据库和飞书真实健康检查
cmd/smoke/        飞书真实链路测试
cmd/query_graph/  命令行快速查询 GraphRAG 关系与实体列表
internal/feishu/  飞书 OpenAPI、长连接与 CardKit
internal/memory/  分层记忆、OceanBase 与混合检索
internal/ocr/     Apple Vision OCR 调用层
internal/media/   私有内容寻址媒体保险库
internal/lovenote/ 云文档增量评论任务
internal/llm/     DeepSeek 客户端与回复决策
internal/profile/ 人物和群成员配置
profiles/         人物设定与群聊成员 OpenID 配置文件
scripts/          本地日常任务与增量评论自动化 Python 脚本
tools/            macOS Vision OCR 原生 Swift 源码工具
web/              本地记忆审计面板与 GraphRAG 关系三元组展示
```

## 许可证

项目采用 [MIT](LICENSE) 许可证。提交代码前请阅读 CONTRIBUTING.md 和 SECURITY.md。
