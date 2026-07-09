package llm

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/model"
)

type Config struct {
	Provider string
	Doubao   DoubaoConfig
}

func NewModel(ctx context.Context, cfg Config) (model.LLM, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "none":
		return nil, nil
	case "doubao":
		return NewDoubaoLLM(ctx, cfg.Doubao)
	default:
		return nil, fmt.Errorf("unsupported LLM provider %q", cfg.Provider)
	}
}
