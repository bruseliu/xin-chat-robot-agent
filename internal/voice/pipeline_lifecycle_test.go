package voice

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/conversation"
)

type lifecycleRecorder struct {
	mu       sync.Mutex
	beginErr error
	begin    int
	finish   []conversation.FinishTurnRequest
}

func (recorder *lifecycleRecorder) BeginTurn(_ context.Context, request conversation.BeginTurnRequest) (conversation.TurnHandle, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.begin++
	if recorder.beginErr != nil {
		return conversation.TurnHandle{}, recorder.beginErr
	}
	return conversation.TurnHandle{ConversationKey: request.ConversationKey, TurnNo: 1}, nil
}

func (recorder *lifecycleRecorder) FinishTurn(_ context.Context, request conversation.FinishTurnRequest) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.finish = append(recorder.finish, request)
	return nil
}

type lifecycleTrigger struct {
	keys []conversation.ConversationKey
}

func (trigger *lifecycleTrigger) Enqueue(key conversation.ConversationKey) {
	trigger.keys = append(trigger.keys, key)
}

type lifecycleRunner struct {
	output agent.TurnOutput
	err    error
	cancel bool
}

func (runner lifecycleRunner) RunTurn(ctx context.Context, input agent.TurnInput) (agent.TurnOutput, error) {
	return runner.RunTurnStream(ctx, input, nil)
}

func (runner lifecycleRunner) RunTurnStream(ctx context.Context, _ agent.TurnInput, yield func(agent.StreamEvent) bool) (agent.TurnOutput, error) {
	if yield != nil {
		yield(agent.StreamEvent{Type: agent.StreamEventLLMDelta, TextDelta: "partial", Partial: true})
	}
	if runner.cancel {
		<-ctx.Done()
		return agent.TurnOutput{Text: "partial"}, nil
	}
	return runner.output, runner.err
}

func TestPipelineRecordsCompletedTurnAndTriggersCompaction(t *testing.T) {
	recorder := &lifecycleRecorder{}
	trigger := &lifecycleTrigger{}
	pipeline := NewPipeline(lifecycleRunner{output: agent.TurnOutput{Text: "answer"}}, nil, slog.Default(), WithAppName("app"), WithTurnRecorder(recorder), WithCompactionTrigger(trigger))
	output, err := pipeline.HandleTextTurn(context.Background(), TextTurnInput{UserID: "user", SessionID: "session", Text: "question", Metadata: map[string]string{"idempotency_key": "request-1"}})
	if err != nil || output.Text != "answer" {
		t.Fatalf("HandleTextTurn() = %+v, %v", output, err)
	}
	if len(recorder.finish) != 1 || recorder.finish[0].Status != conversation.TurnCompleted {
		t.Fatalf("finish calls = %+v, want completed", recorder.finish)
	}
	if len(trigger.keys) != 1 || trigger.keys[0].AppName != "app" {
		t.Fatalf("compaction keys = %+v, want one app key", trigger.keys)
	}
}

func TestPipelineMarksCanceledTurnInterrupted(t *testing.T) {
	recorder := &lifecycleRecorder{}
	trigger := &lifecycleTrigger{}
	pipeline := NewPipeline(lifecycleRunner{cancel: true}, nil, slog.Default(), WithTurnRecorder(recorder), WithCompactionTrigger(trigger))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pipeline.HandleTextTurn(ctx, TextTurnInput{UserID: "user", SessionID: "session", Text: "question"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("HandleTextTurn() error = %v, want context canceled", err)
	}
	if len(recorder.finish) != 1 || recorder.finish[0].Status != conversation.TurnInterrupted {
		t.Fatalf("finish calls = %+v, want interrupted", recorder.finish)
	}
	if len(trigger.keys) != 0 {
		t.Fatalf("compaction keys = %+v, want none", trigger.keys)
	}
}

func TestPipelineRecorderFailureDoesNotStopResponse(t *testing.T) {
	recorder := &lifecycleRecorder{beginErr: errors.New("store unavailable")}
	pipeline := NewPipeline(lifecycleRunner{output: agent.TurnOutput{Text: "answer"}}, nil, slog.Default(), WithTurnRecorder(recorder))
	output, err := pipeline.HandleTextTurn(context.Background(), TextTurnInput{UserID: "user", SessionID: "session", Text: "question"})
	if err != nil || output.Text != "answer" {
		t.Fatalf("HandleTextTurn() = %+v, %v, want normal response", output, err)
	}
	if len(recorder.finish) != 0 {
		t.Fatalf("finish calls = %+v, want none", recorder.finish)
	}
}
