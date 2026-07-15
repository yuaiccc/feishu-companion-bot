package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing or wrong Authorization header")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello back"}}]}`))
	}))
	defer server.Close()

	client := NewClient("test-key", server.URL, "test-model")
	msgs := []Message{{Role: "user", Content: "hello"}}
	reply, err := client.Chat(context.Background(), msgs)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply != "hello back" {
		t.Errorf("reply = %q, want %q", reply, "hello back")
	}
}

func TestChatStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// SSE format: "data: {...}\n\n"
		type delta struct {
			Content string `json:"content"`
		}
		chunk1 := StreamChunk{Choices: []StreamChoice{{Delta: delta{Content: "hello"}}}}
		chunk2 := StreamChunk{Choices: []StreamChoice{{Delta: delta{Content: " world"}}}}
		data1, _ := json.Marshal(chunk1)
		data2, _ := json.Marshal(chunk2)
		_, _ = w.Write([]byte("data: " + string(data1) + "\n"))
		_, _ = w.Write([]byte("data: " + string(data2) + "\n"))
		_, _ = w.Write([]byte("data: [DONE]\n"))
	}))
	defer server.Close()

	client := NewClient("test-key", server.URL, "test-model")
	var full strings.Builder
	err := client.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}},
		func(chunk string) { full.WriteString(chunk) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if full.String() != "hello world" {
		t.Errorf("stream = %q, want %q", full.String(), "hello world")
	}
}
