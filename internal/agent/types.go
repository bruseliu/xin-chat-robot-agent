package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"xin-chat-robot-agent/internal/memory"

	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type Config struct {
	AgentID        string
	RootAgent      adkagent.Agent
	SessionService adksession.Service
	MemoryService  adkmemory.Service
}

type TurnInput struct {
	UserID    string
	SessionID string
	Text      string
	Memories  []memory.Memory
	Metadata  map[string]string
}

type TurnOutput struct {
	Text string `json:"text"`
}

type StreamEventType string

const (
	StreamEventLLMDelta StreamEventType = "llm_delta"
	StreamEventLLMFinal StreamEventType = "llm_final"
)

type StreamEvent struct {
	Type      StreamEventType `json:"type"`
	TextDelta string          `json:"text_delta,omitempty"`
	Text      string          `json:"text,omitempty"`
	Partial   bool            `json:"partial,omitempty"`
	Final     bool            `json:"final,omitempty"`
	EventID   string          `json:"event_id,omitempty"`
	Author    string          `json:"author,omitempty"`
}

type Runner interface {
	RunTurn(ctx context.Context, input TurnInput) (TurnOutput, error)
	RunTurnStream(ctx context.Context, input TurnInput, yield func(StreamEvent) bool) (TurnOutput, error)
}

type ADKRunner struct {
	cfg       Config
	memory    memory.Store
	logger    *slog.Logger
	planner   Planner
	adkRunner *runner.Runner
}

type Planner interface {
	Plan(ctx context.Context, input TurnInput) (TurnOutput, error)
}

func NewADKRunner(cfg Config, store memory.Store, logger *slog.Logger) (*ADKRunner, error) {
	r := &ADKRunner{
		cfg:     cfg,
		memory:  store,
		logger:  logger,
		planner: echoPlanner{},
	}
	if cfg.RootAgent != nil {
		if cfg.SessionService == nil {
			return nil, fmt.Errorf("ADK session service is required when root agent is configured")
		}
		appName := cfg.AgentID
		if appName == "" {
			appName = "voice-agent"
		}
		adkRunner, err := runner.New(runner.Config{
			AppName:           appName,
			Agent:             cfg.RootAgent,
			SessionService:    cfg.SessionService,
			MemoryService:     cfg.MemoryService,
			AutoCreateSession: true,
		})
		if err != nil {
			return nil, fmt.Errorf("create ADK runner: %w", err)
		}
		r.adkRunner = adkRunner
	}
	return r, nil
}

func (r *ADKRunner) RunTurn(ctx context.Context, input TurnInput) (TurnOutput, error) {
	var finalText string
	var delta strings.Builder
	output, err := r.RunTurnStream(ctx, input, func(event StreamEvent) bool {
		if event.TextDelta != "" {
			delta.WriteString(event.TextDelta)
		}
		if event.Final && event.Text != "" {
			finalText = event.Text
		}
		return true
	})
	if err != nil {
		return TurnOutput{}, err
	}
	if finalText != "" {
		output.Text = finalText
	} else if delta.Len() > 0 {
		output.Text = delta.String()
	}
	return output, nil
}

func (r *ADKRunner) RunTurnStream(ctx context.Context, input TurnInput, yield func(StreamEvent) bool) (TurnOutput, error) {
	if strings.TrimSpace(input.Text) == "" {
		return TurnOutput{}, fmt.Errorf("empty input text")
	}
	r.logger.Info("agent turn", "agent_id", r.cfg.AgentID, "user_id", input.UserID, "memory_count", len(input.Memories), "adk_session_service", r.cfg.SessionService != nil, "adk_memory_service", r.cfg.MemoryService != nil)
	if r.adkRunner == nil {
		output, err := r.planner.Plan(ctx, input)
		if err != nil {
			return TurnOutput{}, err
		}
		if yield != nil {
			yield(StreamEvent{Type: StreamEventLLMFinal, Text: output.Text, Final: true})
		}
		return output, nil
	}

	var delta strings.Builder
	finalText := ""
	content := genai.NewContentFromText(input.Text, genai.RoleUser)
	for event, err := range r.adkRunner.Run(ctx, input.UserID, input.SessionID, content, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeSSE,
	}) {
		if err != nil {
			return TurnOutput{}, err
		}
		if event == nil || event.LLMResponse.Content == nil {
			continue
		}
		text := contentText(event.LLMResponse.Content)
		if text == "" {
			continue
		}
		if event.LLMResponse.Partial {
			delta.WriteString(text)
			if yield != nil && !yield(StreamEvent{
				Type:      StreamEventLLMDelta,
				TextDelta: text,
				Partial:   true,
				EventID:   event.ID,
				Author:    event.Author,
			}) {
				return TurnOutput{Text: delta.String()}, nil
			}
			continue
		}
		if event.IsFinalResponse() {
			finalText = text
			if yield != nil && !yield(StreamEvent{
				Type:    StreamEventLLMFinal,
				Text:    text,
				Final:   true,
				EventID: event.ID,
				Author:  event.Author,
			}) {
				return TurnOutput{Text: text}, nil
			}
		}
	}
	if finalText == "" {
		finalText = delta.String()
	}
	return TurnOutput{Text: finalText}, nil
}

type echoPlanner struct{}

func (echoPlanner) Plan(_ context.Context, input TurnInput) (TurnOutput, error) {
	prefix := "我记住了你的上下文。"
	if len(input.Memories) == 0 {
		prefix = "我还没有检索到相关长期记忆。"
	}
	return TurnOutput{Text: prefix + " 你刚才说：" + input.Text}, nil
}
