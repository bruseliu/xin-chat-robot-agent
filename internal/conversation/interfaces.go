package conversation

import "context"

// TurnRecorder persists the lifecycle of a semantic conversation turn.
type TurnRecorder interface {
	BeginTurn(context.Context, BeginTurnRequest) (TurnHandle, error)
	FinishTurn(context.Context, FinishTurnRequest) error
}

// ContextReader loads bounded model context for one conversation.
type ContextReader interface {
	LoadContext(context.Context, string, string, string, ContextLimits) (Context, error)
}

// CompactionStore provides the optimistic-concurrency operations used by a
// background summary worker.
type CompactionStore interface {
	LoadCompactionCandidate(context.Context, string, string, string, int) (CompactionCandidate, error)
	CommitSummary(context.Context, SummaryCommit) (bool, error)
}

// CompactionTrigger accepts best-effort requests to compact a conversation.
// Implementations must not block the realtime voice path.
type CompactionTrigger interface {
	Enqueue(ConversationKey)
}
