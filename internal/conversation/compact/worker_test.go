package compact

import (
	"context"
	"sync"
	"testing"
	"time"

	"xin-chat-robot-agent/internal/conversation"
)

type testStore struct {
	mu        sync.Mutex
	candidate conversation.CompactionCandidate
	committed bool
}

func (store *testStore) LoadCompactionCandidate(context.Context, string, string, string, int) (conversation.CompactionCandidate, error) {
	return store.candidate, nil
}

func (store *testStore) CommitSummary(_ context.Context, commit conversation.SummaryCommit) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.committed = commit.SummarizedThroughTurnNo > 0
	return true, nil
}

type testSummarizer struct {
	mu    sync.Mutex
	turns []conversation.Turn
}

func (summarizer *testSummarizer) Summarize(_ context.Context, _ string, turns []conversation.Turn) (string, error) {
	summarizer.mu.Lock()
	defer summarizer.mu.Unlock()
	summarizer.turns = append([]conversation.Turn(nil), turns...)
	return "summary", nil
}

func TestWorkerCompactsCompletedTurnsOnly(t *testing.T) {
	key := conversation.ConversationKey{AppName: "app", UserID: "user", ConversationID: "conversation"}
	store := &testStore{candidate: conversation.CompactionCandidate{Key: key, KeepRecentTurns: 8, Turns: []conversation.Turn{
		{TurnNo: 1, Status: conversation.TurnCompleted},
		{TurnNo: 2, Status: conversation.TurnInterrupted},
		{TurnNo: 3, Status: conversation.TurnCompleted},
	}}}
	summarizer := &testSummarizer{}
	worker, err := New(store, summarizer, Config{TriggerTurns: 10, Workers: 1}, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()
	worker.Enqueue(key)
	waitFor(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.committed
	})
	cancel()
	<-done
	summarizer.mu.Lock()
	defer summarizer.mu.Unlock()
	if len(summarizer.turns) != 2 || summarizer.turns[0].TurnNo != 1 || summarizer.turns[1].TurnNo != 3 {
		t.Fatalf("summarizer turns = %+v, want completed turns only", summarizer.turns)
	}
}

func TestWorkerBelowThresholdDoesNotCommit(t *testing.T) {
	key := conversation.ConversationKey{AppName: "app", UserID: "user", ConversationID: "conversation"}
	store := &testStore{candidate: conversation.CompactionCandidate{Key: key, KeepRecentTurns: 8, Turns: []conversation.Turn{{TurnNo: 1, Status: conversation.TurnCompleted}}}}
	worker, err := New(store, &testSummarizer{}, Config{TriggerTurns: 20, Workers: 1}, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()
	worker.Enqueue(key)
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if store.committed {
		t.Fatal("worker committed a below-threshold candidate")
	}
}

func TestWorkerEnqueueDeduplicates(t *testing.T) {
	worker, err := New(&testStore{}, &testSummarizer{}, Config{QueueSize: 1}, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	key := conversation.ConversationKey{AppName: "app", UserID: "user", ConversationID: "conversation"}
	worker.Enqueue(key)
	worker.Enqueue(key)
	if len(worker.queue) != 1 {
		t.Fatalf("queue length = %d, want 1", len(worker.queue))
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
