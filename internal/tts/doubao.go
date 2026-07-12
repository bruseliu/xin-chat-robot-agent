package tts

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const defaultDoubaoURL = "wss://openspeech.bytedance.com/api/v3/tts/bidirection"

const (
	messageFullClient  = 1
	messageFullServer  = 9
	messageAudio       = 11
	messageError       = 15
	flagWithEvent      = 4
	eventStartSession  = 100
	eventCancelSession = 101
	eventFinishSession = 102
	eventTaskRequest   = 200
	eventSessionStart  = 150
	eventSessionDone   = 152
	eventSessionFailed = 153
	eventTTSResponse   = 352
)

type DoubaoConfig struct {
	URL, AppID, AccessToken, ResourceID, Speaker string
	SampleRate                                   int
	Timeout                                      time.Duration
	Dialer                                       *websocket.Dialer
}

type DoubaoStream struct {
	conn      *websocket.Conn
	cfg       DoubaoConfig
	id        string
	audio     chan AudioChunk
	errs      chan error
	started   chan struct{}
	done      chan struct{}
	writeMu   sync.Mutex
	closeOnce sync.Once
}

type doubaoPayload struct {
	User      map[string]string `json:"user"`
	Event     int               `json:"event"`
	Namespace string            `json:"namespace"`
	ReqParams doubaoReqParams   `json:"req_params"`
}

type doubaoReqParams struct {
	Text        string         `json:"text,omitempty"`
	Speaker     string         `json:"speaker"`
	AudioParams map[string]any `json:"audio_params"`
	Additions   string         `json:"additions,omitempty"`
}

type doubaoFrame struct {
	MessageType byte
	Event       int32
	SessionID   string
	Payload     []byte
	ErrorCode   int32
}

func NewDoubaoStream(ctx context.Context, cfg DoubaoConfig) (*DoubaoStream, error) {
	cfg = normalizeDoubaoConfig(cfg)
	if strings.TrimSpace(cfg.AppID) == "" || strings.TrimSpace(cfg.AccessToken) == "" || strings.TrimSpace(cfg.Speaker) == "" {
		return nil, fmt.Errorf("doubao TTS app id, access token, and speaker are required")
	}
	dialer := cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	headers := http.Header{}
	headers.Set("X-Api-App-Key", cfg.AppID)
	headers.Set("X-Api-Access-Key", cfg.AccessToken)
	headers.Set("X-Api-Resource-Id", cfg.ResourceID)
	headers.Set("X-Api-Connect-Id", uuid.NewString())
	conn, _, err := dialer.DialContext(ctx, cfg.URL, headers)
	if err != nil {
		return nil, fmt.Errorf("dial doubao TTS: %w", err)
	}
	s := &DoubaoStream{conn: conn, cfg: cfg, id: uuid.NewString(), audio: make(chan AudioChunk, 32), errs: make(chan error, 1), started: make(chan struct{}), done: make(chan struct{})}
	go s.readLoop()
	if err := s.send(eventStartSession, ""); err != nil {
		_ = s.Close()
		return nil, err
	}
	timer := time.NewTimer(cfg.Timeout)
	defer timer.Stop()
	select {
	case <-s.started:
		return s, nil
	case err := <-s.errs:
		_ = s.Close()
		return nil, err
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	case <-timer.C:
		_ = s.Close()
		return nil, fmt.Errorf("doubao TTS session start timeout after %s", cfg.Timeout)
	}
}

func normalizeDoubaoConfig(cfg DoubaoConfig) DoubaoConfig {
	if cfg.URL == "" {
		cfg.URL = defaultDoubaoURL
	}
	if cfg.ResourceID == "" {
		cfg.ResourceID = "volc.service_type.10029"
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = 24000
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return cfg
}

func (s *DoubaoStream) WriteText(_ context.Context, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return s.send(eventTaskRequest, text)
}
func (s *DoubaoStream) Finish(context.Context) error { return s.send(eventFinishSession, "") }
func (s *DoubaoStream) Cancel(context.Context) error { return s.send(eventCancelSession, "") }
func (s *DoubaoStream) Audio() <-chan AudioChunk     { return s.audio }
func (s *DoubaoStream) Errors() <-chan error         { return s.errs }
func (s *DoubaoStream) SampleRate() int              { return s.cfg.SampleRate }
func (s *DoubaoStream) Close() error {
	var err error
	s.closeOnce.Do(func() { close(s.done); err = s.conn.Close() })
	return err
}

func (s *DoubaoStream) send(event int32, text string) error {
	payload := doubaoPayload{User: map[string]string{"uid": "voice-agent"}, Event: int(event), Namespace: "BidirectionalTTS", ReqParams: doubaoReqParams{Text: text, Speaker: s.cfg.Speaker, AudioParams: map[string]any{"format": "pcm", "sample_rate": s.cfg.SampleRate}, Additions: "{}"}}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	frame := encodeClientFrame(event, s.id, body)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.cfg.Timeout))
	if err := s.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		return fmt.Errorf("write doubao TTS event %d: %w", event, err)
	}
	return nil
}

func encodeClientFrame(event int32, sessionID string, payload []byte) []byte {
	frame := []byte{0x11, byte(messageFullClient<<4 | flagWithEvent), 0x10, 0}
	frame = appendInt32(frame, event)
	frame = appendBytes(frame, []byte(sessionID))
	return appendBytes(frame, payload)
}
func appendInt32(dst []byte, value int32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(value))
	return append(dst, b[:]...)
}
func appendBytes(dst, value []byte) []byte {
	dst = appendInt32(dst, int32(len(value)))
	return append(dst, value...)
}

func (s *DoubaoStream) readLoop() {
	defer close(s.audio)
	defer close(s.errs)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(s.cfg.Timeout))
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			if !s.isDone() {
				s.publishError(fmt.Errorf("read doubao TTS: %w", err))
			}
			return
		}
		frame, err := parseServerFrame(raw)
		if err != nil {
			s.publishError(err)
			return
		}
		switch frame.Event {
		case eventSessionStart:
			select {
			case <-s.started:
			default:
				close(s.started)
			}
		case eventTTSResponse:
			if frame.MessageType == messageAudio && len(frame.Payload) > 0 {
				s.audio <- AudioChunk{PCM: append([]byte(nil), frame.Payload...)}
			}
		case eventSessionDone:
			s.audio <- AudioChunk{Final: true}
			return
		case eventSessionFailed:
			s.publishError(fmt.Errorf("doubao TTS session failed: %s", string(frame.Payload)))
			return
		}
		if frame.MessageType == messageError {
			s.publishError(fmt.Errorf("doubao TTS error %d: %s", frame.ErrorCode, string(frame.Payload)))
			return
		}
	}
}
func (s *DoubaoStream) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
func (s *DoubaoStream) publishError(err error) {
	select {
	case s.errs <- err:
	default:
	}
}

func parseServerFrame(raw []byte) (doubaoFrame, error) {
	if len(raw) < 8 {
		return doubaoFrame{}, fmt.Errorf("short doubao TTS frame: %d", len(raw))
	}
	frame := doubaoFrame{MessageType: raw[1] >> 4}
	offset := 4
	if frame.MessageType == messageError {
		frame.ErrorCode = int32(binary.BigEndian.Uint32(raw[offset:]))
		offset += 4
		payload, _, err := readSized(raw, offset)
		frame.Payload = payload
		return frame, err
	}
	if raw[1]&0x0f != flagWithEvent {
		return frame, nil
	}
	frame.Event = int32(binary.BigEndian.Uint32(raw[offset:]))
	offset += 4
	session, next, err := readSized(raw, offset)
	if err != nil {
		return doubaoFrame{}, err
	}
	frame.SessionID, offset = string(session), next
	payload, _, err := readSized(raw, offset)
	if err != nil {
		return doubaoFrame{}, err
	}
	frame.Payload = payload
	return frame, nil
}
func readSized(raw []byte, offset int) ([]byte, int, error) {
	if offset+4 > len(raw) {
		return nil, offset, fmt.Errorf("truncated doubao TTS frame")
	}
	size := int(int32(binary.BigEndian.Uint32(raw[offset:])))
	offset += 4
	if size < 0 || offset+size > len(raw) {
		return nil, offset, fmt.Errorf("invalid doubao TTS payload size %d", size)
	}
	return raw[offset : offset+size], offset + size, nil
}
