package memory

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestWriterPreservesUserOrder(t *testing.T) {
	store := &recordingStore{}
	writer := NewWriter(store, WriterConfig{}, slog.Default())
	writer.Enqueue(AddRequest{UserID: "alice", RunID: "1"})
	writer.Enqueue(AddRequest{UserID: "alice", RunID: "2"})
	writer.Enqueue(AddRequest{UserID: "alice", RunID: "3"})
	writer.Close()

	got := store.requests()
	if len(got) != 3 {
		t.Fatalf("add requests = %d, want 3", len(got))
	}
	for i, want := range []string{"1", "2", "3"} {
		if got[i].RunID != want {
			t.Fatalf("request %d run_id = %q, want %q", i, got[i].RunID, want)
		}
	}
}

func TestWriterDropsOldestQueuedRequest(t *testing.T) {
	store := newBlockingStore()
	writer := NewWriter(store, WriterConfig{QueueSize: 2}, slog.Default())
	writer.Enqueue(AddRequest{UserID: "alice", RunID: "1"})
	<-store.started
	writer.Enqueue(AddRequest{UserID: "alice", RunID: "2"})
	writer.Enqueue(AddRequest{UserID: "alice", RunID: "3"})
	writer.Enqueue(AddRequest{UserID: "alice", RunID: "4"})
	close(store.release)
	writer.Close()

	got := store.requests()
	if len(got) != 3 {
		t.Fatalf("add requests = %d, want 3", len(got))
	}
	for i, want := range []string{"1", "3", "4"} {
		if got[i].RunID != want {
			t.Fatalf("request %d run_id = %q, want %q", i, got[i].RunID, want)
		}
	}
}

func TestWriterLimitsGlobalConcurrency(t *testing.T) {
	store := newConcurrencyStore()
	writer := NewWriter(store, WriterConfig{MaxConcurrency: 1}, slog.Default())
	writer.Enqueue(AddRequest{UserID: "alice"})
	<-store.firstStarted
	writer.Enqueue(AddRequest{UserID: "bob"})
	select {
	case <-store.secondStarted:
		t.Fatal("second write started before the first was released")
	case <-time.After(25 * time.Millisecond):
	}
	close(store.release)
	select {
	case <-store.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second write did not start after first release")
	}
	writer.Close()
}

type recordingStore struct {
	mu   sync.Mutex
	reqs []AddRequest
}

func (s *recordingStore) Search(context.Context, SearchRequest) ([]Memory, error) { return nil, nil }
func (s *recordingStore) Add(_ context.Context, req AddRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
	return nil
}
func (s *recordingStore) requests() []AddRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AddRequest(nil), s.reqs...)
}

type blockingStore struct {
	recordingStore
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingStore() *blockingStore {
	return &blockingStore{started: make(chan struct{}), release: make(chan struct{})}
}

func (s *blockingStore) Add(ctx context.Context, req AddRequest) error {
	s.recordingStore.Add(ctx, req)
	s.once.Do(func() { close(s.started) })
	<-s.release
	return nil
}

type concurrencyStore struct {
	recordingStore
	firstStarted  chan struct{}
	secondStarted chan struct{}
	release       chan struct{}
	mu            sync.Mutex
	calls         int
}

func newConcurrencyStore() *concurrencyStore {
	return &concurrencyStore{
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		release:       make(chan struct{}),
	}
}

func (s *concurrencyStore) Add(ctx context.Context, req AddRequest) error {
	s.recordingStore.Add(ctx, req)
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		close(s.firstStarted)
	} else {
		close(s.secondStarted)
	}
	<-s.release
	return nil
}
