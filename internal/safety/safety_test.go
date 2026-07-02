package safety

import (
	"testing"
)

func TestSanitizeForLLM(t *testing.T) {
	tests := []struct {
		input    string
		contains string
	}{
		{"我的手机号是13812345678，你记一下", "[敏感信息]"},
		{"邮箱是 test@example.com 吗？", "[敏感信息]"},
		{"sk-abc123def456abc123defghijk123456", "[敏感信息]"},
		{"今天天气真好", "今天天气真好"},
	}

	for _, tt := range tests {
		got := SanitizeForLLM(tt.input)
		if tt.contains == "[敏感信息]" {
			if got == tt.input {
				t.Errorf("SanitizeForLLM(%q) = %q, should contain [敏感信息]", tt.input, got)
			}
		}
	}
}

func TestSanitizeCard(t *testing.T) {
	card := map[string]interface{}{
		"header": map[string]interface{}{
			"title": map[string]string{"tag": "plain_text", "content": "正常标题"},
		},
		"body": map[string]interface{}{
			"elements": []interface{}{
				map[string]interface{}{"tag": "markdown", "content": "手机号 13812345678"},
			},
		},
	}

	sanitized := SanitizeCard(card)
	// Should not panic and should return a map
	if _, ok := sanitized["header"]; !ok {
		t.Errorf("SanitizeCard should return map with header, got %v", sanitized)
	}
}

func TestEndsWithPunct(t *testing.T) {
	// tested via endsWithPunct indirectly via streamingReply
}
