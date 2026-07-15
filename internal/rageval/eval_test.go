package rageval

import (
	"context"
	"testing"

	"feishu-companion-bot/internal/memory"
)

type fakeRetriever struct{ items []memory.RetrievedMemory }

func (f fakeRetriever) SearchRelevantWithOptions(string, string, memory.RetrievalOptions) []memory.RetrievedMemory {
	return f.items
}

func TestRunComputesRankAndPrivacy(t *testing.T) {
	report := Run(context.Background(), fakeRetriever{[]memory.RetrievedMemory{{Text: "普通内容"}, {Text: "喜欢无糖咖啡", SourceType: "bot_memory"}}}, []Case{{
		ID: "preference", Query: "饮品", ExpectedAny: []string{"无糖"}, ExpectedSources: []string{"bot_memory"}, Forbidden: []string{"手机号"},
	}})
	if report.MRR != 0.5 || report.PassRate != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
}
