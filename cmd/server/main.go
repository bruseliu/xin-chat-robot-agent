package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/api"
	"xin-chat-robot-agent/internal/config"
	"xin-chat-robot-agent/internal/memory"
	"xin-chat-robot-agent/internal/sessionstore"
	"xin-chat-robot-agent/internal/vad"
	"xin-chat-robot-agent/internal/voice"

	adksession "google.golang.org/adk/v2/session"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	memClient := memory.NewMem0Client(memory.Mem0Config{
		BaseURL: cfg.Mem0.BaseURL,
		APIKey:  cfg.Mem0.APIKey,
		Mode:    cfg.Mem0.Mode,
		TopK:    cfg.Mem0.TopK,
		Timeout: cfg.Mem0.Timeout,
	}, logger)

	var sessionService adksession.Service
	if cfg.Session.PostgresDSN != "" {
		svc, err := sessionstore.NewPostgresService(sessionstore.PostgresConfig{
			DSN:         cfg.Session.PostgresDSN,
			AutoMigrate: cfg.Session.AutoMigrate,
		})
		if err != nil {
			logger.Error("create ADK session service failed", "error", err)
			os.Exit(1)
		}
		sessionService = svc
	} else {
		logger.Warn("ADK_SESSION_POSTGRES_DSN not set, using in-memory session store")
		svc, err := sessionstore.NewInMemoryManagedService(sessionstore.Config{})
		if err != nil {
			logger.Error("create in-memory session service failed", "error", err)
			os.Exit(1)
		}
		sessionService = svc
	}
	adkMemoryService := memory.NewADKService(memClient, cfg.AgentID, cfg.Mem0.TopK)

	graph, err := agent.NewGraph(agent.GraphConfig{AgentID: cfg.AgentID})
	if err != nil {
		logger.Error("create ADK graph failed", "error", err)
		os.Exit(1)
	}
	for _, line := range agent.DescribeGraph(graph.Root) {
		logger.Info("adk graph", "node", line)
	}
	for _, line := range agent.DescribeEdges(graph.Edges) {
		logger.Info("adk graph", "edge", line)
	}

	rootAgent, err := newLLMAgent(context.Background(), cfg.LLM, cfg.AgentID)
	if err != nil {
		logger.Error("create ADK LLM agent failed", "error", err)
		os.Exit(1)
	}

	agentRunner, err := agent.NewADKRunner(agent.Config{
		AgentID:        cfg.AgentID,
		RootAgent:      rootAgent,
		SessionService: sessionService,
		MemoryService:  adkMemoryService,
	}, memClient, logger)
	if err != nil {
		logger.Error("create ADK runner failed", "error", err)
		os.Exit(1)
	}

	pipeline := voice.NewPipeline(agentRunner, memClient, logger)
	ttsFactory, err := newTTSFactory(cfg.TTS)
	if err != nil {
		logger.Error("create TTS factory failed", "error", err)
		os.Exit(1)
	}
	server := api.NewServer(api.Config{
		Addr:       cfg.Addr,
		SampleRate: cfg.VAD.SampleRate,
	}, pipeline, logger,
		func() (vad.AudioProcessor, error) {
			return newVADDetector(cfg.VAD)
		},
		func(ctx context.Context) (voice.StreamingASR, error) {
			return newASRStream(ctx, cfg.ASR, logger)
		},
		ttsFactory,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server starting", "addr", cfg.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown failed", "error", err)
			os.Exit(1)
		}
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}
}
