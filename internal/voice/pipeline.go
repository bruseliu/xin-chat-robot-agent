package voice

import (
	"context"
	"log/slog"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/memory"
)

type TextTurnInput struct {
	UserID    string            `json:"user_id"`
	SessionID string            `json:"session_id,omitempty"`
	Text      string            `json:"text"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type TextTurnOutput struct {
	Text     string              `json:"text"`
	Events   []agent.StreamEvent `json:"events,omitempty"`
	Memories []memory.Memory     `json:"memories,omitempty"`
}

type Pipeline struct {
	agent  agent.Runner
	memory memory.Store
	logger *slog.Logger
}

func NewPipeline(agentRunner agent.Runner, store memory.Store, logger *slog.Logger) *Pipeline {
	return &Pipeline{
		agent:  agentRunner,
		memory: store,
		logger: logger,
	}
}

func (p *Pipeline) HandleTextTurn(ctx context.Context, input TextTurnInput) (TextTurnOutput, error) {
	return p.HandleTextTurnStream(ctx, input, nil)
}

func (p *Pipeline) HandleTextTurnStream(ctx context.Context, input TextTurnInput, yield func(agent.StreamEvent) bool) (TextTurnOutput, error) {
	memories, err := p.memory.Search(ctx, memory.SearchRequest{
		UserID: input.UserID,
		RunID:  input.SessionID,
		Query:  input.Text,
	})
	if err != nil {
		p.logger.Warn("memory search failed", "error", err, "user_id", input.UserID)
		memories = nil
	}

	events := make([]agent.StreamEvent, 0, 8)
	output, err := p.agent.RunTurnStream(ctx, agent.TurnInput{
		UserID:    input.UserID,
		SessionID: input.SessionID,
		Text:      input.Text,
		Memories:  memories,
		Metadata:  input.Metadata,
	}, func(event agent.StreamEvent) bool {
		events = append(events, event)
		if yield == nil {
			return true
		}
		return yield(event)
	})
	if err != nil {
		return TextTurnOutput{}, err
	}

	go p.writeMemory(context.WithoutCancel(ctx), input, output)

	return TextTurnOutput{
		Text:     output.Text,
		Events:   events,
		Memories: memories,
	}, nil
}

func (p *Pipeline) writeMemory(ctx context.Context, input TextTurnInput, output agent.TurnOutput) {
	err := p.memory.Add(ctx, memory.AddRequest{
		UserID: input.UserID,
		RunID:  input.SessionID,
		Messages: []memory.Message{
			{Role: "user", Content: input.Text},
			{Role: "assistant", Content: output.Text},
		},
		Metadata: map[string]any{
			"session_id": input.SessionID,
			"source":     "text_turn",
		},
	})
	if err != nil {
		p.logger.Warn("memory write failed", "error", err, "user_id", input.UserID)
	}
}
