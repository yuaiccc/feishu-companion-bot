import unittest

import actions_runner
import notifier
from main import _classify_tool_intent
from text_safety import assert_public_text_clean, sanitize_card, sanitize_public_text


class BotRegressionTests(unittest.TestCase):
    def test_public_text_sanitizer_replaces_disallowed_nickname(self):
        self.assertEqual(sanitize_public_text("\u5fae\u91cc宝贝"), "舒舒宝贝")

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
        self.assertEqual(_classify_tool_intent("想你了", "舒舒"), "none")


if __name__ == "__main__":
    unittest.main()
