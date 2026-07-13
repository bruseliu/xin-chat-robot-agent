package memory

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type WriterConfig struct {
	MaxConcurrency int
	QueueSize      int
	IdleTimeout    time.Duration
}

type Writer struct {
	store   Store
	logger  *slog.Logger
	sem     chan struct{}
	queueSz int
	idle    time.Duration

	mu     sync.Mutex
	queues map[string]chan AddRequest
	closed bool
	wg     sync.WaitGroup
}

func NewWriter(store Store, cfg WriterConfig, logger *slog.Logger) *Writer {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 5
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 64
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Writer{
		store:   store,
		logger:  logger,
		sem:     make(chan struct{}, cfg.MaxConcurrency),
		queueSz: cfg.QueueSize,
		idle:    cfg.IdleTimeout,
		queues:  make(map[string]chan AddRequest),
	}
}

func (w *Writer) Enqueue(req AddRequest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	ch, ok := w.queues[req.UserID]
	if !ok {
		ch = make(chan AddRequest, w.queueSz)
		w.queues[req.UserID] = ch
		w.wg.Add(1)
		go w.consume(req.UserID, ch)
	}
	select {
	case ch <- req:
	default:
		w.logger.Warn("memory writer queue full, dropping oldest", "user_id", req.UserID)
		select {
		case <-ch:
		default:
		}
		ch <- req
	}
}

func (w *Writer) consume(userID string, ch chan AddRequest) {
	defer w.wg.Done()
	timer := time.NewTimer(w.idle)
	defer timer.Stop()

	for {
		select {
		case req, ok := <-ch:
			if !ok {
				return
			}
			resetTimer(timer, w.idle)
			w.sem <- struct{}{}
			w.process(req)
			<-w.sem
		case <-timer.C:
			w.mu.Lock()
			if len(ch) == 0 {
				delete(w.queues, userID)
				w.mu.Unlock()
				return
			}
			w.mu.Unlock()
			resetTimer(timer, w.idle)
		}
	}
}

func (w *Writer) process(req AddRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.store.Add(ctx, req); err != nil {
		w.logger.Warn("memory write failed", "error", err, "user_id", req.UserID)
	}
}

func (w *Writer) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	for uid, ch := range w.queues {
		close(ch)
		delete(w.queues, uid)
	}
	w.mu.Unlock()
	w.wg.Wait()
}

func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}
