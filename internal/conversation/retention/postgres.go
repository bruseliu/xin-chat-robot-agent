package retention

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var schemaPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// PostgresStore performs bounded retention deletes against conversation tables.
type PostgresStore struct {
	db     *gorm.DB
	schema string
}

func NewPostgresStore(dsn, schema string) (*PostgresStore, error) {
	if dsn == "" {
		return nil, fmt.Errorf("conversation retention postgres DSN is required")
	}
	if schema == "" {
		schema = "voice_agent"
	}
	if !schemaPattern.MatchString(schema) {
		return nil, fmt.Errorf("invalid conversation retention schema %q", schema)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	return &PostgresStore{db: db, schema: schema}, nil
}

func (store *PostgresStore) Cleanup(ctx context.Context, policy Policy) (Result, error) {
	if policy.Now.IsZero() {
		return Result{}, fmt.Errorf("retention cleanup time is required")
	}
	completedBefore := policy.Now.Add(-policy.RawTTL)
	summaryBefore := policy.Now.Add(-policy.SummaryTTL)
	if policy.DryRun {
		return store.countCandidates(ctx, completedBefore, summaryBefore, policy.BatchSize)
	}
	completed, err := store.deleteCompleted(ctx, completedBefore, policy.BatchSize)
	if err != nil {
		return Result{}, err
	}
	audit, err := store.deleteAudit(ctx, completedBefore, policy.BatchSize)
	if err != nil {
		return Result{}, err
	}
	return Result{CompletedDeleted: completed, InterruptedDeleted: audit}, nil
}

func (store *PostgresStore) deleteCompleted(ctx context.Context, before interface{}, limit int) (int, error) {
	query := "DELETE FROM " + store.turns() + " t WHERE t.ctid IN (SELECT t2.ctid FROM " + store.turns() + " t2 JOIN " + store.conversations() + " c USING (app_name,user_id,conversation_id) WHERE t2.status='completed' AND t2.completed_at < ? AND t2.turn_no <= c.summarized_through_turn ORDER BY t2.completed_at LIMIT ?)"
	result := store.db.WithContext(ctx).Exec(query, before, limit)
	return int(result.RowsAffected), result.Error
}

func (store *PostgresStore) deleteAudit(ctx context.Context, before interface{}, limit int) (int, error) {
	query := "DELETE FROM " + store.turns() + " t WHERE t.ctid IN (SELECT ctid FROM " + store.turns() + " WHERE status IN ('interrupted','failed') AND completed_at < ? ORDER BY completed_at LIMIT ?)"
	result := store.db.WithContext(ctx).Exec(query, before, limit)
	return int(result.RowsAffected), result.Error
}

func (store *PostgresStore) deleteEmptyConversations(ctx context.Context, before interface{}, limit int) (int, error) {
	query := "DELETE FROM " + store.conversations() + " c WHERE c.ctid IN (SELECT c2.ctid FROM " + store.conversations() + " c2 WHERE c2.updated_at < ? AND NOT EXISTS (SELECT 1 FROM " + store.turns() + " t WHERE t.app_name = c2.app_name AND t.user_id = c2.user_id AND t.conversation_id = c2.conversation_id) LIMIT ?)"
	result := store.db.WithContext(ctx).Exec(query, before, limit)
	return int(result.RowsAffected), result.Error
}

func (store *PostgresStore) countCandidates(ctx context.Context, before, _ interface{}, limit int) (Result, error) {
	var completed, audit int64
	if err := store.db.WithContext(ctx).Raw("SELECT count(*) FROM (SELECT 1 FROM "+store.turns()+" t JOIN "+store.conversations()+" c USING (app_name,user_id,conversation_id) WHERE t.status='completed' AND t.completed_at < ? AND t.turn_no <= c.summarized_through_turn LIMIT ?) q", before, limit).Scan(&completed).Error; err != nil {
		return Result{}, err
	}
	if err := store.db.WithContext(ctx).Raw("SELECT count(*) FROM (SELECT 1 FROM "+store.turns()+" WHERE status IN ('interrupted','failed') AND completed_at < ? LIMIT ?) q", before, limit).Scan(&audit).Error; err != nil {
		return Result{}, err
	}
	return Result{CompletedDeleted: int(completed), InterruptedDeleted: int(audit)}, nil
}

func (store *PostgresStore) conversations() string {
	return quote(store.schema) + "." + quote("conversations")
}
func (store *PostgresStore) turns() string {
	return quote(store.schema) + "." + quote("conversation_turns")
}
func quote(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
