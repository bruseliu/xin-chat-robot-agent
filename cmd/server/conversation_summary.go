package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"

	"xin-chat-robot-agent/internal/config"
	"xin-chat-robot-agent/internal/conversation"
	"xin-chat-robot-agent/internal/conversation/compact"
	llmpkg "xin-chat-robot-agent/internal/llm"
)

func startConversationCompaction(ctx context.Context, cfg config.LLMConfig, store conversation.CompactionStore, logger *slog.Logger) (conversation.CompactionTrigger, error) {
	model, err := newSummaryModel(ctx, cfg)
	if err != nil || model == nil {
		return nil, err
	}
	worker, err := compact.New(store, compact.ADKSummarizer{Model: model}, compact.Config{}, logger)
	if err != nil {
		return nil, err
	}
	go worker.Run(ctx)
	return worker, nil
}

func newSummaryModel(ctx context.Context, cfg config.LLMConfig) (model.LLM, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "none":
		return nil, nil
	case "gemini":
		if strings.TrimSpace(cfg.GeminiAPIKey) == "" {
			return nil, fmt.Errorf("Gemini API key is required for conversation summaries")
		}
		return gemini.NewModel(ctx, cfg.GeminiModel, &genai.ClientConfig{APIKey: cfg.GeminiAPIKey})
	case "doubao":
		return llmpkg.NewModel(ctx, llmpkg.Config{Provider: "doubao", Doubao: llmpkg.DoubaoConfig{BaseURL: cfg.DoubaoBaseURL, APIKey: cfg.DoubaoAPIKey, Model: cfg.DoubaoModel, Timeout: cfg.DoubaoTimeout, Thinking: "disabled"}})
	default:
		return nil, fmt.Errorf("unsupported summary LLM provider %q", cfg.Provider)
	}
}
