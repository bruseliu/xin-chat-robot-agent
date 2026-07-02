package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"xin-chat-robot-agent/internal/voice"
)

type Config struct {
	Addr string
}

type Server struct {
	httpServer *http.Server
	pipeline   *voice.Pipeline
	logger     *slog.Logger
}

func NewServer(cfg Config, pipeline *voice.Pipeline, logger *slog.Logger) *Server {
	server := &Server{
		pipeline: pipeline,
		logger:   logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.HandleFunc("POST /v1/chat/text", server.handleTextTurn)

	server.httpServer = &http.Server{
		Addr:    cfg.Addr,
		Handler: requestLogger(mux, logger),
	}
	return server
}

func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type textTurnRequest struct {
	UserID    string            `json:"user_id"`
	SessionID string            `json:"session_id"`
	Text      string            `json:"text"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

func (s *Server) handleTextTurn(w http.ResponseWriter, r *http.Request) {
	var req textTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.UserID == "" || req.Text == "" {
		writeError(w, http.StatusBadRequest, "user_id and text are required")
		return
	}

	resp, err := s.pipeline.HandleTextTurn(r.Context(), voice.TextTurnInput{
		UserID:    req.UserID,
		SessionID: req.SessionID,
		Text:      req.Text,
		Metadata:  req.Metadata,
	})
	if err != nil {
		s.logger.Error("text turn failed", "error", err)
		writeError(w, http.StatusInternalServerError, "chat turn failed")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func requestLogger(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
