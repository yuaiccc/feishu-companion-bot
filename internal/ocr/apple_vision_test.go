package ocr

import (
	"encoding/json"
	"testing"
)

func TestResultJSONContract(t *testing.T) {
	raw := `{"text":"你好\nworld","elapsed_ms":12,"observations":[{"text":"你好","confidence":0.9,"x":0.1,"y":0.2,"width":0.3,"height":0.4}]}`
	var result Result
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Text != "你好\nworld" || result.ElapsedMS != 12 || len(result.Observations) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
}
