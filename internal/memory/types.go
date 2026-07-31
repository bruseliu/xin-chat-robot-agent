package memory

import "context"

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Memory struct {
	ID        string         `json:"id,omitempty"`
	Text      string         `json:"memory,omitempty"`
	Score     float64        `json:"score,omitempty"`
	UserID    string         `json:"user_id,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt string         `json:"created_at,omitempty"`
	UpdatedAt string         `json:"updated_at,omitempty"`
}

type SearchRequest struct {
	UserID  string
	AgentID string
	RunID   string
	Query   string
	TopK    int
}

type AddRequest struct {
	UserID   string
	AgentID  string
	RunID    string
	Messages []Message
	Metadata map[string]any
	Infer    *bool

	done chan struct{} // internal: signals flush completion
}

type Store interface {
	Search(ctx context.Context, req SearchRequest) ([]Memory, error)
	Add(ctx context.Context, req AddRequest) error
}
