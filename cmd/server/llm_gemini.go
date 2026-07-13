package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"

	"xin-chat-robot-agent/internal/config"
	llmpkg "xin-chat-robot-agent/internal/llm"
	"xin-chat-robot-agent/internal/memory"
)

func newLLMAgent(ctx context.Context, cfg config.LLMConfig, agentID string, memStore memory.Store, logger *slog.Logger) (adkagent.Agent, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "none":
		return nil, nil
	case "gemini":
		if strings.TrimSpace(cfg.GeminiAPIKey) == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY or GOOGLE_API_KEY is required when LLM_PROVIDER=gemini")
		}
		modelName := strings.TrimSpace(cfg.GeminiModel)
		if modelName == "" {
			modelName = "gemini-flash-latest"
		}
		model, err := gemini.NewModel(ctx, modelName, &genai.ClientConfig{
			APIKey: cfg.GeminiAPIKey,
		})
		if err != nil {
			return nil, fmt.Errorf("create Gemini model: %w", err)
		}
		return newADKLLMAgent(agentID, model, cfg.Instruction, memStore, logger)
	case "doubao":
		model, err := llmpkg.NewModel(ctx, llmpkg.Config{
			Provider: "doubao",
			Doubao: llmpkg.DoubaoConfig{
				BaseURL: cfg.DoubaoBaseURL,
				APIKey:  cfg.DoubaoAPIKey,
				Model:   cfg.DoubaoModel,
				Timeout: cfg.DoubaoTimeout,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("create Doubao LLM model: %w", err)
		}
		return newADKLLMAgent(agentID, model, cfg.Instruction, memStore, logger)
	default:
		return nil, fmt.Errorf("unsupported LLM_PROVIDER %q", cfg.Provider)
	}
}

func newADKLLMAgent(agentID string, llm model.LLM, instruction string, memStore memory.Store, logger *slog.Logger) (adkagent.Agent, error) {
	name := strings.TrimSpace(agentID)
	if name == "" {
		name = "voice-agent"
	}
	var callbacks []llmagent.BeforeModelCallback
	if memStore != nil {
		callbacks = append(callbacks, memoryInjectionCallback(memStore, logger))
	}
	agent, err := llmagent.New(llmagent.Config{
		Name:                 name,
		Model:                llm,
		Description:          "Realtime voice assistant reasoning agent.",
		Instruction:          instruction,
		BeforeModelCallbacks: callbacks,
	})
	if err != nil {
		return nil, fmt.Errorf("create ADK LLM agent: %w", err)
	}
	return agent, nil
}

func memoryInjectionCallback(store memory.Store, logger *slog.Logger) llmagent.BeforeModelCallback {
	return func(ctx adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		query := lastUserText(req.Contents)
		if utf8.RuneCountInString(query) < 4 {
			return nil, nil
		}
		userID := ctx.UserID()
		if userID == "" {
			return nil, nil
		}
		memories, err := store.Search(ctx, memory.SearchRequest{
			UserID: userID,
			Query:  query,
		})
		if err != nil {
			logger.Warn("memory injection search failed", "error", err, "user_id", userID)
			return nil, nil
		}
		if len(memories) == 0 {
			return nil, nil
		}
		var sb strings.Builder
		sb.WriteString("[记忆上下文]\n")
		for _, m := range memories {
			if m.Text != "" {
				sb.WriteString("- ")
				sb.WriteString(m.Text)
				sb.WriteString("\n")
			}
		}
		memContent := genai.NewContentFromText(sb.String(), genai.RoleUser)
		memContent.Role = "user"
		req.Contents = append([]*genai.Content{memContent}, req.Contents...)
		logger.Info("memory injected", "user_id", userID, "count", len(memories))
		return nil, nil
	}
}

func lastUserText(contents []*genai.Content) string {
	for i := len(contents) - 1; i >= 0; i-- {
		c := contents[i]
		if c.Role != genai.RoleUser {
			continue
		}
		for _, part := range c.Parts {
			if part != nil && part.Text != "" {
				return part.Text
			}
		}
	}
	return ""
}
