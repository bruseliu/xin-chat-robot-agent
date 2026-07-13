package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/vad"
)

const defaultFunASRURL = "ws://127.0.0.1:10095"

// FunASRASRConfig configures a private Fun-ASR-Nano real-time WebSocket service.
type FunASRASRConfig struct {
	URL             string
	SampleRate      int
	DialTimeout     time.Duration
	WriteTimeout    time.Duration
	ReadTimeout     time.Duration
	WebSocketDialer *websocket.Dialer
}

// FunASRASR keeps one private Fun-ASR session open for the whole microphone stream.
type FunASRASR struct {
	cfg    FunASRASRConfig
	conn   *websocket.Conn
	logger *slog.Logger

	writeMu  sync.Mutex
	stateMu  sync.Mutex
	closed   bool
	stopping bool
	latest   Transcript

	sentences chan Transcript
	partials  chan Transcript
	completed chan error
	seen      map[string]struct{}
}

var _ StreamingASR = (*FunASRASR)(nil)
var _ ServerEndpointingASR = (*FunASRASR)(nil)

func NewFunASRASR(ctx context.Context, cfg FunASRASRConfig, logger *slog.Logger) (*FunASRASR, error) {
	cfg = normalizeFunASRASRConfig(cfg)
	if err := validateFunASRASRConfig(cfg); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "funasr_asr", "url", cfg.URL)
	dialer := cfg.WebSocketDialer
	if dialer == nil {
		base := *websocket.DefaultDialer
		base.HandshakeTimeout = cfg.DialTimeout
		dialer = &base
	}
	logger.Info("connecting to funasr")
	conn, _, err := dialer.DialContext(ctx, cfg.URL, nil)
	if err != nil {
		logger.Error("funasr dial failed", "error", err)
		return nil, fmt.Errorf("funasr websocket dial: %w", err)
	}
	stream := &FunASRASR{
		cfg:       cfg,
		conn:      conn,
		logger:    logger,
		sentences: make(chan Transcript, 32),
		partials:  make(chan Transcript, 16),
		completed: make(chan error, 1),
		seen:      make(map[string]struct{}),
	}
	if err := stream.writeMessage(ctx, websocket.TextMessage, []byte("START")); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := stream.waitStarted(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	logger.Info("funasr session started")
	go stream.readLoop()
	return stream, nil
}

func normalizeFunASRASRConfig(cfg FunASRASRConfig) FunASRASRConfig {
	if cfg.URL == "" {
		cfg.URL = defaultFunASRURL
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = vad.DefaultSampleRate
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	return cfg
}

func validateFunASRASRConfig(cfg FunASRASRConfig) error {
	endpoint, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("parse funasr url: %w", err)
	}
	if endpoint.Scheme != "ws" && endpoint.Scheme != "wss" {
		return fmt.Errorf("funasr url must use ws or wss")
	}
	if cfg.SampleRate != 16000 {
		return fmt.Errorf("funasr real-time service requires 16000 Hz PCM")
	}
	return nil
}

func (s *FunASRASR) Write(ctx context.Context, chunk vad.Chunk) error {
	if s == nil {
		return fmt.Errorf("nil funasr asr")
	}
	if chunk.SampleRate != 0 && chunk.SampleRate != s.cfg.SampleRate {
		return fmt.Errorf("funasr sample rate changed from %d to %d", s.cfg.SampleRate, chunk.SampleRate)
	}
	audio, err := audioChunkPCM16LE(chunk)
	if err != nil {
		return err
	}
	if len(audio) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.isClosed() {
		return fmt.Errorf("funasr stream is closed")
	}
	if s.isStopping() {
		return fmt.Errorf("funasr stream is stopping")
	}
	return s.writeMessage(ctx, websocket.BinaryMessage, audio)
}

func (s *FunASRASR) SentenceTranscripts() <-chan Transcript {
	if s == nil {
		return nil
	}
	return s.sentences
}

func (s *FunASRASR) PartialTranscripts() <-chan Transcript {
	if s == nil {
		return nil
	}
	return s.partials
}

// NotifySegmentEnd tells the server that local VAD detected end-of-speech.
// The server force-decodes the current segment and returns a sentence without
// closing the connection.
func (s *FunASRASR) NotifySegmentEnd(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("nil funasr asr")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.isClosed() {
		return fmt.Errorf("funasr stream is closed")
	}
	if s.isStopping() {
		return fmt.Errorf("funasr stream is stopping")
	}
	s.logger.Info("sending SEGMENT_END to funasr")
	return s.writeMessage(ctx, websocket.TextMessage, []byte("SEGMENT_END"))
}

func (s *FunASRASR) Finalize(ctx context.Context) (Transcript, error) {
	if s == nil {
		return Transcript{}, fmt.Errorf("nil funasr asr")
	}
	s.writeMu.Lock()
	if s.isClosed() {
		s.writeMu.Unlock()
		return Transcript{}, fmt.Errorf("funasr stream is closed")
	}
	if !s.isStopping() {
		s.setStopping()
		if err := s.writeMessage(ctx, websocket.TextMessage, []byte("STOP")); err != nil {
			s.writeMu.Unlock()
			return Transcript{}, err
		}
	}
	s.writeMu.Unlock()

	if err := s.waitCompleted(ctx); err != nil {
		return Transcript{}, err
	}
	return s.latestTranscript(), nil
}

func (s *FunASRASR) Reset() error {
	return fmt.Errorf("funasr reset requires creating a new stream")
}

func (s *FunASRASR) Close() error {
	if s == nil {
		return nil
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil
	}
	s.closed = true
	s.stateMu.Unlock()
	return s.conn.Close()
}

func (s *FunASRASR) waitStarted(ctx context.Context) error {
	deadline, cancel := contextDeadline(ctx, s.cfg.ReadTimeout)
	defer cancel()
	if !deadline.IsZero() {
		_ = s.conn.SetReadDeadline(deadline)
	}
	messageType, data, err := s.conn.ReadMessage()
	_ = s.conn.SetReadDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("read funasr start response: %w", err)
	}
	if messageType != websocket.TextMessage {
		return fmt.Errorf("unexpected funasr start response message type %d", messageType)
	}
	response, err := parseFunASRResponse(data)
	if err != nil {
		return err
	}
	if response.Event != "started" {
		return fmt.Errorf("unexpected funasr start event %q", response.Event)
	}
	return nil
}

func (s *FunASRASR) readLoop() {
	defer close(s.sentences)
	defer close(s.partials)
	for {
		messageType, data, err := s.conn.ReadMessage()
		if err != nil {
			if !s.isClosed() {
				s.logger.Error("funasr read error", "error", err)
				s.publishCompleted(fmt.Errorf("read funasr response: %w", err))
			}
			return
		}
		if messageType != websocket.TextMessage {
			s.publishCompleted(fmt.Errorf("unexpected funasr response message type %d", messageType))
			return
		}
		response, err := parseFunASRResponse(data)
		if err != nil {
			s.publishCompleted(err)
			return
		}
		if response.Error != "" {
			s.publishCompleted(fmt.Errorf("funasr error: %s", response.Error))
			return
		}
		for _, sentence := range response.Sentences {
			s.publishSentence(sentence)
		}
		if response.Partial != "" {
			s.publishPartial(response.Partial)
		}
		if response.Event == "stopped" || response.IsFinal {
			s.publishCompleted(nil)
			if response.Event == "stopped" {
				return
			}
		}
	}
}

func (s *FunASRASR) publishSentence(sentence funASRSentence) {
	text := strings.TrimSpace(sentence.Text)
	if text == "" {
		return
	}
	key := fmt.Sprintf("%d:%d:%s", sentence.Start, sentence.End, text)
	if _, seen := s.seen[key]; seen {
		return
	}
	s.seen[key] = struct{}{}
	s.logger.Info("funasr sentence", "text", text, "start_ms", sentence.Start, "end_ms", sentence.End)
	transcript := Transcript{Text: text, IsFinal: true}
	s.updateLatest(transcript)
	select {
	case s.sentences <- transcript:
	default:
	}
}

func (s *FunASRASR) publishPartial(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	transcript := Transcript{Text: text, IsPartial: true}
	select {
	case s.partials <- transcript:
	default:
	}
}

func (s *FunASRASR) waitCompleted(ctx context.Context) error {
	timer := time.NewTimer(s.cfg.ReadTimeout)
	defer timer.Stop()
	select {
	case err := <-s.completed:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("funasr completion timeout after %s", s.cfg.ReadTimeout)
	}
}

func (s *FunASRASR) writeMessage(ctx context.Context, messageType int, data []byte) error {
	deadline, cancel := contextDeadline(ctx, s.cfg.WriteTimeout)
	defer cancel()
	if !deadline.IsZero() {
		_ = s.conn.SetWriteDeadline(deadline)
	}
	if err := s.conn.WriteMessage(messageType, data); err != nil {
		return fmt.Errorf("write funasr websocket message: %w", err)
	}
	return nil
}

func (s *FunASRASR) updateLatest(transcript Transcript) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if transcript.Text != "" {
		s.latest.Text = transcript.Text
	}
	s.latest.IsFinal = transcript.IsFinal
}

func (s *FunASRASR) latestTranscript() Transcript {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.latest
}

func (s *FunASRASR) isClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closed
}

func (s *FunASRASR) isStopping() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.stopping
}

func (s *FunASRASR) setStopping() {
	s.stateMu.Lock()
	s.stopping = true
	s.stateMu.Unlock()
}

func (s *FunASRASR) publishCompleted(err error) {
	select {
	case s.completed <- err:
	default:
	}
}

type funASRResponse struct {
	Event     string           `json:"event"`
	Sentences []funASRSentence `json:"sentences"`
	Partial   string           `json:"partial"`
	IsFinal   bool             `json:"is_final"`
	Error     string           `json:"error"`
	// segment_end event is handled like a normal decode response —
	// sentences in the payload are published normally.
}

type funASRSentence struct {
	Text  string `json:"text"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

func parseFunASRResponse(data []byte) (funASRResponse, error) {
	var response funASRResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return funASRResponse{}, fmt.Errorf("decode funasr response: %w", err)
	}
	return response, nil
}
