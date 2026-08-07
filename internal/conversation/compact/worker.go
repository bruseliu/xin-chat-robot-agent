package compact

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"xin-chat-robot-agent/internal/conversation"
)

// Worker is a bounded, deduplicating background compaction queue.
type Worker struct {
	store      conversation.CompactionStore
	summarizer Summarizer
	config     Config
	logger     *slog.Logger

	queue   chan conversation.ConversationKey
	mu      sync.Mutex
	pending map[string]struct{}
}

var _ conversation.CompactionTrigger = (*Worker)(nil)

func New(store conversation.CompactionStore, summarizer Summarizer, config Config, logger *slog.Logger) (*Worker, error) {
	if store == nil {
		return nil, fmt.Errorf("conversation compaction store is required")
	}
	if summarizer == nil {
		return nil, fmt.Errorf("conversation summarizer is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	config = config.withDefaults()
	return &Worker{
		store: store, summarizer: summarizer, config: config, logger: logger,
		queue:   make(chan conversation.ConversationKey, config.QueueSize),
		pending: make(map[string]struct{}),
	}, nil
}

// Enqueue is best effort and never blocks a realtime caller.
func (worker *Worker) Enqueue(key conversation.ConversationKey) {
	if key.Validate() != nil {
		return
	}
	encoded := keyString(key)
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if _, exists := worker.pending[encoded]; exists {
		return
	}
	select {
	case worker.queue <- key:
		worker.pending[encoded] = struct{}{}
	default:
		worker.logger.Warn("conversation compaction queue full", "app_name", key.AppName, "user_id", key.UserID, "conversation_id", key.ConversationID)
	}
}

// Run serves queued compaction jobs until ctx is cancelled.
func (worker *Worker) Run(ctx context.Context) {
	var waitGroup sync.WaitGroup
	for range worker.config.Workers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case key := <-worker.queue:
					worker.process(ctx, key)
				}
			}
		}()
	}
	<-ctx.Done()
	waitGroup.Wait()
}

func (worker *Worker) process(ctx context.Context, key conversation.ConversationKey) {
	defer worker.clearPending(key)
	candidate, err := worker.store.LoadCompactionCandidate(ctx, key.AppName, key.UserID, key.ConversationID, worker.config.KeepTurns)
	if err != nil {
		worker.retry(ctx, key, "load", err)
		return
	}
	if len(candidate.Turns)+candidate.KeepRecentTurns < worker.config.TriggerTurns || len(candidate.Turns) == 0 {
		return
	}
	turns := completedTurns(candidate.Turns)
	if len(turns) == 0 {
		return
	}
	summaryCtx, cancel := context.WithTimeout(ctx, worker.config.Timeout)
	summary, err := worker.summarizer.Summarize(summaryCtx, candidate.Summary.Text, turns)
	cancel()
	if err != nil {
		worker.retry(ctx, key, "summarize", err)
		return
	}
	committed, err := worker.store.CommitSummary(ctx, conversation.SummaryCommit{
		Key: key, Text: summary, SummarizedThroughTurnNo: turns[len(turns)-1].TurnNo, ExpectedVersion: candidate.Summary.Version,
	})
	if err != nil {
		worker.retry(ctx, key, "commit", err)
		return
	}
	if !committed {
		worker.clearPending(key)
		worker.Enqueue(key)
	}
}

func (worker *Worker) retry(ctx context.Context, key conversation.ConversationKey, operation string, err error) {
	worker.logger.Warn("conversation compaction failed", "operation", operation, "error", err, "app_name", key.AppName, "user_id", key.UserID, "conversation_id", key.ConversationID)
	go func() {
		timer := time.NewTimer(worker.config.RetryDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			worker.Enqueue(key)
		}
	}()
}

func (worker *Worker) clearPending(key conversation.ConversationKey) {
	worker.mu.Lock()
	delete(worker.pending, keyString(key))
	worker.mu.Unlock()
}

func completedTurns(turns []conversation.Turn) []conversation.Turn {
	completed := make([]conversation.Turn, 0, len(turns))
	for _, turn := range turns {
		if turn.Status == conversation.TurnCompleted {
			completed = append(completed, turn)
		}
	}
	return completed
}

func keyString(key conversation.ConversationKey) string {
	return key.AppName + "\x1f" + key.UserID + "\x1f" + key.ConversationID
}
