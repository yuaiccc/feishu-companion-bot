package state

import (
	"testing"
)

// newStateForTest builds an in-memory state with no flush path, so MarkSent
// exercises only the map logic without touching disk.
func newStateForTest() *State {
	return &State{SentEvents: make(map[string]bool)}
}

func TestMarkSentAndHasSent(t *testing.T) {
	s := newStateForTest()
	if s.HasSent("a") {
		t.Fatal("未标记的事件应返回 false")
	}
	if err := s.MarkSent("a"); err != nil {
		t.Fatalf("MarkSent 失败: %v", err)
	}
	if !s.HasSent("a") {
		t.Fatal("已标记的事件应返回 true")
	}
	// 重复标记不应报错
	if err := s.MarkSent("a"); err != nil {
		t.Fatalf("重复 MarkSent 失败: %v", err)
	}
}

func TestFilterNew(t *testing.T) {
	s := newStateForTest()
	_ = s.MarkSent("a")
	_ = s.MarkSent("b")

	got := s.FilterNew([]string{"a", "b", "c", "d"})
	if len(got) != 2 {
		t.Fatalf("FilterNew 返回 %d 条，期望 2: %v", len(got), got)
	}
	// 顺序保留输入顺序中未标记的部分
	for _, id := range got {
		if id != "c" && id != "d" {
			t.Fatalf("FilterNew 返回意外 ID: %s", id)
		}
	}
}

func TestFilterNewAllSeen(t *testing.T) {
	s := newStateForTest()
	_ = s.MarkSent("a")
	_ = s.MarkSent("b")
	if got := s.FilterNew([]string{"a", "b"}); len(got) != 0 {
		t.Fatalf("全部已标记时 FilterNew 应返回空，得到 %v", got)
	}
}

func TestFilterNewEmptyInput(t *testing.T) {
	s := newStateForTest()
	if got := s.FilterNew(nil); len(got) != 0 {
		t.Fatalf("空输入应返回空，得到 %v", got)
	}
}

func TestLoadEmptyState(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir, "testprofile")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if s.HasSent("anything") {
		t.Fatal("空 state 不应有已标记事件")
	}
	if s.SentEvents == nil {
		t.Fatal("SentEvents 应已初始化为非 nil map")
	}
}

func TestLoadPersistsAcrossLoads(t *testing.T) {
	dir := t.TempDir()
	s1, err := Load(dir, "testprofile")
	if err != nil {
		t.Fatalf("首次 Load 失败: %v", err)
	}
	if err := s1.MarkSent("event1"); err != nil {
		t.Fatalf("MarkSent 失败: %v", err)
	}

	// 重新 Load 同一 profile，应读到已标记事件
	s2, err := Load(dir, "testprofile")
	if err != nil {
		t.Fatalf("二次 Load 失败: %v", err)
	}
	if !s2.HasSent("event1") {
		t.Fatal("持久化失败：重新 Load 后 event1 应已标记")
	}
}

func TestLoadDifferentProfilesIsolated(t *testing.T) {
	dir := t.TempDir()
	s1, _ := Load(dir, "profileA")
	if err := s1.MarkSent("shared"); err != nil {
		t.Fatalf("MarkSent 失败: %v", err)
	}
	s2, _ := Load(dir, "profileB")
	if s2.HasSent("shared") {
		t.Fatal("不同 profile 的 state 应隔离，shared 不应在 profileB 出现")
	}
}
