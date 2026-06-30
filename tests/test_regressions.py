import unittest
from unittest.mock import patch

import actions_runner
import call_notes
import external_search
import notifier
import summarizer
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
                "repo": "yuaiccc/project-history",
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
                "repo": {"name": "yuaiccc/project-history"},
                "created_at": "2026-06-30T08:24:00Z",
                "payload": {"commits": [{"message": "Remove activity summary"}]},
            }
        ]
        elements = actions_runner.build_commit_card(activities)["card"]["body"]["elements"]
        self.assertEqual(elements[0]["tag"], "table")
        assert_public_text_clean(elements)

    def test_tool_intent_separates_status_from_github(self):
        self.assertEqual(_classify_tool_intent("三哥最近活动", "舒舒"), "status")
        self.assertEqual(_classify_tool_intent("三哥最近提交了什么", "舒舒"), "github")
        self.assertEqual(_classify_tool_intent("最近B站哪些新番热门", "舒舒"), "search")
        self.assertEqual(_classify_tool_intent("帮我查一下最近新闻", "舒舒"), "search")
        self.assertEqual(_classify_tool_intent("想你了", "舒舒"), "none")

    def test_persona_is_helper_not_sange_persona(self):
        prompts = "\n".join([
            summarizer.RELATIONSHIP_CONTEXT,
            summarizer.SYSTEM_PROMPT,
            summarizer.REPLY_PROMPT_SHUSHU,
            external_search.summarize_search_results.__doc__ or "",
        ])
        self.assertIn("三哥的小弟", prompts)
        self.assertIn("大哥的老婆", prompts)
        self.assertNotIn("你是秋酿本人", prompts)
        self.assertNotIn("用第一人称跟舒舒说话", prompts)

    def test_aliases_are_one_person_not_parallel_names(self):
        prompts = "\n".join([
            summarizer.RELATIONSHIP_CONTEXT,
            summarizer.SYSTEM_PROMPT,
            summarizer.REPLY_PROMPT_SHUSHU,
        ])
        self.assertIn("同一个人", prompts)
        self.assertIn("不要把两个名字并列说出来", prompts)
        self.assertNotIn("只用舒舒或烨子", prompts)

    def test_call_notes_fallback_extracts_relationship_context(self):
        transcript = "\n".join([
            "随便聊一点普通事情",
            "舒舒说晚上记得早点来打电话，想听你说晚安",
            "秋酿答应散步回来就找她",
        ])
        summary = call_notes._fallback_summarize_transcript(transcript)
        self.assertIn("记得早点来打电话", summary)
        self.assertIn("散步回来就找她", summary)
        assert_public_text_clean(summary)

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
            return_value="舒舒在意晚上能不能好好通电话；秋酿要记得主动找她。",
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


if __name__ == "__main__":
    unittest.main()
