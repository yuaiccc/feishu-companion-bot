# Project History

飞书群聊机器人。它的人设是秋酿（三哥）本人不在时的临时代替，用来陪微里（舒烨）说话、回答问题，并在必要时汇报 GitHub 活动。

## 运行模式

- 本地实时模式：`python3 main.py`，启动飞书长连接和 GitHub 轮询，适合电脑开机时使用。
- GitHub Actions 兜底模式：`.github/workflows/bot.yml` 每 5 分钟运行 `actions_runner.py`，适合电脑关机、休眠或本地机器人不在线时使用。
- macOS 常驻模式：`launchd/com.xujunshan.github-activity-bot.plist` 用 `caffeinate` 包住本地进程，登录后自动启动并阻止空闲睡眠。

## 人设边界

- 秋酿/三哥 = 许君山；微里/舒舒 = 舒烨。
- 机器人不是第三方助手，而是秋酿暂时不在时的本人替代。
- 默认关系重心是秋酿想着微里；代码、项目、GitHub 只是状态线索，除非被明确问到，不要反复提。

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

## 云端兜底

GitHub Actions 使用 Environment `feishu` 下的 secrets，不使用 repository secrets。变量名按 `AGENT.md` 配置，其中 GitHub 相关变量在 Actions 中使用 `GH_USERNAME`、`GH_TOKEN`、`GH_PRIVATE_REPOS`，避免 GitHub 保留名前缀。

飞书接口字段以官方文档为准：https://open.feishu.cn/document/home/index

## 信息源

- 本地窗口状态：`local_apps.py` 通过 AppleScript 读取前台应用和窗口标题，只在本地模式可用。
- 通话纪要：`call_notes.py` 通过飞书妙记官方接口读取已配置 `minute_token` 的文字记录，默认关闭。开启前要配置 `CALL_NOTES_ENABLED=true` 和 `FEISHU_MINUTE_TOKENS`，并确保应用具备妙记读取/导出权限。
- GitHub 活动：用于兜底判断时间线，不应该盖过秋酿和微里的关系上下文。
