package context

import "testing"

func TestBudgetCountsUnicodeCharacters(t *testing.T) {
	b := NewBudget(4)
	if !b.Reserve("cn", "舒舒你好") {
		t.Fatal("four Chinese characters should fit a four-character budget")
	}
	if b.Used != 4 {
		t.Fatalf("used=%d, want 4", b.Used)
	}
}
