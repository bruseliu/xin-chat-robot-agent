// Package conversation defines provider-independent conversation state contracts.
package conversation

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrMissingAppName        = errors.New("conversation app name is required")
	ErrMissingUserID         = errors.New("conversation user ID is required")
	ErrMissingConversationID = errors.New("conversation ID is required")
)

// TurnStatus is the lifecycle state of a semantic user/assistant interaction.
type TurnStatus string

const (
	TurnPending     TurnStatus = "pending"
	TurnCompleted   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnFailed      TurnStatus = "failed"
)

// Valid reports whether status is a known turn lifecycle state.
func (status TurnStatus) Valid() bool {
	switch status {
	case TurnPending, TurnCompleted, TurnInterrupted, TurnFailed:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether a persisted turn may move to next.
// Repeating the same status is allowed so FinishTurn can be idempotent.
func (status TurnStatus) CanTransitionTo(next TurnStatus) bool {
	if !status.Valid() || !next.Valid() {
		return false
	}
	if status == next {
		return true
	}
	return status == TurnPending
}

// ConversationKey identifies one durable conversation within an application.
type ConversationKey struct {
	AppName        string
	UserID         string
	ConversationID string
}

// Validate checks that all fields needed to address a conversation are present.
func (key ConversationKey) Validate() error {
	if key.AppName == "" {
		return ErrMissingAppName
	}
	if key.UserID == "" {
		return ErrMissingUserID
	}
	if key.ConversationID == "" {
		return ErrMissingConversationID
	}
	return nil
}

// TurnHandle identifies an allocated semantic turn.
type TurnHandle struct {
	ConversationKey
	TurnNo int64
}

// Validate checks that the handle can identify an existing turn.
func (handle TurnHandle) Validate() error {
	if err := handle.ConversationKey.Validate(); err != nil {
		return err
	}
	if handle.TurnNo < 1 {
		return fmt.Errorf("conversation turn number must be positive")
	}
	return nil
}

// BeginTurnRequest creates or reuses a pending semantic user turn.
type BeginTurnRequest struct {
	ConversationKey
	UserText       string
	Source         string
	IdempotencyKey string
}

// Validate checks the fields required to persist a semantic user turn.
func (request BeginTurnRequest) Validate() error {
	if err := request.ConversationKey.Validate(); err != nil {
		return err
	}
	if request.UserText == "" {
		return errors.New("conversation user text is required")
	}
	if request.Source == "" {
		return errors.New("conversation source is required")
	}
	if request.IdempotencyKey == "" {
		return errors.New("conversation idempotency key is required")
	}
	return nil
}

// FinishTurnRequest records the assistant result for an allocated turn.
type FinishTurnRequest struct {
	Handle        TurnHandle
	AssistantText string
	Status        TurnStatus
	ErrorCode     string
}

// Validate checks the fields required to finish a semantic turn.
func (request FinishTurnRequest) Validate() error {
	if err := request.Handle.Validate(); err != nil {
		return err
	}
	if request.Status == TurnPending || !request.Status.Valid() {
		return fmt.Errorf("conversation finish status must be completed, interrupted, or failed")
	}
	return nil
}

// Turn is the durable semantic representation of one user/assistant interaction.
type Turn struct {
	TurnNo        int64
	UserText      string
	AssistantText string
	Status        TurnStatus
	CreatedAt     time.Time
	CompletedAt   time.Time
}

// Summary is the compacted representation of older completed turns.
type Summary struct {
	Text                    string
	SummarizedThroughTurnNo int64
	Version                 int64
}

// Context is the conversation state available to an LLM request.
type Context struct {
	Summary     Summary
	RecentTurns []Turn
}

// ContextLimits bounds the context loaded for a model request.
type ContextLimits struct {
	MaxRecentTurns int
	MaxTextUnits   int
}

// CompactionCandidate is the bounded input used to produce an updated summary.
type CompactionCandidate struct {
	Key             ConversationKey
	Summary         Summary
	Turns           []Turn
	KeepRecentTurns int
}

// SummaryCommit atomically advances a conversation summary from ExpectedVersion.
type SummaryCommit struct {
	Key                     ConversationKey
	Text                    string
	SummarizedThroughTurnNo int64
	ExpectedVersion         int64
}
