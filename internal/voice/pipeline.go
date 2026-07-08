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
	Text     string          `json:"text"`
	Memories []memory.Memory `json:"memories,omitempty"`
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
	memories, err := p.memory.Search(ctx, memory.SearchRequest{
		UserID: input.UserID,
		RunID:  input.SessionID,
		Query:  input.Text,
	})
	if err != nil {
		p.logger.Warn("memory search failed", "error", err, "user_id", input.UserID)
		memories = nil
	}

	output, err := p.agent.RunTurn(ctx, agent.TurnInput{
		UserID:    input.UserID,
		SessionID: input.SessionID,
		Text:      input.Text,
		Memories:  memories,
		Metadata:  input.Metadata,
	})
	if err != nil {
		return TextTurnOutput{}, err
	}

	go p.writeMemory(context.WithoutCancel(ctx), input, output)

	return TextTurnOutput{
		Text:     output.Text,
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
