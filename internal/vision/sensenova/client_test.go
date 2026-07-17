package sensenova

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDescribeImage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("missing authorization")
		}
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if request.Model != "test-model" || len(request.Messages) != 1 || len(request.Messages[0].Content) != 2 {
			t.Fatalf("unexpected request: %+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"choices":[{"message":{"content":"这是一张测试图片。"}}]}}`))
	}))
	defer server.Close()

	client := NewClient("test-token", server.URL, "test-model", time.Second)
	got, err := client.DescribeImage(context.Background(), []byte("image"))
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if got != "这是一张测试图片。" {
		t.Fatalf("got %q", got)
	}
}

func TestDescribeImageRequiresTokenAndModel(t *testing.T) {
	for name, client := range map[string]*Client{
		"token": NewClient("", "", "model", time.Second),
		"model": NewClient("token", "", "", time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.DescribeImage(context.Background(), []byte("image"))
			if err == nil || !strings.Contains(err.Error(), "not configured") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
