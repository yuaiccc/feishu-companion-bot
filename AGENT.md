# AGENT.md - Maintenance Guide

Read this before changing the bot.

## Project Goal

Feishu Companion Bot is a configurable self-hosted companion bot. It should help a chat when the owner is away, but must not impersonate the owner. Keep the default codebase generic and safe for public open source use.

Private deployments belong in `.env`, `memory_data/`, and untracked profile files.

## Main Files

- `main.py`: entry point, intent routing, long connection, polling threads.
- `feishu_api.py`: Feishu/Lark SDK wrapper, messages, reactions, CardKit streaming, card callbacks.
- `summarizer.py`: DeepSeek prompts and reply generation.
- `context_manager.py`: bounded prompt context assembly.
- `latency.py`: local span-style timing logs.
- `memory.py`: local JSON memory, embedding, agentic write/rerank.
- `external_search.py`: local DeerFlow/OpenClaw web search integration.
- `call_notes.py`: Feishu Minutes transcript summaries.
- `love_note.py`: Feishu Docx/Wiki comment workflow.
- `actions_runner.py`: GitHub Actions fallback.
- `tests/test_regressions.py`: regression tests for prompts, cards, memory, search, notes, and Feishu behavior.

## Rules

- Check Feishu behavior against official docs or `lark-cli schema`; do not guess API fields.
- For Card JSON 2.0, put buttons directly in `body.elements`; do not use the old `{"tag": "action", "actions": [...]}` wrapper.
- Card button callbacks use `card.action.trigger`; the app must subscribe to the event in the Feishu Developer Console.
- Group messages should only trigger active replies when the bot is mentioned. P2P chats can reply directly.
- Keep memory privacy boundaries: `private` never enters prompts; `owner_only` only for owner; `public_to_target` can be used for the target user.
- Every new LLM context source needs an explicit budget in `context_manager.py`.
- Every user-visible Feishu text/card should pass through `text_safety.py`.
- Do not commit `.env`, logs, `state.json`, `memory_data/`, local QR codes, or private profiles.

## Feishu Permissions

Common scopes:

- `im:message`
- `im:message:send_as_bot`
- `im:resource`
- `im:message.reactions:write`
- `im:message:readonly`

For card callbacks, enable `card.action.trigger` in Events & Callbacks. The SDK handler is registered in `start_event_listener()`.

## Streaming Replies

Normal chat replies should prefer:

```python
reply_to_shushu_stream(...)
send_streaming_reply(...)
```

The card starts with `正在输入...`, updates `reply_text`, then disables `streaming_mode`. Buttons currently supported:

- `rephrase`: generate a rewritten reply from the cached short-term context.
- `continue`: generate one extra short continuation.
- `remember`: write a conservative owner-only manual memory.
- `forget`: remove very recent memories related to the exchange when possible.

The stream message ID is cached in `state.json.streaming_reply_contexts` for button callbacks. Keep it short-lived and compact.

## Context And Latency

All normal replies and activity summaries should use `context_manager.build_reply_context()`.

`main.py` should log local latency spans for:

- `read_messages`
- `search_memory`
- `call_notes`
- first DeepSeek token
- reply sent

This is intentionally local and LangSmith-like, without sending private data to external telemetry.

## GitHub Activity Cards

Activity cards should stay compact and table-first. Commit/star rows must be understandable to non-developers. Do not add a sentimental top summary to every GitHub card.

## Tests

Run before committing:

```bash
.venv/bin/python -m py_compile config.py context_manager.py feishu_api.py main.py memory.py summarizer.py tests/test_regressions.py
.venv/bin/python -m unittest tests.test_regressions
git diff --check
```
