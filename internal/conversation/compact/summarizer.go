package compact

import (
	"context"

	"xin-chat-robot-agent/internal/conversation"
)

// Summarizer produces a replacement for the prior summary plus old turns.
// Provider-specific prompting and model calls belong outside this package.
type Summarizer interface {
	Summarize(context.Context, string, []conversation.Turn) (string, error)
}
