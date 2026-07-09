package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestDoubaoLLMGenerateContentOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var req doubaoChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request error = %v", err)
		}
		if req.Model != "ep-test" || req.Stream {
			t.Fatalf("request model/stream = %q/%v", req.Model, req.Stream)
		}
		_ = json.NewEncoder(w).Encode(doubaoChatResponse{Model: "ep-test", Choices: []doubaoChoice{{Message: doubaoMessage{Role: "assistant", Content: "你好"}}}})
	}))
	defer server.Close()

	llm, err := NewDoubaoLLM(context.Background(), DoubaoConfig{BaseURL: server.URL, APIKey: "test-key", Model: "ep-test"})
	if err != nil {
		t.Fatalf("NewDoubaoLLM() error = %v", err)
	}
	got, err := collectDoubaoResponses(llm.GenerateContent(context.Background(), &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}, false))
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if len(got) != 1 || contentText(got[0].Content) != "你好" || got[0].Partial {
		t.Fatalf("responses = %+v", got)
	}
}

func TestDoubaoLLMGenerateContentStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req doubaoChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("Decode request error = %v", err)
		}
		if !req.Stream {
			t.Fatal("Stream = false, want true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"ep-test\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"model\":\"ep-test\",\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	llm, err := NewDoubaoLLM(context.Background(), DoubaoConfig{BaseURL: server.URL, APIKey: "test-key", Model: "ep-test"})
	if err != nil {
		t.Fatalf("NewDoubaoLLM() error = %v", err)
	}
	got, err := collectDoubaoResponses(llm.GenerateContent(context.Background(), &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}, true))
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("response count = %d, want 3", len(got))
	}
	if contentText(got[0].Content) != "你" || !got[0].Partial {
		t.Fatalf("first response = %+v", got[0])
	}
	if contentText(got[1].Content) != "好" || !got[1].Partial {
		t.Fatalf("second response = %+v", got[1])
	}
	if contentText(got[2].Content) != "你好" || got[2].Partial {
		t.Fatalf("final response = %+v", got[2])
	}
}

func collectDoubaoResponses(seq func(func(*model.LLMResponse, error) bool)) ([]*model.LLMResponse, error) {
	var out []*model.LLMResponse
	var firstErr error
	for resp, err := range seq {
		if err != nil {
			firstErr = err
			break
		}
		out = append(out, resp)
	}
	return out, firstErr
}
