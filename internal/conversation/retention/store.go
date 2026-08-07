package retention

import (
	"context"
	"time"
)

// Store deletes only rows that satisfy the retention policy. Implementations
// must use bounded database-side batches.
type Store interface {
	Cleanup(context.Context, Policy) (Result, error)
}

type Policy struct {
	RawTTL     time.Duration
	SummaryTTL time.Duration
	BatchSize  int
	DryRun     bool
	Now        time.Time
}

type Result struct {
	CompletedDeleted   int
	InterruptedDeleted int
	SummariesDeleted   int
}

func (result Result) Total() int {
	return result.CompletedDeleted + result.InterruptedDeleted + result.SummariesDeleted
}
