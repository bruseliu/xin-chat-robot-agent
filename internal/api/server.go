package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/vad"
	"xin-chat-robot-agent/internal/voice"
)

type Config struct {
	Addr       string
	SampleRate int
}

type VADFactory func() (vad.AudioProcessor, error)

type ASRFactory func(ctx context.Context) (voice.StreamingASR, error)

type SessionCloseFunc func(ctx context.Context, userID, sessionID string)

type Server struct {
	httpServer     *http.Server
	pipeline       *voice.Pipeline
	logger         *slog.Logger
	upgrader       websocket.Upgrader
	vadFactory     VADFactory
	asrFactory     ASRFactory
	ttsFactory     voice.TTSFactory
	sampleRate     int
	onSessionClose SessionCloseFunc
}

type ServerOption func(*Server)

func WithSessionClose(fn SessionCloseFunc) ServerOption {
	return func(s *Server) { s.onSessionClose = fn }
}

func WithTTSFactory(f voice.TTSFactory) ServerOption {
	return func(s *Server) { s.ttsFactory = f }
}

func NewServer(cfg Config, pipeline *voice.Pipeline, logger *slog.Logger, vadFactory VADFactory, asrFactory ASRFactory, opts ...ServerOption) *Server {
	if cfg.SampleRate == 0 {
		cfg.SampleRate = vad.DefaultSampleRate
	}
	server := &Server{
		pipeline:   pipeline,
		logger:     logger,
		upgrader:   websocket.Upgrader{},
		vadFactory: vadFactory,
		asrFactory: asrFactory,
		sampleRate: cfg.SampleRate,
	}
	for _, opt := range opts {
		opt(server)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.handleHealth)
	mux.HandleFunc("POST /v1/chat/text", server.handleTextTurn)
	mux.HandleFunc("GET /v1/chat/voice", server.handleVoiceStream)

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

func (s *Server) handleVoiceStream(w http.ResponseWriter, r *http.Request) {
	if s.vadFactory == nil || s.asrFactory == nil {
		writeError(w, http.StatusServiceUnavailable, "voice streaming is not configured")
		return
	}
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		writeError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	sampleRate := s.sampleRate
	if raw := r.URL.Query().Get("sample_rate"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "sample_rate must be a positive integer")
			return
		}
		sampleRate = parsed
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Warn("voice websocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	s.logger.Info("voice websocket connected", "user_id", userID, "session_id", sessionID, "sample_rate", sampleRate, "remote_addr", r.RemoteAddr)

	// Write pump: serializes all writes through one goroutine.
	// Pong has a dedicated channel with priority over data frames.
	type wsMsg struct {
		msgType int
		data    []byte
	}
	writeCh := make(chan wsMsg, 256)
	pongCh := make(chan wsMsg, 4)
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for {
			// Drain pong with priority
			select {
			case msg := <-pongCh:
				if msg.msgType == 0 {
					continue
				}
				if err := conn.WriteMessage(msg.msgType, msg.data); err != nil {
					return
				}
				continue
			default:
			}
			select {
			case msg := <-pongCh:
				if msg.msgType == 0 {
					continue
				}
				if err := conn.WriteMessage(msg.msgType, msg.data); err != nil {
					return
				}
			case msg, ok := <-writeCh:
				if !ok {
					return
				}
				if err := conn.WriteMessage(msg.msgType, msg.data); err != nil {
					return
				}
			}
		}
	}()
	conn.SetPingHandler(func(appData string) error {
		select {
		case pongCh <- wsMsg{msgType: websocket.PongMessage, data: []byte(appData)}:
		default:
		}
		return nil
	})

	closeSession := func() {}

	processor, err := s.vadFactory()
	if err != nil {
		closeSession()
		writeWSError(conn, "vad_create_failed", err)
		return
	}
	session, err := voice.NewVoiceSessionWithASRFactory(r.Context(), s.pipeline, processor, voice.ASRFactory(s.asrFactory), voice.VoiceSessionConfig{
		UserID:    userID,
		SessionID: sessionID,
		Metadata: map[string]string{
			"transport": "websocket",
		},
		TTSFactory: s.ttsFactory,
	})
	if err != nil {
		if processor != nil {
			_ = processor.Close()
		}
		closeSession()
		writeWSError(conn, "voice_session_create_failed", err)
		return
	}
	outputs := session.Outputs()
	var writerDone chan struct{}
	if outputs != nil {
		writerDone = make(chan struct{})
		go func() {
			defer close(writerDone)
			for output := range outputs {
				var msg wsMsg
				if len(output.AudioPCM) > 0 {
					msg = wsMsg{msgType: websocket.BinaryMessage, data: output.AudioPCM}
				} else {
					data, err := json.Marshal(output)
					if err != nil {
						return
					}
					msg = wsMsg{msgType: websocket.TextMessage, data: data}
				}
				select {
				case writeCh <- msg:
				case <-writeDone:
					return
				}
			}
		}()
	}
	closeSession = func() {
		_ = session.Close()
		if writerDone != nil {
			<-writerDone
			writerDone = nil
		}
		select {
		case <-writeDone:
		default:
			close(writeCh)
			close(pongCh)
			<-writeDone
		}
	}
	defer closeSession()

	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			s.logger.Info("voice websocket closed", "user_id", userID, "session_id", sessionID, "error", err)
			if s.onSessionClose != nil {
				s.onSessionClose(r.Context(), userID, sessionID)
			}
			return
		}
		switch messageType {
		case websocket.BinaryMessage:
			out, err := session.ProcessAudio(r.Context(), voice.AudioChunk{PCM16LE: payload, SampleRate: sampleRate})
			if err != nil {
				closeSession()
				writeWSError(conn, "voice_process_failed", err)
				return
			}
			if outputs == nil && out.Final {
				if err := conn.WriteJSON(out); err != nil {
					return
				}
			}
		case websocket.TextMessage:
			var msg voiceControlMessage
			if err := json.Unmarshal(payload, &msg); err != nil {
				closeSession()
				writeWSError(conn, "invalid_control_message", err)
				return
			}
			if msg.Type != "final" {
				closeSession()
				writeWSError(conn, "unsupported_control_message", fmt.Errorf("unsupported control type %q", msg.Type))
				return
			}
			out, err := session.ProcessAudio(r.Context(), voice.AudioChunk{SampleRate: sampleRate, Final: true})
			if err != nil {
				closeSession()
				writeWSError(conn, "voice_finalize_failed", err)
				return
			}
			if outputs == nil && out.Final {
				if err := conn.WriteJSON(out); err != nil {
					return
				}
			}
		}
	}
}

type voiceControlMessage struct {
	Type string `json:"type"`
}

func writeWSError(conn *websocket.Conn, code string, err error) {
	_ = conn.WriteJSON(map[string]string{
		"error":  code,
		"detail": err.Error(),
	})
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
