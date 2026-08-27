package memory

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMem0ClientAddSendsFactExtractionPrompt(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/memories" {
			t.Fatalf("path = %q, want /memories", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results": []}`))
	}))
	defer server.Close()

	client := NewMem0Client(Mem0Config{BaseURL: server.URL}, slog.Default())
	err := client.Add(context.Background(), AddRequest{
		UserID:   "user-1",
		Messages: []Message{{Role: "user", Content: "我喜欢蜗牛"}},
		Prompt:   EnglishFactExtractionPrompt,
	})
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if got, _ := payload["prompt"].(string); got != EnglishFactExtractionPrompt {
		t.Fatalf("prompt = %q, want %q", got, EnglishFactExtractionPrompt)
	}
}
