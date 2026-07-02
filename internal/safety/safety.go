package safety

import (
	"regexp"
	"strings"
)

var (
	// Patterns that should never be exposed in prompts
	privatePatterns = []*regexp.Regexp{
		regexp.MustCompile(`手机号[码号]\d+`),
		regexp.MustCompile(`\d{11}`),
		regexp.MustCompile(`\d{3,4}[-\s]?\d{3,4}[-\s]?\d{3,4}`),
		regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`),
		regexp.MustCompile(`oc\.[a-zA-Z0-9]+`),
		regexp.MustCompile(`ou_[a-zA-Z0-9]+`),
		regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`),
	}
)

func SanitizeForLLM(text string) string {
	if text == "" {
		return ""
	}
	result := text
	for _, pattern := range privatePatterns {
		result = pattern.ReplaceAllString(result, "[敏感信息]")
	}
	return result
}

func SanitizePublicText(text string) string {
	result := SanitizeForLLM(text)
	// Additional public-facing sanitization
	result = strings.TrimSpace(result)
	if len(result) > 2000 {
		result = result[:2000] + "..."
	}
	return result
}

func SanitizeCard(card map[string]interface{}) map[string]interface{} {
	// Recursively sanitize string values in card
	result := sanitizeCardRecursive(card, 0)
	if m, ok := result.(map[string]interface{}); ok {
		return m
	}
	return card
}

func sanitizeCardRecursive(v interface{}, depth int) interface{} {
	if depth > 10 {
		return v
	}
	switch val := v.(type) {
	case string:
		return SanitizePublicText(val)
	case map[string]interface{}:
		result := make(map[string]interface{}, len(val))
		for k, v := range val {
			result[k] = sanitizeCardRecursive(v, depth+1)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, v := range val {
			result[i] = sanitizeCardRecursive(v, depth+1)
		}
		return result
	default:
		return val
	}
}
