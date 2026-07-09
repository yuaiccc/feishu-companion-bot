# Feishu Companion Bot (飞书专属陪伴小弟) 🚀

这是一个**高解耦、通用化、以情感与认知网络为核心**的飞书陪伴机器人框架。
它能够适配任何想要快速接入的个人用户，提供温暖、连贯、且具备长短期记忆和动态关系图谱提炼能力的飞书专属聊天体验。

---

## 🌟 核心特色 (Features)

### 1. ⚙️ 热插拔式模块化开关 (Feature Toggles)
所有的核心记忆与情感处理逻辑皆已完全解耦。您可以在内嵌的 Web 面板上，一键开启或关闭以下模块：
*   **情感温度计 (`module_emotion_tracker`)**：实时计算主人的“情绪值”与“亲密度”，并基于波动记录写入历史变化表。
*   **图谱纠偏与演进 (`module_graph_self_evolution`)**：自动识别新老记忆中的三元组逻辑冲突，并由大模型进行关系覆盖/否定句物理删除。
*   **跨会话多轮提炼 (`module_multi_turn_graph`)**：跨对话识别“他/她/这个”等隐含代词，将其消解并提炼为标准的图谱实体。
*   **图片 MD5 去重缓存 (`module_image_dedup`)**：静默秒回复已识图片，免去大模型重复视觉提炼。

### 2. 🧠 开放式 GraphRAG 关系图谱
*   **双轨并行存储**：使用 `bot_memories` 物理表存储非结构化的长期记忆，使用 `knowledge_relations` 物理表存储关系图谱。
*   **自由关系边**：图谱支持完全开放式的 Predicate 关系命名（如：别名、同事、喜欢、毕业于、前女友、讨厌等），用户可在控制台无限制自由定义并直接写回库中。

### 3. 🎨 单二进制内嵌极简 Web 控制面板 (SPA Go Embed)
*   **一体化极简发布**：无需配置 Nginx 等繁琐代理，Vite + React (TypeScript) 静态资源完美内嵌在 Go 的可执行程序中，开箱即用。
*   **原生 SVG 双折线趋势图**：手写响应式 SVG Canvas 绘制 Mood & Affinity 历史起伏。
*   **实时 OceanBase 监控**：带动态呼吸灯，实时轮询并展示 OB 的版本、连接池负载 (Active/Idle) 与图谱数据行数。

---

## 🚀 快速启动指南 (Quick Start)

### 1. 配置文件设置
将项目根目录下的 `.env.example` 复制为 `.env`，并配置您的环境参数：
```ini
# 飞书应用凭证
FEISHU_APP_ID=cli_xxx
FEISHU_APP_SECRET=xxx

# 飞书用户 OpenID (用于标识主人)
FEISHU_OWNER_OPENID=ou_xxx

# 大模型配置 (推荐使用 DeepSeek V3)
DEEPSEEK_API_KEY=sk-xxx
DEEPSEEK_BASE_URL=https://api.deepseek.com/v1
DEEPSEEK_MODEL=deepseek-chat

# OceanBase 数据库连接串
MEMORY_DATABASE_DSN="user:password@tcp(127.0.0.1:3306)/database_name?charset=utf8mb4&parseTime=True&loc=Local"
```

### 2. 前端静态网页打包
在 `/web` 目录下进行前端 TypeScript 编译与 Vite 打包：
```bash
cd web
npm run build
```
*(打包后的资源会自动重定向复制进 `cmd/bot/web/dist` 下，完美越过 Go embed 的父级目录限制)*

### 3. 编译并启动机器人
回到项目根目录下：
```bash
# 编译
go build -o bot ./cmd/bot

# 启动
./bot
```
后台服务拉起后，会在终端打印监听地址，您可以直接在浏览器中打开：
👉 **`http://127.0.0.1:8080/`**

---

## ⚙️ 角色初始化引导 (Onboarding Wizard)

对于第一天新接入的用户，Web 控制面板提供了直观的**新手角色配置向导**：
1.  **称呼定义**：填入“主人称呼”（如：三哥）、“机器人称呼”（如：小弟）、以及“陪伴伴侣的称呼”（如：舒舒）；
2.  **角色 Prompt 写入**：点击“装载推荐模板”或手写您的个性化机器人 System Prompt；
3.  **确认保存**：一键保存后，机器人将立即热装载最新的配置，并为您在图谱中预设好初始别名映射关系！

---

## 🧪 单元与集成测试 (Test Suite)

运行以下命令执行全回归测试：
```bash
go test -v ./cmd/bot
```
测试覆盖了**多轮代词指代消解、自适应情感微调、图片去重校验、及图谱纠偏冲突调和**等 11 项核心场景，确保系统在更新时的 100% 稳定性。
