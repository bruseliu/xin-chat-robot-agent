// Package context builds bounded, provider-independent model context.
package context

import (
	stdcontext "context"
	"fmt"

	"xin-chat-robot-agent/internal/conversation"
)

const (
	defaultMaxRecentTurns = 12
	defaultMaxTextUnits   = 16000
)

// Config defines default bounds when a caller does not provide explicit limits.
type Config struct {
	MaxRecentTurns int
	MaxTextUnits   int
}

// Builder wraps a ContextReader and enforces a deterministic text budget.
type Builder struct {
	reader conversation.ContextReader
	config Config
}

var _ conversation.ContextReader = (*Builder)(nil)

func New(reader conversation.ContextReader, config Config) (*Builder, error) {
	if reader == nil {
		return nil, fmt.Errorf("conversation context reader is required")
	}
	if config.MaxRecentTurns <= 0 {
		config.MaxRecentTurns = defaultMaxRecentTurns
	}
	if config.MaxTextUnits <= 0 {
		config.MaxTextUnits = defaultMaxTextUnits
	}
	return &Builder{reader: reader, config: config}, nil
}

// LoadContext reads a bounded turn window and then enforces its text budget.
func (b *Builder) LoadContext(ctx stdcontext.Context, appName, userID, conversationID string, limits conversation.ContextLimits) (conversation.Context, error) {
	if limits.MaxRecentTurns <= 0 {
		limits.MaxRecentTurns = b.config.MaxRecentTurns
	}
	if limits.MaxTextUnits <= 0 {
		limits.MaxTextUnits = b.config.MaxTextUnits
	}
	loaded, err := b.reader.LoadContext(ctx, appName, userID, conversationID, limits)
	if err != nil {
		return conversation.Context{}, err
	}
	return fit(loaded, limits.MaxTextUnits), nil
}
