import json
import sys
import unittest
from unittest.mock import patch

import actions_runner
import call_notes
import context_manager
import commit_text
import external_search
import feishu_api
import health
import local_apps
import love_note
import memory_audit
import memory
import notifier
import passive_assistant
import proactive_topic
import profile as bot_profile
import summarizer
import state
from main import _classify_tool_intent
from text_safety import assert_public_text_clean, sanitize_card, sanitize_public_text


class BotRegressionTests(unittest.TestCase):
    def test_public_text_sanitizer_replaces_disallowed_nickname(self):
        self.assertEqual(sanitize_public_text("\u5fae\u91cc宝贝"), "舒舒宝贝")
        self.assertEqual(sanitize_public_text("舒舒和烨子都可以看看"), "舒舒都可以看看")

    def test_cards_are_sanitized_recursively(self):
        card = {
            "card": {
                "body": {
                    "elements": [
                        {"tag": "markdown", "content": "给\u5fae\u91cc看的内容"},
                    ]
                }
            }
        }
        sanitized = sanitize_card(card)
        assert_public_text_clean(sanitized)
        self.assertIn("舒舒", sanitized["card"]["body"]["elements"][0]["content"])

    def test_local_activity_card_starts_with_table_not_summary(self):
        notifier._get_repo_desc = lambda repo: "和舒舒的聊天机器人"
        activities = [
            {
                "type": "PushEvent",
                "repo": "example/feishu-companion-bot",
                "created_at": "2026-06-30T08:24:00Z",
                "detail": {
                    "commit_count": 1,
                    "commit_messages": ["Remove activity summary"],
                },
            }
        ]
        elements = notifier.build_message(activities)["card"]["body"]["elements"]
        self.assertEqual(elements[0]["tag"], "table")
        assert_public_text_clean(elements)

    def test_actions_activity_card_starts_with_table_not_summary(self):
        actions_runner.fetch_repo_desc = lambda repo: "和舒舒的聊天机器人"
        activities = [
            {
                "type": "PushEvent",
                "repo": {"name": "example/feishu-companion-bot"},
                "created_at": "2026-06-30T08:24:00Z",
                "payload": {"commits": [{"message": "Remove activity summary"}]},
            }
        ]
        elements = actions_runner.build_commit_card(activities)["card"]["body"]["elements"]
        self.assertEqual(elements[0]["tag"], "table")
        assert_public_text_clean(elements)

    def test_activity_rows_use_lightweight_commit_summary(self):
        notifier._get_repo_desc = lambda repo: "和舒舒的聊天机器人"
        with patch.object(
            commit_text,
            "_summarize_activity_with_deepseek",
            return_value="给和舒舒的聊天机器人新增了恋爱笔记评论功能",
        ):
            card = notifier.build_message([
                {
                    "type": "PushEvent",
                    "repo": "example/feishu-companion-bot",
                    "created_at": "2026-06-30T08:24:00Z",
                    "detail": {
                        "commit_count": 1,
                        "commit_messages": ["feat: add love note comments"],
                    },
                }
            ])
        rows = card["card"]["body"]["elements"][0]["rows"]
        self.assertEqual(rows[0]["activity"], "给和舒舒的聊天机器人新增了恋爱笔记评论功能")
        self.assertNotIn("feat:", rows[0]["activity"])

    def test_activity_rows_use_lightweight_star_summary(self):
        actions_runner.fetch_repo_desc = lambda repo: "WCDB is a cross-platform database framework developed by WeChat."
        with patch.object(commit_text, "_summarize_activity_with_deepseek", return_value=""):
            card = actions_runner.build_commit_card([
                {
                    "type": "WatchEvent",
                    "repo": {"name": "Tencent/wcdb"},
                    "created_at": "2026-06-30T08:24:00Z",
                    "payload": {},
                }
            ])
        rows = card["card"]["body"]["elements"][0]["rows"]
        self.assertIn("收藏了一个大概和微信跨平台数据库有关的项目", rows[0]["activity"])

    def test_tool_intent_separates_status_from_github(self):
        self.assertEqual(_classify_tool_intent("三哥最近活动", "舒舒"), "status")
        self.assertEqual(_classify_tool_intent("三哥最近提交了什么", "舒舒"), "github")
        self.assertEqual(_classify_tool_intent("最近B站哪些新番热门", "舒舒"), "search")
        self.assertEqual(_classify_tool_intent("帮我查一下最近新闻", "舒舒"), "search")
        self.assertEqual(_classify_tool_intent("机器人健康检查", "三哥"), "health")
        self.assertEqual(_classify_tool_intent("打开记忆审计面板", "三哥"), "memory_audit")
        self.assertEqual(_classify_tool_intent("想你了", "舒舒"), "none")

    def test_persona_is_helper_not_impersonation(self):
        prompts = "\n".join([
            summarizer.RELATIONSHIP_CONTEXT,
            summarizer.SYSTEM_PROMPT,
            summarizer.REPLY_PROMPT_SHUSHU,
            external_search.summarize_search_results.__doc__ or "",
        ])
        self.assertIn("不是用户本人", prompts)
        self.assertIn("不要冒充用户", prompts)
        self.assertNotIn("你是用户本人", prompts)
        self.assertNotIn("用第一人称跟对方说话", prompts)

    def test_aliases_are_one_person_not_parallel_names(self):
        prompts = "\n".join([
            summarizer.RELATIONSHIP_CONTEXT,
            summarizer.SYSTEM_PROMPT,
            summarizer.REPLY_PROMPT_SHUSHU,
        ])
        self.assertIn("直接称呼对方", prompts)
        self.assertNotIn("只用舒舒或烨子", prompts)

    def test_default_profile_has_no_private_names(self):
        with patch.object(bot_profile, "PROFILE_ID", "default"):
            bot_profile.load_profile.cache_clear()
            context = bot_profile.relationship_context()
        self.assertNotIn("三哥", context)
        self.assertNotIn("舒舒", context)
        self.assertIn("可配置", context)
        bot_profile.load_profile.cache_clear()

    def test_example_couple_profile_supplies_relationship_context(self):
        with patch.object(bot_profile, "PROFILE_ID", "example-couple"):
            bot_profile.load_profile.cache_clear()
            context = bot_profile.relationship_context()
            addressing = bot_profile.target_addressing_instruction()
        self.assertIn("A 和 B", context)
        self.assertIn("宝贝", addressing)
        self.assertIn("不要把两个名字并列说出来", addressing)
        bot_profile.load_profile.cache_clear()

    def test_call_notes_fallback_extracts_relationship_context(self):
        transcript = "\n".join([
            "随便聊一点普通事情",
            "舒舒说晚上记得早点来打电话，想听你说晚安",
            "用户答应散步回来就找她",
        ])
        summary = call_notes._fallback_summarize_transcript(transcript)
        self.assertIn("记得早点来打电话", summary)
        self.assertIn("散步回来就找她", summary)
        assert_public_text_clean(summary)

    def test_memory_visibility_blocks_owner_only_for_target(self):
        memories = [
            {
                "id": "safe",
                "content": "舒舒喜欢不加糖的饮料。",
                "category": "preference",
                "visibility": "public_to_target",
                "importance": 3,
                "seen_count": 1,
            },
            {
                "id": "addr",
                "content": "三哥家住某某小区 71 栋 3 单元。",
                "category": "person",
                "visibility": "owner_only",
                "importance": 5,
                "seen_count": 1,
            },
            {
                "id": "secret",
                "content": "DeepSeek API Key 是 sk-abcdefghijklmnop。",
                "category": "note",
                "visibility": "private",
                "importance": 5,
                "seen_count": 1,
            },
        ]
        with patch.object(memory, "_load_all", return_value=memories), patch.object(
            memory,
            "MEMORY_AGENTIC_RAG_ENABLED",
            False,
        ), patch.object(
            memory,
            "MEMORY_EMBEDDING_PROVIDER",
            "local_hash",
        ):
            target_results = memory.search_memories("住哪里 不加糖", audience="target", top_k=5)
            owner_results = memory.search_memories("住哪里 不加糖", audience="owner", top_k=5)
        self.assertIn("舒舒喜欢不加糖的饮料。", target_results)
        self.assertNotIn("三哥家住某某小区 71 栋 3 单元。", target_results)
        self.assertNotIn("DeepSeek API Key 是 sk-abcdefghijklmnop。", owner_results)
        self.assertNotIn("三哥家住某某小区 71 栋 3 单元。", owner_results)

    def test_memory_entry_gets_local_embedding_and_visibility(self):
        with patch.object(memory, "MEMORY_EMBEDDING_PROVIDER", "local_hash"):
            entry = memory._normalize_memory_entry({"content": "舒舒喜欢晚上散步。"}, 1)
            private_entry = memory._normalize_memory_entry({"content": "家住某某小区 71 栋 3 单元。"}, 2)
        self.assertEqual(entry["visibility"], "public_to_target")
        self.assertEqual(entry["embedding_model"], "local-hash-ngram-v1")
        self.assertEqual(len(entry["embedding"]), memory.MEMORY_EMBEDDING_DIM)
        self.assertEqual(private_entry["visibility"], "private")

    def test_memory_clean_store_removes_noise_and_dedupes(self):
        memories = [
            {"id": "1", "content": "你好"},
            {"id": "2", "content": "舒舒喜欢不加糖的东方树叶。"},
            {"id": "3", "content": "舒舒喜欢不加糖的东方树叶。"},
        ]
        with patch.object(memory, "_load_all", return_value=memories), patch.object(
            memory,
            "_save_all",
        ) as save_mock, patch.object(memory, "MEMORY_EMBEDDING_PROVIDER", "local_hash"):
            result = memory.clean_memory_store(dry_run=False)
        self.assertEqual(result["before"], 3)
        self.assertEqual(result["after"], 1)
        self.assertEqual(result["removed"], 1)
        self.assertEqual(result["merged"], 1)
        save_mock.assert_called_once()

    def test_memory_write_low_confidence_requires_confirmation(self):
        decision = memory._normalize_write_decision(
            {
                "action": "create",
                "content": "舒舒可能喜欢某种饮料。",
                "category": "preference",
                "visibility": "public_to_target",
                "confidence": 0.2,
            },
            "舒舒可能喜欢某种饮料。",
            "preference",
            "public_to_target",
        )
        self.assertEqual(decision["action"], "confirm")

    def test_private_memory_write_policy_stays_local(self):
        with patch.object(memory.requests, "post") as post_mock:
            decision = memory._decide_memory_write("三哥家住某某小区 71 栋 3 单元。", [])
        post_mock.assert_not_called()
        self.assertEqual(decision["action"], "create")
        self.assertEqual(decision["visibility"], "private")

    def test_add_memories_honors_agentic_create_and_ignore(self):
        saved = {}

        def capture(memories):
            saved["memories"] = memories

        decisions = [
            {"action": "ignore", "reason": "寒暄"},
            {
                "action": "create",
                "content": "舒舒喜欢不加糖的东方树叶。",
                "category": "preference",
                "visibility": "public_to_target",
                "confidence": 0.92,
            },
        ]
        with patch.object(memory, "_extract_facts", return_value=["你好", "舒舒喜欢不加糖的东方树叶。"]), patch.object(
            memory,
            "_load_all",
            return_value=[],
        ), patch.object(
            memory,
            "_save_all",
            side_effect=capture,
        ), patch.object(
            memory,
            "_decide_memory_write",
            side_effect=decisions,
        ), patch.object(
            memory,
            "MEMORY_EMBEDDING_PROVIDER",
            "local_hash",
        ):
            new_entries = memory.add_memories([{"time": "10:00", "sender": "舒舒", "content": "你好"}])
        self.assertEqual(len(new_entries), 1)
        self.assertEqual(saved["memories"][0]["content"], "舒舒喜欢不加糖的东方树叶。")
        self.assertEqual(saved["memories"][0]["source_type"], "agentic_write")

    def test_add_memories_honors_agentic_update(self):
        existing = [{
            "id": "m1",
            "content": "舒舒喜欢东方树叶。",
            "category": "preference",
            "visibility": "public_to_target",
            "importance": 3,
            "confidence": 0.8,
            "seen_count": 1,
        }]
        saved = {}

        def capture(memories):
            saved["memories"] = memories

        with patch.object(memory, "_extract_facts", return_value=["舒舒喜欢不加糖的东方树叶。"]), patch.object(
            memory,
            "_load_all",
            return_value=existing,
        ), patch.object(
            memory,
            "_save_all",
            side_effect=capture,
        ), patch.object(
            memory,
            "_decide_memory_write",
            return_value={
                "action": "update",
                "target_id": "m1",
                "content": "舒舒喜欢不加糖的东方树叶。",
                "category": "preference",
                "visibility": "public_to_target",
                "confidence": 0.9,
            },
        ), patch.object(
            memory,
            "MEMORY_EMBEDDING_PROVIDER",
            "local_hash",
        ):
            new_entries = memory.add_memories([{"time": "10:00", "sender": "舒舒", "content": "喜欢不加糖"}])
        self.assertEqual(new_entries, [])
        self.assertEqual(len(saved["memories"]), 1)
        self.assertEqual(saved["memories"][0]["content"], "舒舒喜欢不加糖的东方树叶。")
        self.assertEqual(saved["memories"][0]["seen_count"], 2)

    def test_search_interaction_writes_compact_interest_memory(self):
        with patch.object(memory, "add_manual_memory") as add_mock:
            external_search.remember_search_interaction(
                "查一下 CLANNAD 古河渚",
                [{"title": "CLANNAD 角色介绍", "snippet": "古河渚是作品角色", "url": "https://example.com"}],
                actor="舒舒",
            )
        add_mock.assert_called_once()
        fact = add_mock.call_args.args[0]
        self.assertIn("舒舒对", fact)
        self.assertIn("CLANNAD", fact)

    def test_memory_audit_hides_private_content_for_group_audience(self):
        memories = [
            {
                "id": "public",
                "content": "舒舒喜欢不加糖的东方树叶。",
                "visibility": "public_to_target",
                "confidence": 0.9,
            },
            {
                "id": "private",
                "content": "三哥家住某某小区 71 栋 3 单元。",
                "visibility": "private",
                "confidence": 0.8,
            },
        ]
        with patch.object(memory_audit, "_load_all", return_value=memories):
            target_card = memory_audit.build_memory_audit_card(audience="target")
            owner_card = memory_audit.build_memory_audit_card(audience="owner")
        self.assertNotIn("71 栋", str(target_card))
        self.assertIn("[私密记忆已隐藏]", str(target_card))
        self.assertIn("71 栋", str(owner_card))

    def test_health_card_uses_table(self):
        with patch.object(health, "_config_check", return_value={"item": "飞书配置", "status": "正常", "detail": "ok"}), patch.object(
            health,
            "_deepseek_check",
            return_value={"item": "DeepSeek", "status": "正常", "detail": "ok"},
        ), patch.object(
            health,
            "_ollama_check",
            return_value={"item": "Ollama 向量", "status": "正常", "detail": "ok"},
        ), patch.object(
            health,
            "_openclaw_check",
            return_value={"item": "OpenClaw", "status": "正常", "detail": "ok"},
        ), patch.object(
            health,
            "_memory_check",
            return_value={"item": "记忆库", "status": "正常", "detail": "ok"},
        ), patch.object(
            health,
            "_local_status_check",
            return_value={"item": "本机状态", "status": "正常", "detail": "ok"},
        ):
            card = health.build_health_card()
        self.assertEqual(card["card"]["body"]["elements"][0]["tag"], "table")
        assert_public_text_clean(card)

    def test_streaming_reply_reuses_token_and_batches_updates(self):
        updates = []
        with patch.object(feishu_api, "DRY_RUN", False), patch.object(
            feishu_api,
            "create_streaming_card",
            return_value="card_1",
        ) as create_mock, patch.object(
            feishu_api,
            "send_card_entity",
            return_value=True,
        ), patch.object(
            feishu_api,
            "_get_token",
            return_value="tenant_token",
        ) as token_mock, patch.object(
            feishu_api,
            "update_streaming_text",
            side_effect=lambda card_id, text, sequence, token="": updates.append((text, sequence, token)) or True,
        ), patch.object(
            feishu_api,
            "stop_streaming",
            return_value=True,
        ) as stop_mock, patch.object(
            feishu_api.time,
            "time",
            side_effect=[0.0, 0.1, 0.2, 0.3],
        ):
            text = feishu_api.send_streaming_reply(iter(["你", "好", "呀"]), update_interval=10)
        self.assertEqual(text, "你好呀")
        create_mock.assert_called_once()
        self.assertEqual(create_mock.call_args.kwargs.get("title"), "回复")
        self.assertEqual(create_mock.call_args.kwargs.get("initial_text"), "正在输入...")
        token_mock.assert_called_once()
        self.assertEqual(updates[0], ("你", 1, "tenant_token"))
        self.assertEqual(updates[-1][0], "你好呀")
        stop_mock.assert_called_once()

    def test_streaming_card_uses_schema2_buttons(self):
        payloads = []

        class Resp:
            def json(self):
                return {"code": 0, "data": {"card_id": "card_1"}}

        with patch.object(feishu_api, "_get_token", return_value="tenant_token"), patch.object(
            feishu_api._requests,
            "post",
            side_effect=lambda *args, **kwargs: payloads.append(kwargs["json"]) or Resp(),
        ):
            card_id = feishu_api.create_streaming_card()

        self.assertEqual(card_id, "card_1")
        card_json = json.loads(payloads[0]["data"])
        elements = card_json["body"]["elements"]
        self.assertEqual(card_json["schema"], "2.0")
        self.assertNotIn("action", [e.get("tag") for e in elements])
        buttons = [e for e in elements if e.get("tag") == "button"]
        self.assertEqual(
            [b["value"]["action"] for b in buttons],
            ["rephrase", "continue", "remember", "forget"],
        )

    def test_reply_context_is_bounded_and_auditable(self):
        messages = [
            {"time": f"07-01 12:{i:02d}", "sender": "舒舒", "content": "消息" + str(i) * 60}
            for i in range(20)
        ]
        memories = [f"记忆{i}" + "很重要" * 80 for i in range(8)]
        with patch.object(context_manager, "CONTEXT_MAX_CHARS", 1200), patch.object(
            context_manager,
            "CONTEXT_CHAT_MAX_CHARS",
            500,
        ), patch.object(
            context_manager,
            "CONTEXT_MEMORY_MAX_CHARS",
            400,
        ), patch.object(
            context_manager,
            "CONTEXT_CALL_NOTES_MAX_CHARS",
            200,
        ):
            bundle = context_manager.build_reply_context(messages, memories, "通话纪要" * 200)
        self.assertLessEqual(len(bundle.text), 1200)
        self.assertIn("最近对话", bundle.text)
        self.assertIn("相关记忆", bundle.text)
        self.assertIn("重要通话纪要上下文", bundle.text)
        self.assertLess(bundle.stats["chat_messages"], len(messages))
        self.assertLess(bundle.stats["memories"], len(memories))

    def test_call_notes_context_uses_summary_cache_shape(self):
        with patch.dict(
            "os.environ",
            {
                "CALL_NOTES_ENABLED": "true",
                "FEISHU_MINUTE_TOKENS": "minute_a",
                "CALL_NOTES_MAX_CHARS": "500",
            },
        ), patch.object(call_notes, "_get_tenant_token", return_value="token"), patch.object(
            call_notes,
            "fetch_minute_info",
            return_value={"title": "电话", "create_time": "1782806400000"},
        ), patch.object(
            call_notes,
            "fetch_minute_transcript",
            return_value="舒舒说记得晚上来电话。\n这是一段很长但不该原文全塞的纪要。",
        ), patch.object(
            call_notes,
            "_load_cache",
            return_value={},
        ), patch.object(
            call_notes,
            "_save_cache",
        ), patch.object(
            call_notes,
            "_summarize_transcript_with_deepseek",
            return_value="对方在意晚上能不能好好通电话；用户要记得主动找她。",
        ):
            context = call_notes.build_call_notes_context()
        self.assertIn("通话摘要", context)
        self.assertIn("主动找她", context)
        self.assertNotIn("这是一段很长", context)
        assert_public_text_clean(context)

    def test_external_search_card_uses_table(self):
        card = external_search.build_search_card(
            "近期B站新番",
            [
                {
                    "title": "七月新番导视",
                    "snippet": "多部作品讨论度较高，建议以官方版权页为准。",
                    "url": "https://search.bilibili.com/all?keyword=%E6%96%B0%E7%95%AA",
                }
            ],
            intro="小弟搜到这些近期新番线索，先给舒舒列成表。",
        )
        elements = card["card"]["body"]["elements"]
        self.assertEqual(elements[1]["tag"], "table")
        self.assertEqual(elements[1]["columns"][0]["display_name"], "推荐")
        assert_public_text_clean(card)

    def test_deerflow_search_wraps_agent_answer_as_result(self):
        class Proc:
            returncode = 0
            stdout = json.dumps({
                "answer": "结论：近期可以先看官方专题。来源：https://example.com/anime"
            }, ensure_ascii=False)
            stderr = ""

        with patch.object(external_search, "DEERFLOW_BACKEND_DIR", external_search.Path(".")), patch.object(
            external_search,
            "DEERFLOW_PYTHON",
            sys.executable,
        ), patch.object(external_search.subprocess, "run", return_value=Proc()):
            results = external_search.search_deerflow("近期B站新番", limit=3)
        self.assertEqual(results[0]["provider"], "deerflow")
        self.assertIn("DeerFlow 本地调研", results[0]["title"])
        self.assertEqual(results[1]["url"], "https://example.com/anime")

    def test_search_web_can_fallback_from_deerflow_to_openclaw(self):
        with patch.object(external_search, "EXTERNAL_SEARCH_BACKEND", "auto"), patch.object(
            external_search,
            "search_deerflow",
            side_effect=RuntimeError("down"),
        ), patch.object(
            external_search,
            "search_openclaw",
            return_value=[{"title": "fallback", "snippet": "ok", "url": ""}],
        ):
            results = external_search.search_web("查一下")
        self.assertEqual(results[0]["title"], "fallback")

    def test_presence_summary_uses_probability_language(self):
        with patch.object(local_apps, "is_screen_locked", return_value=False), patch.object(
            local_apps,
            "get_idle_seconds",
            return_value=20,
        ):
            self.assertIn("大概率在电脑前", local_apps.get_presence_summary())
        with patch.object(local_apps, "is_screen_locked", return_value=True), patch.object(
            local_apps,
            "get_idle_seconds",
            return_value=20,
        ):
            self.assertIn("大概率不在电脑前", local_apps.get_presence_summary())

    def test_passive_assistant_topic_detection_is_conservative(self):
        self.assertFalse(passive_assistant._is_high_signal("哈哈哈哈"))
        self.assertTrue(passive_assistant._is_high_signal("这番最后是BE了嘛？"))
        self.assertEqual(
            passive_assistant._query_for_content("这番最后是BE了嘛？"),
            "这番最后是BE了嘛？ 背景 介绍 推荐",
        )
        self.assertEqual(
            passive_assistant._query_for_content("最近B站有什么新番热门"),
            "近期 B站 热门 新番 推荐",
        )

    def test_passive_state_cooldown_and_hourly_limit(self):
        s = {
            "passive_processed_message_ids": [],
            "passive_topic_timestamps": {},
            "passive_sent_timestamps": [],
        }
        self.assertFalse(state.is_passive_message_processed(s, "m1"))
        with patch.object(state, "save_state"):
            state.mark_passive_topic_sent(s, "topic-a", "m1", now=1000)
        self.assertTrue(state.is_passive_message_processed(s, "m1"))
        self.assertTrue(state.is_passive_topic_in_cooldown(s, "topic-a", 1800, now=1200))
        self.assertFalse(state.is_passive_topic_in_cooldown(s, "topic-a", 1800, now=4000))
        self.assertFalse(state.can_send_passive_now(s, 1, now=1200))

    def test_proactive_topic_respects_quiet_window(self):
        now = proactive_topic.datetime(2026, 7, 2, 12, 0, tzinfo=proactive_topic._SHANGHAI)
        quiet_messages = [{"timestamp": int((now.timestamp() - 1900) * 1000), "content": "刚刚聊完"}]
        active_messages = [{"timestamp": int((now.timestamp() - 100) * 1000), "content": "还在聊"}]
        with patch.object(proactive_topic, "PROACTIVE_TOPIC_QUIET_SECONDS", 1800):
            self.assertTrue(proactive_topic._is_group_quiet(quiet_messages, now))
            self.assertFalse(proactive_topic._is_group_quiet(active_messages, now))

    def test_proactive_topic_mentions_both_people(self):
        with patch.object(proactive_topic, "FEISHU_SANGE_OPEN_ID", "ou_sange"), patch.object(
            proactive_topic,
            "FEISHU_SHUSHU_OPEN_ID",
            "ou_shushu",
        ):
            text = proactive_topic._with_mentions("小弟来开个话题。")
        self.assertIn('<at user_id="ou_sange">用户</at>', text)
        self.assertIn('<at user_id="ou_shushu">对方</at>', text)

    def test_proactive_state_daily_limit(self):
        s = {"proactive_topic_sent_dates": {"2026-07-02": 1}}
        self.assertFalse(state.can_send_proactive_today(s, "2026-07-02", 1))
        self.assertTrue(state.can_send_proactive_today(s, "2026-07-02", 2))

    def test_love_note_markdown_to_docx_blocks(self):
        blocks = love_note.markdown_to_docx_blocks(
            "## 2026-07-01\n\n### 今天的小事\n- 舒舒说下雨像云雾。\n> 想坐你旁边看你敲电脑"
        )
        self.assertEqual(blocks[0]["block_type"], 3)
        self.assertEqual(blocks[1]["block_type"], 4)
        self.assertEqual(blocks[2]["block_type"], 12)
        self.assertEqual(blocks[3]["block_type"], 2)
        assert_public_text_clean(blocks)

    def test_love_note_source_trims_generated_summary_tail(self):
        source = "\n".join([
            "和舒舒的恋爱笔记",
            "今天一起研究飞书。",
            "每日总结 2026-07-01",
            "文档里记录的小事",
            "这段是机器人已经生成过的总结。",
        ])
        trimmed = love_note._trim_document_source(source)
        self.assertIn("今天一起研究飞书", trimmed)
        self.assertNotIn("机器人已经生成过", trimmed)

    def test_love_note_comment_anchor_can_use_middle_sweet_block(self):
        blocks = [
            {"block_id": "root", "page": {"elements": []}},
            {
                "block_id": "a",
                "text": {"elements": [{"text_run": {"content": "第一段普通内容"}}]},
            },
            {
                "block_id": "sweet",
                "text": {"elements": [{"text_run": {"content": "想和舒舒一起出去玩儿，想永远陪在舒舒身边"}}]},
            },
            {"block_id": "image", "image": {"token": "img"}},
            {
                "block_id": "blank",
                "text": {"elements": [{"text_run": {"content": ""}}]},
            },
            {
                "block_id": "b",
                "text": {"elements": [{"text_run": {"content": "最后一段"}}]},
            },
        ]
        with patch.object(love_note, "get_docx_blocks", return_value=blocks), patch.object(
            love_note,
            "_pick_anchor_with_deepseek",
            return_value="",
        ):
            self.assertEqual(love_note.pick_love_note_comment_anchor("doc", "这也太甜了"), "sweet")

    def test_love_note_comment_anchor_accepts_model_middle_choice(self):
        blocks = [
            {
                "block_id": "a",
                "text": {"elements": [{"text_run": {"content": "第一段"}}]},
            },
            {
                "block_id": "middle",
                "text": {"elements": [{"text_run": {"content": "中间这一段最适合评论"}}]},
            },
            {
                "block_id": "b",
                "text": {"elements": [{"text_run": {"content": "最后一段"}}]},
            },
        ]
        with patch.object(love_note, "get_docx_blocks", return_value=blocks), patch.object(
            love_note,
            "_pick_anchor_with_deepseek",
            return_value="middle",
        ):
            self.assertEqual(love_note.pick_love_note_comment_anchor("doc", "短评"), "middle")

    def test_love_note_anchor_candidates_skip_commented_blocks_first(self):
        blocks = [
            {
                "block_id": "commented",
                "comment_ids": ["c1"],
                "text": {"elements": [{"text_run": {"content": "已经评论过"}}]},
            },
            {
                "block_id": "fresh",
                "text": {"elements": [{"text_run": {"content": "还没评论"}}]},
            },
        ]
        self.assertEqual(
            love_note._comment_anchor_candidates(blocks),
            [{"block_id": "fresh", "text": "还没评论"}],
        )

    def test_love_note_anchor_candidates_fallback_when_all_commented(self):
        blocks = [
            {
                "block_id": "commented",
                "comment_ids": ["c1"],
                "text": {"elements": [{"text_run": {"content": "已经评论过"}}]},
            },
        ]
        self.assertEqual(
            love_note._comment_anchor_candidates(blocks),
            [{"block_id": "commented", "text": "已经评论过"}],
        )

    def test_love_note_comment_elements_escape_and_chunk_text(self):
        elements = love_note._comment_text_elements("<" + "a" * 1200 + ">")
        self.assertGreater(len(elements), 1)
        self.assertTrue(all(item["type"] == "text" for item in elements))
        self.assertTrue(all(len(item["text"]) <= 900 for item in elements))
        joined = "".join(item["text"] for item in elements)
        self.assertIn("&lt;", joined)
        self.assertIn("&gt;", joined)

    def test_love_note_prompt_is_reaction_not_structured_summary(self):
        source = love_note.generate_love_note_reaction.__code__.co_consts
        prompt_text = "\n".join(str(item) for item in source if isinstance(item, str))
        self.assertIn("嗑糖短评", prompt_text)
        self.assertIn("不要标题、不要分节、不要列表", prompt_text)
        self.assertIn("不要出现“每日总结”", prompt_text)

    def test_love_note_rejects_summary_template_comments(self):
        self.assertFalse(love_note._is_acceptable_reaction("## 每日总结\n### 三哥该记得"))
        self.assertTrue(love_note._is_acceptable_reaction("这段也太甜了，隔着屏幕都能看见两个人的小心思。"))

    def test_love_note_first_run_sets_baseline_without_commenting(self):
        state_obj = {}
        blocks = [
            {"block_id": "a", "text": {"elements": [{"text_run": {"content": "旧内容"}}]}},
        ]
        with patch.object(love_note, "load_state", return_value=state_obj), patch.object(
            love_note,
            "save_state",
        ) as save_mock, patch.object(
            love_note,
            "get_docx_document",
            return_value={"revision_id": 10},
        ), patch.object(
            love_note,
            "get_docx_blocks",
            return_value=blocks,
        ), patch.object(
            love_note,
            "create_docx_comment",
        ) as create_mock, patch.object(
            love_note,
            "LOVE_NOTE_DOC_TOKEN",
            "doc",
        ):
            result = love_note.run_daily_love_note()
        self.assertIn("已建立恋爱笔记增量基线", result)
        self.assertEqual(state_obj["love_note_seen_block_ids"], ["a"])
        self.assertEqual(state_obj["love_note_last_revision_id"], 10)
        create_mock.assert_not_called()
        save_mock.assert_called_once()

    def test_love_note_no_new_blocks_does_not_comment(self):
        state_obj = {"love_note_seen_block_ids": ["a"], "love_note_daily_comment_counts": {}}
        blocks = [
            {"block_id": "a", "text": {"elements": [{"text_run": {"content": "旧内容"}}]}},
        ]
        with patch.object(love_note, "load_state", return_value=state_obj), patch.object(
            love_note,
            "save_state",
        ), patch.object(
            love_note,
            "get_docx_document",
            return_value={"revision_id": 11},
        ), patch.object(
            love_note,
            "get_docx_blocks",
            return_value=blocks,
        ), patch.object(
            love_note,
            "create_docx_comment",
        ) as create_mock, patch.object(
            love_note,
            "LOVE_NOTE_DOC_TOKEN",
            "doc",
        ):
            result = love_note.run_daily_love_note()
        self.assertIn("没有新增正文", result)
        create_mock.assert_not_called()

    def test_love_note_daily_limit_is_two(self):
        state_obj = {"love_note_seen_block_ids": ["old"], "love_note_daily_comment_counts": {}}
        blocks = [
            {"block_id": "old", "text": {"elements": [{"text_run": {"content": "旧内容"}}]}},
            {"block_id": "n1", "text": {"elements": [{"text_run": {"content": "新内容一"}}]}},
            {"block_id": "n2", "text": {"elements": [{"text_run": {"content": "新内容二"}}]}},
            {"block_id": "n3", "text": {"elements": [{"text_run": {"content": "新内容三"}}]}},
        ]
        reactions = [
            {"block_id": "n1", "comment": "第一条甜甜的短评"},
            {"block_id": "n2", "comment": "第二条甜甜的短评"},
            {"block_id": "n3", "comment": "第三条不应该发送"},
        ]
        with patch.object(love_note, "load_state", return_value=state_obj), patch.object(
            love_note,
            "save_state",
        ), patch.object(
            love_note,
            "get_docx_document",
            return_value={"revision_id": 12},
        ), patch.object(
            love_note,
            "get_docx_blocks",
            return_value=blocks,
        ), patch.object(
            love_note,
            "generate_love_note_reactions",
            return_value=reactions,
        ), patch.object(
            love_note,
            "create_docx_comment",
            return_value={"data": {"comment_id": "c", "reply_id": "r"}},
        ) as create_mock, patch.object(
            love_note,
            "LOVE_NOTE_DOC_TOKEN",
            "doc",
        ):
            result = love_note.run_daily_love_note(target_date=love_note.datetime(2026, 7, 2, tzinfo=love_note._SHANGHAI))
        self.assertIn("第一条", result)
        self.assertEqual(create_mock.call_count, 2)
        self.assertEqual(state_obj["love_note_daily_comment_counts"]["2026-07-02"], 2)

    def test_hide_love_note_comment_falls_back_to_solved(self):
        with patch.object(love_note, "_delete_comment_reply", return_value={"ok": False}), patch.object(
            love_note,
            "_mark_comment_solved",
            return_value={"ok": True},
        ) as solved_mock, patch.object(
            love_note,
            "LOVE_NOTE_DOC_TOKEN",
            "doc",
        ):
            self.assertTrue(love_note.hide_love_note_comment("c1", "r1").get("ok"))
        solved_mock.assert_called_once_with("doc", "c1")


if __name__ == "__main__":
    unittest.main()
