package rageval

import (
	"context"
	"strings"
	"time"

	"feishu-companion-bot/internal/memory"
)

type Case struct {
	ID              string   `json:"id"`
	Query           string   `json:"query"`
	Audience        string   `json:"audience"`
	TopK            int      `json:"top_k"`
	ExpectedAny     []string `json:"expected_any"`
	Forbidden       []string `json:"forbidden"`
	ExpectedSources []string `json:"expected_sources"`
	IncludeChat     bool     `json:"include_chat_archive"`
	IncludeMedia    bool     `json:"include_media_archive"`
}

type CaseResult struct {
	ID               string   `json:"id"`
	Passed           bool     `json:"passed"`
	HitRank          int      `json:"hit_rank"`
	SourceHit        bool     `json:"source_hit"`
	PrivacyViolation bool     `json:"privacy_violation"`
	LatencyMS        int64    `json:"latency_ms"`
	Results          []string `json:"results"`
}

type Report struct {
	Cases             int          `json:"cases"`
	PassRate          float64      `json:"pass_rate"`
	HitRate           float64      `json:"hit_rate"`
	MRR               float64      `json:"mrr"`
	SourceHitRate     float64      `json:"source_hit_rate"`
	PrivacyViolations int          `json:"privacy_violations"`
	LatencyP50MS      int64        `json:"latency_p50_ms"`
	LatencyP95MS      int64        `json:"latency_p95_ms"`
	Results           []CaseResult `json:"results"`
}

type Retriever interface {
	SearchRelevantWithOptions(query, audience string, opts memory.RetrievalOptions) []memory.RetrievedMemory
}

func Run(_ context.Context, retriever Retriever, cases []Case) Report {
	report := Report{Cases: len(cases)}
	var hits, passes, sourceHits int
	var reciprocal float64
	latencies := make([]int64, 0, len(cases))
	for _, test := range cases {
		if test.Audience == "" {
			test.Audience = "owner"
		}
		if test.TopK <= 0 {
			test.TopK = 8
		}
		started := time.Now()
		items := retriever.SearchRelevantWithOptions(test.Query, test.Audience, memory.RetrievalOptions{
			TopK: test.TopK, IncludeBotMemory: true,
			IncludeChatArchive: test.IncludeChat, IncludeMediaArchive: test.IncludeMedia,
		})
		result := CaseResult{ID: test.ID, LatencyMS: time.Since(started).Milliseconds()}
		latencies = append(latencies, result.LatencyMS)
		for i, item := range items {
			result.Results = append(result.Results, item.PromptText())
			if result.HitRank == 0 && containsAny(item.Text, test.ExpectedAny) {
				result.HitRank = i + 1
			}
			if containsAny(item.Text, test.Forbidden) {
				result.PrivacyViolation = true
			}
			if containsAny(item.SourceType, test.ExpectedSources) {
				result.SourceHit = true
			}
		}
		if len(test.ExpectedAny) == 0 {
			result.HitRank = 1
		}
		if len(test.ExpectedSources) == 0 {
			result.SourceHit = true
		}
		if result.HitRank > 0 {
			hits++
			reciprocal += 1 / float64(result.HitRank)
		}
		if result.SourceHit {
			sourceHits++
		}
		if result.PrivacyViolation {
			report.PrivacyViolations++
		}
		result.Passed = result.HitRank > 0 && result.SourceHit && !result.PrivacyViolation
		if result.Passed {
			passes++
		}
		report.Results = append(report.Results, result)
	}
	if len(cases) > 0 {
		n := float64(len(cases))
		report.PassRate = float64(passes) / n
		report.HitRate = float64(hits) / n
		report.MRR = reciprocal / n
		report.SourceHitRate = float64(sourceHits) / n
		report.LatencyP50MS = percentile(latencies, 0.50)
		report.LatencyP95MS = percentile(latencies, 0.95)
	}
	return report
}

func containsAny(value string, terms []string) bool {
	value = strings.ToLower(value)
	for _, term := range terms {
		if term = strings.ToLower(strings.TrimSpace(term)); term != "" && strings.Contains(value, term) {
			return true
		}
	}
	return false
}

func percentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := append([]int64(nil), values...)
	for i := 1; i < len(copyValues); i++ {
		for j := i; j > 0 && copyValues[j] < copyValues[j-1]; j-- {
			copyValues[j], copyValues[j-1] = copyValues[j-1], copyValues[j]
		}
	}
	index := int(float64(len(copyValues)-1)*p + 0.5)
	return copyValues[index]
}
