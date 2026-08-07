package postgres

import (
	"context"
	"fmt"
)

func (s *Store) Migrate(ctx context.Context) error {
	if err := s.db.WithContext(ctx).Exec("CREATE SCHEMA IF NOT EXISTS " + quoteIdentifier(s.schema)).Error; err != nil {
		return fmt.Errorf("create conversation schema: %w", err)
	}
	if err := s.migrateTable(ctx, "conversations", &conversationModel{}); err != nil {
		return fmt.Errorf("migrate conversations: %w", err)
	}
	if err := s.migrateTable(ctx, "conversation_turns", &turnModel{}); err != nil {
		return fmt.Errorf("migrate conversation turns: %w", err)
	}

	for _, statement := range []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS " + quoteIdentifier("conversation_turns_idempotency_idx") + " ON " + s.turnsTable() + " (app_name, user_id, conversation_id, idempotency_key)",
		"CREATE INDEX IF NOT EXISTS " + quoteIdentifier("conversation_turns_recent_idx") + " ON " + s.turnsTable() + " (app_name, user_id, conversation_id, turn_no DESC)",
		"CREATE INDEX IF NOT EXISTS " + quoteIdentifier("conversation_turns_status_recent_idx") + " ON " + s.turnsTable() + " (app_name, user_id, conversation_id, status, turn_no DESC)",
		"CREATE INDEX IF NOT EXISTS " + quoteIdentifier("conversations_updated_at_idx") + " ON " + s.conversationsTable() + " (updated_at)",
	} {
		if err := s.db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("create conversation index: %w", err)
		}
	}
	return nil
}

// GORM's PostgreSQL table introspection treats a quoted schema-qualified table
// name as one identifier. Check the catalog ourselves so an existing table is
// never passed to AutoMigrate for a second CREATE TABLE attempt.
func (s *Store) migrateTable(ctx context.Context, name string, model any) error {
	var exists bool
	if err := s.db.WithContext(ctx).Raw(
		"SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_tables WHERE schemaname = ? AND tablename = ?)",
		s.schema,
		name,
	).Scan(&exists).Error; err != nil {
		return fmt.Errorf("check table %s: %w", name, err)
	}
	if exists {
		return nil
	}
	var table string
	if name == "conversations" {
		table = s.conversationsTable()
	} else {
		table = s.turnsTable()
	}
	return s.db.WithContext(ctx).Table(table).AutoMigrate(model)
}
