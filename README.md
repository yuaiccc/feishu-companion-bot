# Project History

飞书群聊机器人。它的人设是三哥的小弟，在秋酿（三哥）本人不在时帮忙照看群聊、给舒舒（舒烨）传话和解释状态，并在必要时汇报 GitHub 活动。

## 运行模式

- 本地实时模式：`python3 main.py`，启动飞书长连接和 GitHub 轮询，适合电脑开机时使用。
- GitHub Actions 兜底模式：`.github/workflows/bot.yml` 每 5 分钟运行 `actions_runner.py`，适合电脑关机、休眠或本地机器人不在线时使用。
- macOS 常驻模式：`launchd/com.xujunshan.github-activity-bot.plist` 用 `caffeinate` 包住本地进程，登录后自动启动并阻止空闲睡眠。

## 人设边界

- 秋酿/三哥 = 许君山；舒舒/烨子 = 舒烨。
- 群里直接称呼她时，在"舒舒"和"烨子"里任选一个；这是同一个人，不要并列说"舒舒和烨子"。
- 机器人是三哥的小弟，不是秋酿分身，也不要冒充秋酿本人；对小弟来说舒舒是大哥的老婆，维护时按这个分寸照看她的安全感。
- 对舒舒说话时用"三哥..."来转述状态和心意，不要用三哥第一人称说"我想你/我在干嘛"。
- 默认关系重心是秋酿想着舒舒；代码、项目、GitHub 只是状态线索，除非被明确问到，不要反复提。

## 接手入口

先读 `AGENT.md`。那里记录了飞书权限、Actions secrets、群聊 @ 规则、常见坑和关系背景。

## 本地运行

```bash
pip install -r requirements.txt
cp .env.example .env
python3 main.py
```

默认 `DRY_RUN=true` 不会真的发飞书消息。生产运行前把 `.env` 里的 `DRY_RUN=false`，并补齐 DeepSeek、飞书应用、GitHub token 等配置。

本机长期在线推荐安装 LaunchAgent：

```bash
mkdir -p ~/Library/LaunchAgents
cp launchd/com.xujunshan.github-activity-bot.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/501 ~/Library/LaunchAgents/com.xujunshan.github-activity-bot.plist
launchctl kickstart -k gui/501/com.xujunshan.github-activity-bot
tail -f bot.log
```

即时回复和私聊回复依赖本地长连接。GitHub Actions 兜底只读取 `FEISHU_CHAT_ID` 指向的群聊历史消息，不读取私聊。

如果配置了 `FEISHU_STATUS_CHAT_ID`，本地服务启动、重启、轮询异常、长连接退出时会向这个单聊推送状态。电脑关机或系统睡死时，本地进程无法主动推送，只能依赖 GitHub Actions 兜底。

长连接补发的旧消息默认超过 10 分钟会跳过；已撤回、找不到或纯 @ 的历史消息不会进入聊天上下文。

## 云端兜底

GitHub Actions 使用 Environment `feishu` 下的 secrets，不使用 repository secrets。变量名按 `AGENT.md` 配置，其中 GitHub 相关变量在 Actions 中使用 `GH_USERNAME`、`GH_TOKEN`、`GH_PRIVATE_REPOS`，避免 GitHub 保留名前缀。

飞书接口字段以官方文档为准：https://open.feishu.cn/document/home/index

## 信息源

- 本地窗口/在席状态：`local_apps.py` 通过 AppleScript 读取前台应用和窗口标题，并通过 macOS `HIDIdleTime`/ConsoleUser 会话推测三哥是否在电脑前，只在本地模式可用。这个判断是概率推测，不要说成确定事实。
- 通话纪要：`call_notes.py` 通过飞书妙记官方接口读取已配置 `minute_token` 的文字记录，默认关闭。开启前要配置 `CALL_NOTES_ENABLED=true` 和 `FEISHU_MINUTE_TOKENS`，并确保应用具备妙记读取/导出权限。读取后会先整理成短摘要并缓存，只给回复模型关系上下文，不把原文整段塞进去。
- 外部搜索：`external_search.py` 通过本机 `openclaw infer web search` 搜索网页，再用 DeepSeek 整理为"短结论 + 表格 + 来源链接"卡片。它只在本地模式可用；Actions 兜底不能调用三哥电脑上的 OpenClaw。
- 旁听辅助：`passive_assistant.py` 接收未 @ 机器人的群聊消息，只在最近时间窗口内出现资料型话题、群里静默一段时间、同话题不在冷却中时，才用 OpenClaw 补一张背景资料卡片。已处理消息和话题冷却写入 `state.json`，避免同一个问题重复回答。
- 每日恋爱笔记：`love_note.py` 每天按 `LOVE_NOTE_RUN_AT` 读取已有飞书 Wiki/Docx 恋爱笔记正文，只对新增正文生成嗑糖短评，每天最多 2 条，并以局部评论挂到匹配短评的正文段落上，不向正文追加内容。预览用 `python main.py --daily-note-preview`，手动写入测试用 `python main.py --daily-note-test`。
- GitHub 活动：用于兜底判断时间线，不应该盖过秋酿和舒舒的关系上下文。
- 状态查询和 GitHub 查询分开处理：问"在干嘛/最近活动"默认只看本地窗口状态；明确问 GitHub、提交、代码、仓库时才推 GitHub 卡片。
- 外部搜索和近期活动分开处理：问"最近B站哪些新番热门/查一下/搜索"走 OpenClaw；问"三哥最近活动/在干嘛"仍走电脑活动。
- 活动卡片只合并同仓库且组内首尾时间跨度不超过 1 小时的提交；超过 1 小时必须分成多行。
- 活动卡片里的提交说明会强制改写成中文短句，避免舒舒看到英文 commit 标题看不懂。
- 所有发往飞书的文本和卡片都会先经过 `text_safety.py` 统一清洗。

## 记忆管理

`memory.py` 仍然使用本地 JSON，默认写入 `memory_data/memories.json`。新增记忆会先用 DeepSeek 抽取事实，再做简单归类、重要度评分和去重；重复事实只更新 `last_seen`/`seen_count`，不会无限堆叠。默认最多保留 200 条，可用 `MEMORY_MAX_ITEMS` 调整。

## 旁听辅助

默认开启但很克制。它不回复普通情绪闲聊，只对番剧、作品、地名、人物、新闻、热榜等资料型话题尝试补背景。默认参数：

- `PASSIVE_ASSIST_QUIET_SECONDS=75`：群里静默 75 秒后才可能发。
- `PASSIVE_ASSIST_RECENT_WINDOW_SECONDS=480`：只看最近 8 分钟消息。
- `PASSIVE_ASSIST_TOPIC_COOLDOWN_SECONDS=1800`：同话题 30 分钟内不重复。
- `PASSIVE_ASSIST_MAX_PER_HOUR=2`：每小时最多补 2 次。

## 每日恋爱笔记

默认关闭，打开 `LOVE_NOTE_ENABLED=true` 后，本地长连接进程会启动每日评论线程。它读取现有恋爱笔记正文，只评论新增内容；如果当天没有新增正文，就不评论。每日最多 2 条短评，评论会挂在最适合的正文段落上，不覆盖原文，也不向正文追加噪声。

配置项：

- `LOVE_NOTE_WIKI_TOKEN`：Wiki 链接里的 token，例如 `IwfGwwGBBiQ4t3k9MW1cjJuDnab`。
- `LOVE_NOTE_DOC_TOKEN`：解析后的 docx token，例如 `TjKadw7I8oqQT4xyCC0c2WhEnPe`；填了可以少一次 Wiki 解析。
- `LOVE_NOTE_RUN_AT=23:55`：每天写入时间。

手动命令：

- `python main.py --daily-note-preview`：只生成短评预览，不写入文档、不更新状态。
- `python main.py --daily-note-test`：强制生成并创建今天的文档短评评论，会绕过当天幂等检查。

状态字段：

- `love_note_seen_block_ids`：已处理过的正文 block，避免重复评论旧内容。
- `love_note_daily_comment_counts`：每日评论计数，默认最多 2 条。
