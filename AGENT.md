# AGENT.md - 维护说明

改这个项目之前先读这里。

## 项目目标

飞书陪伴机器人是一个可配置、自托管的陪伴型机器人。它可以在 owner 暂时不在线时帮忙回应、整理信息、保留记忆，但不能伪装成 owner 本人。

默认代码库必须保持通用、可公开、可复用。私有部署内容只应该放在 `.env`、`memory_data/`、未跟踪 profile 或未跟踪本地扩展文件中。

## 目录约定

- `main.py`：本地长连接入口，负责启动轮询线程和飞书事件监听。
- `actions_runner.py`：GitHub Actions 兜底入口。
- `feishu_companion/`：通用业务模块。
- `profiles/`：公开的通用 profile 模板。
- `tests/`：回归测试。
- `launchd/`：macOS LaunchAgent 模板。

## 关键模块

- `feishu_api.py`：飞书 SDK 封装，包含消息、表情、卡片、按钮回调。
- `summarizer.py`：DeepSeek 回复生成。
- `context_manager.py`：有预算的上下文拼接。
- `latency.py`：本地 span 风格耗时日志。
- `memory.py`：本地 JSON 记忆、向量、agentic 写入和 rerank。
- `external_search.py`：本地 DeerFlow/OpenClaw 搜索。
- `call_notes.py`：飞书妙记/通话纪要摘要。
- `github_client.py`：GitHub Events 和 private repo commit 轮询。
- `notifier.py`：GitHub 动态飞书卡片。
- `health.py`：服务自检卡片。
- `memory_audit.py`：记忆审计卡片。

## 规则

- 飞书字段和行为必须对照官方文档或 `lark-cli schema`，不要猜。
- Card JSON 2.0 的按钮直接放在 `body.elements`，不要用旧版 `{"tag": "action", "actions": [...]}`。
- 卡片按钮回调依赖 `card.action.trigger`，需要在飞书开发者后台订阅。
- 群聊只有 @ 机器人时主动回复；私聊可以直接回复。
- 记忆隐私边界不能破坏：`private` 不进 prompt，`owner_only` 只给 owner，`public_to_target` 可以给目标用户。
- 新增任何 LLM 上下文来源，都要在 `context_manager.py` 里设置明确预算。
- 所有用户可见飞书文本和卡片都要经过 `text_safety.py`。
- 不要提交 `.env`、日志、`state.json`、`memory_data/`、二维码、私有 profile、私有本地扩展。

## 流式回复

私聊可以走：

```python
reply_to_shushu_stream(...)
send_streaming_reply(...)
```

卡片先显示 `正在输入...`，逐步更新 `reply_text`，结束后关闭 `streaming_mode`。群聊默认走普通文本回复，不要把操作按钮塞进群里。

私聊流式卡片可以按需支持：

- `rephrase`：根据缓存上下文换个说法。
- `continue`：继续补充一句。

记忆确认使用单独的私聊卡片：

- `remember_candidate`：owner 确认后写入 owner-only 记忆。
- `dismiss_candidate`：忽略该候选。

按钮上下文存在 `state.json.streaming_reply_contexts`，必须保持短期、紧凑。普通消息不要自动入库，除非 owner 明确确认。

## GitHub 动态

动态卡片保持表格优先，commit/star 行要让非开发者也能看懂。不要给每张 GitHub 卡片加很腻的情绪总结。

幂等规则：

- 先按 GitHub event id 去重。
- PushEvent 再按 head sha 生成指纹，避免公开 Events API 和 private repo 轮询重复报同一个 commit。

## 本地私有扩展

不适合公开的工作流不要提交。需要每日私有任务时，在未跟踪文件里实现：

```python
def run_daily_job(force: bool = False) -> str:
    ...

def preview_daily_job() -> str:
    ...
```

然后通过 `.env` 配：

```env
LOCAL_DAILY_JOB_MODULE=local_daily_job
LOCAL_DAILY_JOB_RUN_AT=23:55
```

`local_daily_job.py` 已加入 `.gitignore`。

## 提交前检查

```bash
.venv/bin/python -m py_compile main.py actions_runner.py feishu_companion/*.py tests/test_regressions.py
.venv/bin/python -m unittest tests.test_regressions
git diff --check
```
