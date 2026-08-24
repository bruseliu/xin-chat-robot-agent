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
	conversationcontext "xin-chat-robot-agent/internal/conversation/context"
	conversationpostgres "xin-chat-robot-agent/internal/conversation/postgres"
	"xin-chat-robot-agent/internal/conversation/retention"
	"xin-chat-robot-agent/internal/memory"
	"xin-chat-robot-agent/internal/sessionstore"
	"xin-chat-robot-agent/internal/vad"
	"xin-chat-robot-agent/internal/vision"
	"xin-chat-robot-agent/internal/voice"

	adksession "google.golang.org/adk/v2/session"
)

func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	memClient := memory.NewMem0Client(memory.Mem0Config{
		BaseURL:      cfg.Mem0.BaseURL,
		APIKey:       cfg.Mem0.APIKey,
		Mode:         cfg.Mem0.Mode,
		TopK:         cfg.Mem0.TopK,
		Timeout:      cfg.Mem0.Timeout,
		WriteTimeout: cfg.Mem0.WriteTimeout,
	}, logger)

	if cfg.Session.PostgresDSN == "" {
		logger.Error("ADK_SESSION_POSTGRES_DSN is required for persistent sessions")
		os.Exit(1)
	}
	postgresSessionService, err := sessionstore.NewPostgresService(sessionstore.PostgresConfig{
		DSN:         cfg.Session.PostgresDSN,
		AutoMigrate: cfg.Session.AutoMigrate,
	})
	if err != nil {
		logger.Error("create PostgreSQL ADK session service failed", "error", err)
		os.Exit(1)
	}
	managedSessionService, err := sessionstore.NewManagedService(postgresSessionService, sessionstore.Config{
		TTL:       cfg.Session.TTL,
		MaxEvents: cfg.Session.MaxEvents,
		MaxTurns:  cfg.Session.MaxTurns,
	})
	if err != nil {
		logger.Error("create managed ADK session service failed", "error", err)
		os.Exit(1)
	}
	var sessionService adksession.Service = managedSessionService
	startSessionCleanup(ctx, managedSessionService, cfg.AgentID, cfg.Session.CleanupInterval, logger)
	adkMemoryService := memory.NewADKService(memClient, cfg.AgentID, cfg.Mem0.TopK)
	conversationStore, err := conversationpostgres.New(conversationpostgres.Config{
		DSN:         cfg.Session.PostgresDSN,
		AutoMigrate: cfg.Session.AutoMigrate,
	})
	if err != nil {
		logger.Error("create conversation PostgreSQL store failed", "error", err)
		os.Exit(1)
	}
	conversationReader, err := conversationcontext.New(conversationStore, conversationcontext.Config{})
	if err != nil {
		logger.Error("create conversation context reader failed", "error", err)
		os.Exit(1)
	}
	startConversationRetention(ctx, cfg.Retention, cfg.Session.PostgresDSN, logger)

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

	rootAgent, err := newLLMAgent(context.Background(), cfg.LLM, cfg.AgentID, memClient, conversationReader, logger)
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

	compactionTrigger, err := startConversationCompaction(ctx, cfg.LLM, conversationStore, logger)
	if err != nil {
		logger.Error("start conversation compaction failed", "error", err)
		os.Exit(1)
	}
	memWriter := memory.NewWriter(memClient, memory.WriterConfig{}, logger)
	pipeline := voice.NewPipeline(agentRunner, memWriter, logger, voice.WithAppName(cfg.AgentID), voice.WithTurnRecorder(conversationStore), voice.WithCompactionTrigger(compactionTrigger))
	// Barge-in also needs the client to keep sending microphone audio while TTS
	// plays, so log the server's half to make a mismatch obvious.
	logger.Info("barge-in config", "enabled", cfg.BargeIn.Enabled, "drain_timeout", cfg.BargeIn.DrainTimeout)
	ttsFactory, err := newTTSFactory(cfg.TTS)
	if err != nil {
		logger.Error("create TTS factory failed", "error", err)
		os.Exit(1)
	}
	server := api.NewServer(api.Config{
		Addr:       cfg.Addr,
		SampleRate: cfg.VAD.SampleRate,
		Vision: vision.Limits{
			Enabled:       cfg.Vision.Enabled,
			MaxFrameBytes: cfg.Vision.MaxFrameBytes,
			MaxWidth:      cfg.Vision.MaxWidth,
			MaxHeight:     cfg.Vision.MaxHeight,
			MaxAge:        cfg.Vision.MaxAge,
		},
	}, pipeline, logger,
		func() (vad.AudioProcessor, error) {
			return newVADDetector(cfg.VAD)
		},
		func(ctx context.Context) (voice.StreamingASR, error) {
			return newASRStream(ctx, cfg.ASR, logger)
		},
		api.WithTTSFactory(ttsFactory),
		api.WithBargeIn(voice.BargeInConfig{
			Enabled:      cfg.BargeIn.Enabled,
			DrainTimeout: cfg.BargeIn.DrainTimeout,
		}),
		api.WithSessionClose(func(ctx context.Context, userID, sessionID string) {
			memWriter.Flush(userID)
			logger.Info("session retained on ws close", "user_id", userID, "session_id", sessionID)
		}),
	)

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

func startConversationRetention(ctx context.Context, cfg config.RetentionConfig, dsn string, logger *slog.Logger) {
	if !cfg.Enabled {
		logger.Info("conversation retention disabled")
		return
	}

	store, err := retention.NewPostgresStore(dsn, "")
	if err != nil {
		logger.Error("create conversation retention store failed", "error", err)
		return
	}
	worker, err := retention.New(store, retention.Config{
		Policy: retention.Policy{
			RawTTL:     cfg.RawTTL,
			SummaryTTL: cfg.SummaryTTL,
			BatchSize:  cfg.BatchSize,
			DryRun:     cfg.DryRun,
		},
		Interval: cfg.CleanupInterval,
		Logger:   logger,
	})
	if err != nil {
		logger.Error("create conversation retention worker failed", "error", err)
		return
	}
	logger.Info("conversation retention started", "dry_run", cfg.DryRun, "interval", cfg.CleanupInterval, "raw_ttl", cfg.RawTTL, "summary_ttl", cfg.SummaryTTL)
	go worker.Run(ctx)
}

func startSessionCleanup(ctx context.Context, service *sessionstore.ManagedService, appName string, interval time.Duration, logger *slog.Logger) {
	if service == nil || interval <= 0 {
		return
	}

	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		deleted, err := service.DeleteExpired(cleanupCtx, appName, "")
		if err != nil {
			logger.Warn("delete expired sessions failed", "error", err)
			return
		}
		if deleted > 0 {
			logger.Info("expired sessions deleted", "count", deleted)
		}
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanup()
			}
		}
	}()
}
