package main

import (
	"context"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"

	"xin-chat-robot-agent/internal/config"
	llmpkg "xin-chat-robot-agent/internal/llm"
)

func newLLMAgent(ctx context.Context, cfg config.LLMConfig, agentID string) (adkagent.Agent, error) {
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
		return newADKLLMAgent(agentID, model, cfg.Instruction)
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
		return newADKLLMAgent(agentID, model, cfg.Instruction)
	default:
		return nil, fmt.Errorf("unsupported LLM_PROVIDER %q", cfg.Provider)
	}
}

func newADKLLMAgent(agentID string, llm model.LLM, instruction string) (adkagent.Agent, error) {
	name := strings.TrimSpace(agentID)
	if name == "" {
		name = "voice-agent"
	}
	agent, err := llmagent.New(llmagent.Config{
		Name:        name,
		Model:       llm,
		Description: "Realtime voice assistant reasoning agent.",
		Instruction: instruction,
	})
	if err != nil {
		return nil, fmt.Errorf("create ADK LLM agent: %w", err)
	}
	return agent, nil
}
