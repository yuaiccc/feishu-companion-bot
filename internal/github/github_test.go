package github

import (
	"testing"
)

func TestDedupEvents(t *testing.T) {
	events := []Event{
		{ID: "1", Type: "PushEvent", Payload: map[string]interface{}{"commits": []interface{}{map[string]interface{}{"sha": "abc"}}}},
		{ID: "2", Type: "PushEvent", Payload: map[string]interface{}{"commits": []interface{}{map[string]interface{}{"sha": "abc"}}}},
		{ID: "1", Type: "StarEvent"},
		{ID: "3", Type: "PushEvent", Payload: map[string]interface{}{"commits": []interface{}{map[string]interface{}{"sha": "def"}}}},
	}

	deduped := DedupEvents(events)
	if len(deduped) != 2 {
		t.Errorf("deduped %d events, want 2", len(deduped))
	}
}

func TestParseActivity(t *testing.T) {
	pushEvent := Event{
		Type:      "PushEvent",
		Repo:      map[string]interface{}{"name": "owner/repo"},
		CreatedAt: "2026-07-02T10:00:00Z",
		Payload: map[string]interface{}{
			"size": 3.0,
			"commits": []interface{}{
				map[string]interface{}{"sha": "abc", "message": "feat: add new feature\n\nDetailed description"},
			},
		},
	}

	activity := ParseActivity(pushEvent)
	if activity.Type != "PushEvent" {
		t.Errorf("Type = %q, want %q", activity.Type, "PushEvent")
	}
	if activity.Repo != "owner/repo" {
		t.Errorf("Repo = %q, want %q", activity.Repo, "owner/repo")
	}
	if activity.Text == "" {
		t.Errorf("Text should not be empty")
	}
}

func TestParsePrivateCommitActivity(t *testing.T) {
	event := Event{
		Type:      "PushEvent",
		Repo:      map[string]interface{}{"name": "owner/private"},
		CreatedAt: "2026-07-02T10:00:00Z",
		Payload: map[string]interface{}{
			"size": float64(1),
			"commits": []interface{}{
				map[string]interface{}{"sha": "abc", "message": "中文化 GitHub 动态"},
			},
		},
	}
	activity := ParseActivity(event)
	if activity.Text == "" {
		t.Fatalf("Text should not be empty")
	}
	if activity.Text == "向 owner/private 推送了代码" {
		t.Fatalf("private commit message was not parsed: %q", activity.Text)
	}
}

func TestSortByCreatedAt(t *testing.T) {
	events := []Event{
		{ID: "1", CreatedAt: "2026-07-01T10:00:00Z"},
		{ID: "2", CreatedAt: "2026-07-02T10:00:00Z"},
		{ID: "3", CreatedAt: "2026-06-30T10:00:00Z"},
	}
	SortByCreatedAt(events)
	if events[0].ID != "2" || events[2].ID != "3" {
		t.Errorf("sort order wrong: %v", events)
	}
}
