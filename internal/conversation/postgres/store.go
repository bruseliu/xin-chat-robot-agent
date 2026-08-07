// Package postgres implements durable semantic conversation storage.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"xin-chat-robot-agent/internal/conversation"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const defaultSchema = "voice_agent"

var schemaNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Config configures a PostgreSQL-backed conversation store.
type Config struct {
	DSN                 string
	Schema              string
	AutoMigrate         bool
	CompactionBatchSize int
	Now                 func() time.Time
}

// Store implements the conversation storage interfaces using PostgreSQL.
type Store struct {
	db                  *gorm.DB
	schema              string
	compactionBatchSize int
	now                 func() time.Time
}

var (
	_ conversation.TurnRecorder    = (*Store)(nil)
	_ conversation.ContextReader   = (*Store)(nil)
	_ conversation.CompactionStore = (*Store)(nil)
)

// New opens a PostgreSQL connection and optionally runs the additive migration.
func New(cfg Config) (*Store, error) {
	if cfg.DSN == "" {
		return nil, fmt.Errorf("conversation postgres DSN is required")
	}
	db, err := gorm.Open(postgres.Open(cfg.DSN), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open conversation postgres store: %w", err)
	}
	return NewWithDB(db, cfg)
}

// NewWithDB constructs a store from an existing GORM database. It is useful
// for tests and lets the composition root share one connection pool later.
func NewWithDB(db *gorm.DB, cfg Config) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("conversation postgres database is required")
	}
	if cfg.Schema == "" {
		cfg.Schema = defaultSchema
	}
	if !schemaNamePattern.MatchString(cfg.Schema) {
		return nil, fmt.Errorf("invalid conversation postgres schema %q", cfg.Schema)
	}
	if cfg.CompactionBatchSize <= 0 {
		cfg.CompactionBatchSize = 64
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	store := &Store{
		db:                  db,
		schema:              cfg.Schema,
		compactionBatchSize: cfg.CompactionBatchSize,
		now:                 cfg.Now,
	}
	if cfg.AutoMigrate {
		if err := store.Migrate(context.Background()); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// BeginTurn allocates a pending turn or returns the existing idempotent turn.
func (s *Store) BeginTurn(ctx context.Context, request conversation.BeginTurnRequest) (conversation.TurnHandle, error) {
	if err := request.Validate(); err != nil {
		return conversation.TurnHandle{}, err
	}

	var handle conversation.TurnHandle
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := s.now()
		conversationRow := conversationModel{
			AppName:        request.AppName,
			UserID:         request.UserID,
			ConversationID: request.ConversationID,
			NextTurnNo:     1,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := tx.Table(s.conversationsTable()).Clauses(clause.OnConflict{DoNothing: true}).Create(&conversationRow).Error; err != nil {
			return fmt.Errorf("create conversation: %w", err)
		}

		var current conversationModel
		if err := tx.Table(s.conversationsTable()).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("app_name = ? AND user_id = ? AND conversation_id = ?", request.AppName, request.UserID, request.ConversationID).
			Take(&current).Error; err != nil {
			return fmt.Errorf("lock conversation: %w", err)
		}

		var existing turnModel
		err := tx.Table(s.turnsTable()).
			Where("app_name = ? AND user_id = ? AND conversation_id = ? AND idempotency_key = ?", request.AppName, request.UserID, request.ConversationID, request.IdempotencyKey).
			Take(&existing).Error
		switch {
		case err == nil:
			handle = toHandle(existing)
			return nil
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return fmt.Errorf("lookup idempotent turn: %w", err)
		}

		turn := turnModel{
			AppName:        request.AppName,
			UserID:         request.UserID,
			ConversationID: request.ConversationID,
			TurnNo:         current.NextTurnNo,
			IdempotencyKey: request.IdempotencyKey,
			Source:         request.Source,
			UserText:       request.UserText,
			Status:         string(conversation.TurnPending),
			CreatedAt:      now,
		}
		if err := tx.Table(s.turnsTable()).Create(&turn).Error; err != nil {
			return fmt.Errorf("create conversation turn: %w", err)
		}
		if err := tx.Table(s.conversationsTable()).
			Where("app_name = ? AND user_id = ? AND conversation_id = ?", request.AppName, request.UserID, request.ConversationID).
			Updates(map[string]any{"next_turn_no": current.NextTurnNo + 1, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("advance conversation turn number: %w", err)
		}
		handle = toHandle(turn)
		return nil
	})
	if err != nil {
		return conversation.TurnHandle{}, err
	}
	return handle, nil
}

// FinishTurn records a terminal assistant result. Repeating the same terminal
// status is accepted to make a retried completion idempotent.
func (s *Store) FinishTurn(ctx context.Context, request conversation.FinishTurnRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row turnModel
		if err := tx.Table(s.turnsTable()).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("app_name = ? AND user_id = ? AND conversation_id = ? AND turn_no = ?", request.Handle.AppName, request.Handle.UserID, request.Handle.ConversationID, request.Handle.TurnNo).
			Take(&row).Error; err != nil {
			return fmt.Errorf("load conversation turn: %w", err)
		}

		current := conversation.TurnStatus(row.Status)
		if !current.CanTransitionTo(request.Status) {
			return fmt.Errorf("invalid conversation turn transition %q -> %q", current, request.Status)
		}
		if current == request.Status {
			return nil
		}

		now := s.now()
		if err := tx.Table(s.turnsTable()).
			Where("app_name = ? AND user_id = ? AND conversation_id = ? AND turn_no = ?", request.Handle.AppName, request.Handle.UserID, request.Handle.ConversationID, request.Handle.TurnNo).
			Updates(map[string]any{
				"assistant_text": request.AssistantText,
				"status":         string(request.Status),
				"error_code":     request.ErrorCode,
				"completed_at":   now,
			}).Error; err != nil {
			return fmt.Errorf("finish conversation turn: %w", err)
		}
		return nil
	})
}

// LoadContext returns only the requested number of completed turns. The SQL
// query is bounded before rows reach Go memory.
func (s *Store) LoadContext(ctx context.Context, appName, userID, conversationID string, limits conversation.ContextLimits) (conversation.Context, error) {
	key := conversation.ConversationKey{AppName: appName, UserID: userID, ConversationID: conversationID}
	if err := key.Validate(); err != nil {
		return conversation.Context{}, err
	}

	result := conversation.Context{}
	var row conversationModel
	err := s.db.WithContext(ctx).Table(s.conversationsTable()).
		Where("app_name = ? AND user_id = ? AND conversation_id = ?", appName, userID, conversationID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return result, nil
	}
	if err != nil {
		return conversation.Context{}, fmt.Errorf("load conversation summary: %w", err)
	}
	result.Summary = conversation.Summary{
		Text:                    row.SummaryText,
		SummarizedThroughTurnNo: row.SummarizedThroughTurn,
		Version:                 row.SummaryVersion,
	}
	if limits.MaxRecentTurns <= 0 {
		return result, nil
	}

	var rows []turnModel
	if err := s.db.WithContext(ctx).Table(s.turnsTable()).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND status = ?", appName, userID, conversationID, string(conversation.TurnCompleted)).
		Order("turn_no DESC").
		Limit(limits.MaxRecentTurns).
		Find(&rows).Error; err != nil {
		return conversation.Context{}, fmt.Errorf("load recent conversation turns: %w", err)
	}
	result.RecentTurns = make([]conversation.Turn, len(rows))
	for i := range rows {
		result.RecentTurns[len(rows)-1-i] = toTurn(rows[i])
	}
	return result, nil
}

// LoadCompactionCandidate loads a bounded batch of completed, unsummarized
// turns while excluding the newest keepRecentTurns exact turns.
func (s *Store) LoadCompactionCandidate(ctx context.Context, appName, userID, conversationID string, keepRecentTurns int) (conversation.CompactionCandidate, error) {
	key := conversation.ConversationKey{AppName: appName, UserID: userID, ConversationID: conversationID}
	if err := key.Validate(); err != nil {
		return conversation.CompactionCandidate{}, err
	}
	if keepRecentTurns < 0 {
		return conversation.CompactionCandidate{}, fmt.Errorf("keep recent turns must not be negative")
	}

	var row conversationModel
	err := s.db.WithContext(ctx).Table(s.conversationsTable()).
		Where("app_name = ? AND user_id = ? AND conversation_id = ?", appName, userID, conversationID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return conversation.CompactionCandidate{Key: key, KeepRecentTurns: keepRecentTurns}, nil
	}
	if err != nil {
		return conversation.CompactionCandidate{}, fmt.Errorf("load compaction summary: %w", err)
	}

	candidate := conversation.CompactionCandidate{
		Key:             key,
		Summary:         conversation.Summary{Text: row.SummaryText, SummarizedThroughTurnNo: row.SummarizedThroughTurn, Version: row.SummaryVersion},
		KeepRecentTurns: keepRecentTurns,
	}
	query := s.db.WithContext(ctx).Table(s.turnsTable()).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND status = ? AND turn_no > ?", appName, userID, conversationID, string(conversation.TurnCompleted), row.SummarizedThroughTurn)
	if keepRecentTurns > 0 {
		var protected []turnModel
		if err := query.Select("turn_no").Order("turn_no DESC").Limit(keepRecentTurns).Find(&protected).Error; err != nil {
			return conversation.CompactionCandidate{}, fmt.Errorf("load protected conversation turns: %w", err)
		}
		if len(protected) < keepRecentTurns {
			return candidate, nil
		}
		query = query.Where("turn_no < ?", protected[len(protected)-1].TurnNo)
	}

	var rows []turnModel
	if err := query.Order("turn_no ASC").Limit(s.compactionBatchSize).Find(&rows).Error; err != nil {
		return conversation.CompactionCandidate{}, fmt.Errorf("load compaction turns: %w", err)
	}
	candidate.Turns = make([]conversation.Turn, len(rows))
	for i := range rows {
		candidate.Turns[i] = toTurn(rows[i])
	}
	return candidate, nil
}

// CommitSummary updates a summary only when its expected version is current.
func (s *Store) CommitSummary(ctx context.Context, commit conversation.SummaryCommit) (bool, error) {
	if err := commit.Key.Validate(); err != nil {
		return false, err
	}
	if commit.ExpectedVersion < 0 || commit.SummarizedThroughTurnNo < 0 {
		return false, fmt.Errorf("summary version and turn number must not be negative")
	}
	result := s.db.WithContext(ctx).Table(s.conversationsTable()).
		Where("app_name = ? AND user_id = ? AND conversation_id = ? AND summary_version = ? AND summarized_through_turn <= ?", commit.Key.AppName, commit.Key.UserID, commit.Key.ConversationID, commit.ExpectedVersion, commit.SummarizedThroughTurnNo).
		Updates(map[string]any{
			"summary_text":            commit.Text,
			"summarized_through_turn": commit.SummarizedThroughTurnNo,
			"summary_version":         commit.ExpectedVersion + 1,
			"updated_at":              s.now(),
		})
	if result.Error != nil {
		return false, fmt.Errorf("commit conversation summary: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

func (s *Store) conversationsTable() string {
	return quoteIdentifier(s.schema) + "." + quoteIdentifier("conversations")
}

func (s *Store) turnsTable() string {
	return quoteIdentifier(s.schema) + "." + quoteIdentifier("conversation_turns")
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func toHandle(row turnModel) conversation.TurnHandle {
	return conversation.TurnHandle{
		ConversationKey: conversation.ConversationKey{AppName: row.AppName, UserID: row.UserID, ConversationID: row.ConversationID},
		TurnNo:          row.TurnNo,
	}
}

func toTurn(row turnModel) conversation.Turn {
	completedAt := time.Time{}
	if row.CompletedAt != nil {
		completedAt = *row.CompletedAt
	}
	return conversation.Turn{
		TurnNo:        row.TurnNo,
		UserText:      row.UserText,
		AssistantText: row.AssistantText,
		Status:        conversation.TurnStatus(row.Status),
		CreatedAt:     row.CreatedAt,
		CompletedAt:   completedAt,
	}
}
