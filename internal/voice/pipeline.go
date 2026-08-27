package voice

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/conversation"
	"xin-chat-robot-agent/internal/memory"
	"xin-chat-robot-agent/internal/vision"
)

type TextTurnInput struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id,omitempty"`
	Text      string `json:"text"`
	// Frame is an ephemeral camera snapshot for this turn. It is excluded from
	// JSON and all persistence paths.
	Frame    *vision.Frame     `json:"-"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type TextTurnOutput struct {
	Text   string              `json:"text"`
	Events []agent.StreamEvent `json:"events,omitempty"`
}

type Pipeline struct {
	agent      agent.Runner
	writer     *memory.Writer
	logger     *slog.Logger
	recorder   conversation.TurnRecorder
	compaction conversation.CompactionTrigger
	appName    string
}

type PipelineOption func(*Pipeline)

func WithTurnRecorder(recorder conversation.TurnRecorder) PipelineOption {
	return func(pipeline *Pipeline) { pipeline.recorder = recorder }
}

func WithCompactionTrigger(trigger conversation.CompactionTrigger) PipelineOption {
	return func(pipeline *Pipeline) { pipeline.compaction = trigger }
}

func WithAppName(appName string) PipelineOption {
	return func(pipeline *Pipeline) { pipeline.appName = appName }
}

func NewPipeline(agentRunner agent.Runner, writer *memory.Writer, logger *slog.Logger, options ...PipelineOption) *Pipeline {
	pipeline := &Pipeline{
		agent:   agentRunner,
		writer:  writer,
		logger:  logger,
		appName: "voice-agent",
	}
	for _, option := range options {
		if option != nil {
			option(pipeline)
		}
	}
	if pipeline.logger == nil {
		pipeline.logger = slog.Default()
	}
	return pipeline
}

func (p *Pipeline) HandleTextTurn(ctx context.Context, input TextTurnInput) (TextTurnOutput, error) {
	return p.HandleTextTurnStream(ctx, input, nil)
}

func (p *Pipeline) HandleTextTurnStream(ctx context.Context, input TextTurnInput, yield func(agent.StreamEvent) bool) (TextTurnOutput, error) {
	var handle conversation.TurnHandle
	recorded := false
	if p.recorder != nil && strings.TrimSpace(input.Text) != "" {
		beginCtx, cancel := context.WithTimeout(ctx, time.Second)
		begin, err := p.recorder.BeginTurn(beginCtx, conversation.BeginTurnRequest{
			ConversationKey: conversation.ConversationKey{AppName: p.appName, UserID: input.UserID, ConversationID: input.SessionID},
			UserText:        input.Text,
			Source:          turnSource(input.Metadata),
			IdempotencyKey:  turnIdempotencyKey(input.Metadata),
		})
		cancel()
		if err != nil {
			p.logger.Warn("record conversation turn start failed", "error", err, "user_id", input.UserID, "session_id", input.SessionID)
		} else {
			handle, recorded = begin, true
		}
	}
	events := make([]agent.StreamEvent, 0, 8)
	output, err := p.agent.RunTurnStream(ctx, agent.TurnInput{
		UserID:    input.UserID,
		SessionID: input.SessionID,
		Text:      input.Text,
		Frame:     input.Frame,
		Metadata:  input.Metadata,
	}, func(event agent.StreamEvent) bool {
		events = append(events, event)
		if yield == nil {
			return true
		}
		return yield(event)
	})
	if err != nil {
		p.finishTurn(ctx, handle, recorded, output.Text, conversation.TurnFailed, "agent_run_failed")
		return TextTurnOutput{}, err
	}
	if ctx.Err() != nil {
		p.finishTurn(ctx, handle, recorded, output.Text, conversation.TurnInterrupted, "")
		return TextTurnOutput{}, ctx.Err()
	}
	p.finishTurn(ctx, handle, recorded, output.Text, conversation.TurnCompleted, "")

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
			Prompt: memory.EnglishFactExtractionPrompt,
		})
	}

	if p.compaction != nil && recorded {
		p.compaction.Enqueue(handle.ConversationKey)
	}
	return TextTurnOutput{
		Text:   output.Text,
		Events: events,
	}, nil
}
