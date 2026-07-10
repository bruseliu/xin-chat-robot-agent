package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/vad"
)

const (
	defaultAliyunNLSURL = "wss://nls-gateway.cn-shanghai.aliyuncs.com/ws/v1"
	aliyunNLSStatusOK   = 20000000
)

// AliyunNLSASRConfig configures the SpeechTranscriber WebSocket protocol.
type AliyunNLSASRConfig struct {
	URL                             string
	Token                           string
	AppKey                          string
	SampleRate                      int
	EnableIntermediateResult        bool
	EnablePunctuation               bool
	EnableInverseTextNormalization  bool
	EnableSemanticSentenceDetection bool
	MaxSentenceSilenceMs            int
	DialTimeout                     time.Duration
	WriteTimeout                    time.Duration
	ReadTimeout                     time.Duration
	WebSocketDialer                 *websocket.Dialer
}

// AliyunNLSASR keeps one ASR task open across multiple provider-detected sentences.
type AliyunNLSASR struct {
	cfg    AliyunNLSASRConfig
	conn   *websocket.Conn
	taskID string

	writeMu  sync.Mutex
	stateMu  sync.Mutex
	closed   bool
	stopping bool
	latest   Transcript

	sentences chan Transcript
	partials  chan Transcript
	completed chan error
}

var _ StreamingASR = (*AliyunNLSASR)(nil)
var _ ServerEndpointingASR = (*AliyunNLSASR)(nil)

func NewAliyunNLSASR(ctx context.Context, cfg AliyunNLSASRConfig) (*AliyunNLSASR, error) {
	cfg = normalizeAliyunNLSASRConfig(cfg)
	if err := validateAliyunNLSASRConfig(cfg); err != nil {
		return nil, err
	}
	endpoint, err := aliyunNLSEndpoint(cfg.URL, cfg.Token)
	if err != nil {
		return nil, err
	}
	dialer := cfg.WebSocketDialer
	if dialer == nil {
		base := *websocket.DefaultDialer
		base.HandshakeTimeout = cfg.DialTimeout
		dialer = &base
	}
	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("aliyun nls websocket dial: %w", err)
	}
	stream := &AliyunNLSASR{
		cfg:       cfg,
		conn:      conn,
		taskID:    newAliyunNLSID(),
		sentences: make(chan Transcript, 32),
		partials:  make(chan Transcript, 16),
		completed: make(chan error, 1),
	}
	if err := stream.sendStart(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := stream.waitStarted(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go stream.readLoop()
	return stream, nil
}

func normalizeAliyunNLSASRConfig(cfg AliyunNLSASRConfig) AliyunNLSASRConfig {
	if cfg.URL == "" {
		cfg.URL = defaultAliyunNLSURL
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = vad.DefaultSampleRate
	}
	if cfg.MaxSentenceSilenceMs == 0 {
		cfg.MaxSentenceSilenceMs = 800
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

func validateAliyunNLSASRConfig(cfg AliyunNLSASRConfig) error {
	if strings.TrimSpace(cfg.Token) == "" {
		return fmt.Errorf("aliyun nls token is required")
	}
	if strings.TrimSpace(cfg.AppKey) == "" {
		return fmt.Errorf("aliyun nls app key is required")
	}
	if cfg.SampleRate != 8000 && cfg.SampleRate != 16000 {
		return fmt.Errorf("aliyun nls sample rate must be 8000 or 16000")
	}
	if cfg.MaxSentenceSilenceMs < 200 || cfg.MaxSentenceSilenceMs > 2000 {
		return fmt.Errorf("aliyun nls max sentence silence must be between 200 and 2000 ms")
	}
	return nil
}

func aliyunNLSEndpoint(rawURL, token string) (string, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse aliyun nls url: %w", err)
	}
	if endpoint.Scheme != "ws" && endpoint.Scheme != "wss" {
		return "", fmt.Errorf("aliyun nls url must use ws or wss")
	}
	query := endpoint.Query()
	query.Set("token", token)
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func (s *AliyunNLSASR) Write(ctx context.Context, chunk vad.Chunk) error {
	if s == nil {
		return fmt.Errorf("nil aliyun nls asr")
	}
	if chunk.SampleRate != 0 && chunk.SampleRate != s.cfg.SampleRate {
		return fmt.Errorf("aliyun nls sample rate changed from %d to %d", s.cfg.SampleRate, chunk.SampleRate)
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
		return fmt.Errorf("aliyun nls asr stream is closed")
	}
	if s.isStopping() {
		return fmt.Errorf("aliyun nls asr stream is stopping")
	}
	return s.writeMessage(ctx, websocket.BinaryMessage, audio)
}

// SentenceTranscripts emits provider-confirmed utterances from SentenceEnd events.
func (s *AliyunNLSASR) SentenceTranscripts() <-chan Transcript {
	if s == nil {
		return nil
	}
	return s.sentences
}

func (s *AliyunNLSASR) PartialTranscripts() <-chan Transcript {
	if s == nil {
		return nil
	}
	return s.partials
}

// Finalize stops the complete ASR task. It does not create an utterance boundary.
func (s *AliyunNLSASR) Finalize(ctx context.Context) (Transcript, error) {
	if s == nil {
		return Transcript{}, fmt.Errorf("nil aliyun nls asr")
	}
	s.writeMu.Lock()
	if s.isClosed() {
		s.writeMu.Unlock()
		return Transcript{}, fmt.Errorf("aliyun nls asr stream is closed")
	}
	if !s.isStopping() {
		s.setStopping()
		if err := s.writeCommand(ctx, "StopTranscription", nil); err != nil {
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

func (s *AliyunNLSASR) Reset() error {
	return fmt.Errorf("aliyun nls asr reset requires creating a new stream")
}

func (s *AliyunNLSASR) Close() error {
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

func (s *AliyunNLSASR) sendStart(ctx context.Context) error {
	payload := aliyunNLSStartPayload{
		Format:                          "pcm",
		SampleRate:                      s.cfg.SampleRate,
		EnableIntermediateResult:        s.cfg.EnableIntermediateResult,
		EnablePunctuationPrediction:     s.cfg.EnablePunctuation,
		EnableInverseTextNormalization:  s.cfg.EnableInverseTextNormalization,
		EnableSemanticSentenceDetection: s.cfg.EnableSemanticSentenceDetection,
		MaxSentenceSilence:              s.cfg.MaxSentenceSilenceMs,
	}
	return s.writeCommand(ctx, "StartTranscription", payload)
}

func (s *AliyunNLSASR) writeCommand(ctx context.Context, name string, payload any) error {
	command := aliyunNLSCommand{
		Header: aliyunNLSHeader{
			AppKey:    s.cfg.AppKey,
			MessageID: newAliyunNLSID(),
			TaskID:    s.taskID,
			Namespace: "SpeechTranscriber",
			Name:      name,
		},
		Payload: payload,
	}
	data, err := json.Marshal(command)
	if err != nil {
		return fmt.Errorf("marshal aliyun nls %s: %w", name, err)
	}
	return s.writeMessage(ctx, websocket.TextMessage, data)
}

func (s *AliyunNLSASR) writeMessage(ctx context.Context, messageType int, data []byte) error {
	deadline, cancel := contextDeadline(ctx, s.cfg.WriteTimeout)
	defer cancel()
	if !deadline.IsZero() {
		_ = s.conn.SetWriteDeadline(deadline)
	}
	if err := s.conn.WriteMessage(messageType, data); err != nil {
		return fmt.Errorf("write aliyun nls websocket message: %w", err)
	}
	return nil
}

func (s *AliyunNLSASR) waitStarted(ctx context.Context) error {
	deadline, cancel := contextDeadline(ctx, s.cfg.ReadTimeout)
	defer cancel()
	if !deadline.IsZero() {
		_ = s.conn.SetReadDeadline(deadline)
	}
	messageType, data, err := s.conn.ReadMessage()
	_ = s.conn.SetReadDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("read aliyun nls start response: %w", err)
	}
	if messageType != websocket.TextMessage {
		return fmt.Errorf("unexpected aliyun nls start response message type %d", messageType)
	}
	message, err := parseAliyunNLSMessage(data)
	if err != nil {
		return err
	}
	if err := message.error(); err != nil {
		return err
	}
	if message.Header.Name != "TranscriptionStarted" {
		return fmt.Errorf("unexpected aliyun nls start event %q", message.Header.Name)
	}
	return nil
}

func (s *AliyunNLSASR) readLoop() {
	defer close(s.sentences)
	defer close(s.partials)
	for {
		messageType, data, err := s.conn.ReadMessage()
		if err != nil {
			if !s.isClosed() {
				s.publishCompleted(fmt.Errorf("read aliyun nls response: %w", err))
			}
			return
		}
		if messageType != websocket.TextMessage {
			s.publishCompleted(fmt.Errorf("unexpected aliyun nls response message type %d", messageType))
			return
		}
		message, err := parseAliyunNLSMessage(data)
		if err != nil {
			s.publishCompleted(err)
			return
		}
		if err := message.error(); err != nil {
			s.publishCompleted(err)
			return
		}
		switch message.Header.Name {
		case "TranscriptionResultChanged":
			partial := Transcript{Text: message.Payload.Result, Confidence: message.Payload.Confidence, IsPartial: true}
			s.updateLatest(partial)
			if strings.TrimSpace(partial.Text) != "" {
				select {
				case s.partials <- partial:
				default:
				}
			}
		case "SentenceEnd":
			transcript := Transcript{Text: message.Payload.Result, Confidence: message.Payload.Confidence, IsFinal: true}
			s.updateLatest(transcript)
			if strings.TrimSpace(transcript.Text) != "" {
				select {
				case s.sentences <- transcript:
				default:
				}
			}
		case "TranscriptionCompleted":
			s.publishCompleted(nil)
			return
		}
	}
}

func (s *AliyunNLSASR) waitCompleted(ctx context.Context) error {
	timer := time.NewTimer(s.cfg.ReadTimeout)
	defer timer.Stop()
	select {
	case err := <-s.completed:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("aliyun nls transcription completion timeout after %s", s.cfg.ReadTimeout)
	}
}

func (s *AliyunNLSASR) updateLatest(transcript Transcript) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if transcript.Text != "" {
		s.latest.Text = transcript.Text
	}
	if transcript.Confidence != 0 {
		s.latest.Confidence = transcript.Confidence
	}
	s.latest.IsFinal = transcript.IsFinal
}

func (s *AliyunNLSASR) latestTranscript() Transcript {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.latest
}

func (s *AliyunNLSASR) isClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closed
}

func (s *AliyunNLSASR) isStopping() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.stopping
}

func (s *AliyunNLSASR) setStopping() {
	s.stateMu.Lock()
	s.stopping = true
	s.stateMu.Unlock()
}

func (s *AliyunNLSASR) publishCompleted(err error) {
	select {
	case s.completed <- err:
	default:
	}
}

func newAliyunNLSID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

type aliyunNLSCommand struct {
	Header  aliyunNLSHeader `json:"header"`
	Payload any             `json:"payload,omitempty"`
}

type aliyunNLSHeader struct {
	AppKey        string `json:"appkey,omitempty"`
	MessageID     string `json:"message_id"`
	TaskID        string `json:"task_id"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Status        int    `json:"status,omitempty"`
	StatusMessage string `json:"status_message,omitempty"`
}

type aliyunNLSStartPayload struct {
	Format                          string `json:"format"`
	SampleRate                      int    `json:"sample_rate"`
	EnableIntermediateResult        bool   `json:"enable_intermediate_result"`
	EnablePunctuationPrediction     bool   `json:"enable_punctuation_prediction"`
	EnableInverseTextNormalization  bool   `json:"enable_inverse_text_normalization"`
	EnableSemanticSentenceDetection bool   `json:"enable_semantic_sentence_detection"`
	MaxSentenceSilence              int    `json:"max_sentence_silence"`
}

type aliyunNLSMessage struct {
	Header  aliyunNLSHeader  `json:"header"`
	Payload aliyunNLSPayload `json:"payload"`
}

type aliyunNLSPayload struct {
	Result     string  `json:"result"`
	Confidence float64 `json:"confidence"`
}

func parseAliyunNLSMessage(data []byte) (aliyunNLSMessage, error) {
	var message aliyunNLSMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return aliyunNLSMessage{}, fmt.Errorf("decode aliyun nls response: %w", err)
	}
	if message.Header.Name == "" {
		return aliyunNLSMessage{}, fmt.Errorf("aliyun nls response has no event name")
	}
	return message, nil
}

func (m aliyunNLSMessage) error() error {
	if m.Header.Status == 0 || m.Header.Status == aliyunNLSStatusOK {
		return nil
	}
	message := strings.TrimSpace(m.Header.StatusMessage)
	if message == "" {
		message = "unknown error"
	}
	return fmt.Errorf("aliyun nls %s failed with status %d: %s", m.Header.Name, m.Header.Status, message)
}
