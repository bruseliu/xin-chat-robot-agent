package retention

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu     sync.Mutex
	policy Policy
	calls  int
	result Result
}

func (store *fakeStore) Cleanup(_ context.Context, policy Policy) (Result, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.calls++
	store.policy = policy
	return store.result, nil
}

func TestWorkerRunOnceUsesBoundedDefaults(t *testing.T) {
	store := &fakeStore{}
	worker, err := New(store, Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	worker.RunOnce(context.Background())
	if store.calls != 1 || store.policy.BatchSize != 500 || store.policy.RawTTL != 30*24*time.Hour {
		t.Fatalf("policy = %+v, calls = %d", store.policy, store.calls)
	}
}

func TestWorkerStopsWhenCanceled(t *testing.T) {
	store := &fakeStore{}
	worker, err := New(store, Config{Interval: time.Millisecond})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}
