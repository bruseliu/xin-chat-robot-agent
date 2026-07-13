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
	Text   string              `json:"text"`
	Events []agent.StreamEvent `json:"events,omitempty"`
}

type Pipeline struct {
	agent  agent.Runner
	writer *memory.Writer
	logger *slog.Logger
}

func NewPipeline(agentRunner agent.Runner, writer *memory.Writer, logger *slog.Logger) *Pipeline {
	return &Pipeline{
		agent:  agentRunner,
		writer: writer,
		logger: logger,
	}
}

func (p *Pipeline) HandleTextTurn(ctx context.Context, input TextTurnInput) (TextTurnOutput, error) {
	return p.HandleTextTurnStream(ctx, input, nil)
}

func (p *Pipeline) HandleTextTurnStream(ctx context.Context, input TextTurnInput, yield func(agent.StreamEvent) bool) (TextTurnOutput, error) {
	events := make([]agent.StreamEvent, 0, 8)
	output, err := p.agent.RunTurnStream(ctx, agent.TurnInput{
		UserID:    input.UserID,
		SessionID: input.SessionID,
		Text:      input.Text,
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

	if p.writer != nil {
		p.writer.Enqueue(memory.AddRequest{
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
	}

	return TextTurnOutput{
		Text:   output.Text,
		Events: events,
	}, nil
}
