package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type Config struct {
	Policy   Policy
	Interval time.Duration
	Logger   *slog.Logger
}

type Worker struct {
	store  Store
	config Config
}

func New(store Store, config Config) (*Worker, error) {
	if store == nil {
		return nil, fmt.Errorf("conversation retention store is required")
	}
	if config.Policy.RawTTL <= 0 {
		config.Policy.RawTTL = 30 * 24 * time.Hour
	}
	if config.Policy.SummaryTTL <= 0 {
		config.Policy.SummaryTTL = 90 * 24 * time.Hour
	}
	if config.Policy.BatchSize <= 0 {
		config.Policy.BatchSize = 500
	}
	if config.Interval <= 0 {
		config.Interval = time.Hour
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Worker{store: store, config: config}, nil
}

// Run executes periodic cleanup. Cancellation is checked between every batch
// by the Store implementation and before every scheduled run here.
func (worker *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(worker.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			worker.RunOnce(ctx)
		}
	}
}

func (worker *Worker) RunOnce(ctx context.Context) {
	policy := worker.config.Policy
	policy.Now = time.Now()
	started := time.Now()
	result, err := worker.store.Cleanup(ctx, policy)
	if err != nil {
		worker.config.Logger.Warn("conversation retention cleanup failed", "error", err)
		return
	}
	worker.config.Logger.Info("conversation retention cleanup complete", "deleted", result.Total(), "dry_run", policy.DryRun, "duration", time.Since(started))
}
