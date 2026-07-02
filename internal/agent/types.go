package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"xin-chat-robot-agent/internal/memory"
)

type Config struct {
	AgentID string
}

type TurnInput struct {
	UserID    string
	SessionID string
	Text      string
	Memories  []memory.Memory
	Metadata  map[string]string
}

type TurnOutput struct {
	Text string `json:"text"`
}

type Runner interface {
	RunTurn(ctx context.Context, input TurnInput) (TurnOutput, error)
}

type ADKRunner struct {
	cfg     Config
	memory  memory.Store
	logger  *slog.Logger
	planner Planner
}

type Planner interface {
	Plan(ctx context.Context, input TurnInput) (TurnOutput, error)
}

func NewADKRunner(cfg Config, store memory.Store, logger *slog.Logger) *ADKRunner {
	return &ADKRunner{
		cfg:     cfg,
		memory:  store,
		logger:  logger,
		planner: echoPlanner{},
	}
}

func (r *ADKRunner) RunTurn(ctx context.Context, input TurnInput) (TurnOutput, error) {
	if strings.TrimSpace(input.Text) == "" {
		return TurnOutput{}, fmt.Errorf("empty input text")
	}
	r.logger.Info("agent turn", "agent_id", r.cfg.AgentID, "user_id", input.UserID, "memory_count", len(input.Memories))
	return r.planner.Plan(ctx, input)
}

type echoPlanner struct{}

func (echoPlanner) Plan(_ context.Context, input TurnInput) (TurnOutput, error) {
	prefix := "我记住了你的上下文。"
	if len(input.Memories) == 0 {
		prefix = "我还没有检索到相关长期记忆。"
	}
	return TurnOutput{Text: prefix + " 你刚才说：" + input.Text}, nil
}
