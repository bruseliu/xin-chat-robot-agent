package postgres

import "time"

type conversationModel struct {
	AppName               string    `gorm:"primaryKey;column:app_name"`
	UserID                string    `gorm:"primaryKey;column:user_id"`
	ConversationID        string    `gorm:"primaryKey;column:conversation_id"`
	NextTurnNo            int64     `gorm:"column:next_turn_no;not null;default:1"`
	SummaryText           string    `gorm:"column:summary_text;type:text;not null;default:''"`
	SummarizedThroughTurn int64     `gorm:"column:summarized_through_turn;not null;default:0"`
	SummaryVersion        int64     `gorm:"column:summary_version;not null;default:0"`
	CreatedAt             time.Time `gorm:"column:created_at;not null"`
	UpdatedAt             time.Time `gorm:"column:updated_at;not null"`
}

type turnModel struct {
	AppName        string     `gorm:"primaryKey;column:app_name"`
	UserID         string     `gorm:"primaryKey;column:user_id"`
	ConversationID string     `gorm:"primaryKey;column:conversation_id"`
	TurnNo         int64      `gorm:"primaryKey;column:turn_no"`
	IdempotencyKey string     `gorm:"column:idempotency_key;not null"`
	Source         string     `gorm:"column:source;not null"`
	UserText       string     `gorm:"column:user_text;type:text;not null"`
	AssistantText  string     `gorm:"column:assistant_text;type:text;not null;default:''"`
	Status         string     `gorm:"column:status;not null"`
	ErrorCode      string     `gorm:"column:error_code;not null;default:''"`
	CreatedAt      time.Time  `gorm:"column:created_at;not null"`
	CompletedAt    *time.Time `gorm:"column:completed_at"`
}
