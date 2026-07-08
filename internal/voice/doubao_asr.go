package voice

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/vad"
)

const (
	doubaoProtocolVersion = 1
	doubaoHeaderSize      = 1

	doubaoMsgFullClientRequest = 1
	doubaoMsgAudioOnlyRequest  = 2
	doubaoMsgFullServerResp    = 9
	doubaoMsgServerAck         = 11
	doubaoMsgServerError       = 15

	doubaoFlagNoSequence           = 0
	doubaoFlagPositiveSequence     = 1
	doubaoFlagNegativeSequence     = 2
	doubaoFlagNegativeWithSequence = 3

	doubaoSerializationNone = 0
	doubaoSerializationJSON = 1

	doubaoCompressionNone = 0
	doubaoCompressionGzip = 1
)

const defaultDoubaoASRURL = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel"

type DoubaoASRConfig struct {
	URL             string
	AppKey          string
	AccessKey       string
	ResourceID      string
	ConnectID       string
	ModelName       string
	Language        string
	Format          string
	Codec           string
	SampleRate      int
	Bits            int
	Channel         int
	EnablePunc      bool
	CompressAudio   bool
	DialTimeout     time.Duration
	WriteTimeout    time.Duration
	ReadTimeout     time.Duration
	RequestExtra    map[string]any
	WebSocketDialer *websocket.Dialer
}

type DoubaoASR struct {
	cfg      DoubaoASRConfig
	conn     *websocket.Conn
	sequence int32

	writeMu sync.Mutex
	stateMu sync.Mutex
	closed  bool
	latest  Transcript

	finalCh      chan doubaoASRResult
	transcriptCh chan Transcript
}

type doubaoASRResult struct {
	transcript Transcript
	err        error
}

func NewDoubaoASR(ctx context.Context, cfg DoubaoASRConfig) (*DoubaoASR, error) {
	cfg = normalizeDoubaoASRConfig(cfg)
	if err := validateDoubaoASRConfig(cfg); err != nil {
		return nil, err
	}
	dialer := cfg.WebSocketDialer
	if dialer == nil {
		d := websocket.DefaultDialer
		copy := *d
		copy.HandshakeTimeout = cfg.DialTimeout
		dialer = &copy
	}

	header := http.Header{}
	header.Set("X-Api-App-Key", cfg.AppKey)
	header.Set("X-Api-Access-Key", cfg.AccessKey)
	header.Set("X-Api-Resource-Id", cfg.ResourceID)
	header.Set("X-Api-Request-Id", uuid.NewString())
	header.Set("X-Api-Sequence", "-1")

	conn, resp, err := dialer.DialContext(ctx, cfg.URL, header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("doubao asr websocket dial: %w (status %s)", err, resp.Status)
		}
		return nil, fmt.Errorf("doubao asr websocket dial: %w", err)
	}
	stream := &DoubaoASR{cfg: cfg, conn: conn, sequence: 1, finalCh: make(chan doubaoASRResult, 1), transcriptCh: make(chan Transcript, 32)}
	if err := stream.sendInitialRequest(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	go stream.readLoop()
	return stream, nil
}

func normalizeDoubaoASRConfig(cfg DoubaoASRConfig) DoubaoASRConfig {
	if cfg.URL == "" {
		cfg.URL = defaultDoubaoASRURL
	}
	if cfg.ResourceID == "" {
		cfg.ResourceID = "volc.bigasr.sauc.duration"
	}
	if cfg.ConnectID == "" {
		cfg.ConnectID = uuid.NewString()
	}
	if cfg.ModelName == "" {
		cfg.ModelName = "bigmodel"
	}
	if cfg.Language == "" {
		cfg.Language = "zh-CN"
	}
	if cfg.Format == "" {
		cfg.Format = "pcm"
	}
	if cfg.Codec == "" {
		cfg.Codec = "raw"
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = vad.DefaultSampleRate
	}
	if cfg.Bits == 0 {
		cfg.Bits = 16
	}
	if cfg.Channel == 0 {
		cfg.Channel = 1
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

func validateDoubaoASRConfig(cfg DoubaoASRConfig) error {
	if strings.TrimSpace(cfg.AppKey) == "" {
		return fmt.Errorf("doubao asr app key is required")
	}
	if strings.TrimSpace(cfg.AccessKey) == "" {
		return fmt.Errorf("doubao asr access key is required")
	}
	if strings.TrimSpace(cfg.ResourceID) == "" {
		return fmt.Errorf("doubao asr resource id is required")
	}
	if cfg.SampleRate <= 0 {
		return fmt.Errorf("doubao asr sample rate must be positive")
	}
	return nil
}

func (s *DoubaoASR) Write(ctx context.Context, chunk vad.Chunk) error {
	if s == nil {
		return fmt.Errorf("nil doubao asr")
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
		return fmt.Errorf("doubao asr stream is closed")
	}
	s.sequence++
	return s.writeAudioPacketLocked(ctx, s.sequence, false, audio)
}

func (s *DoubaoASR) Transcripts() <-chan Transcript {
	if s == nil {
		return nil
	}
	return s.transcriptCh
}

func (s *DoubaoASR) Finalize(ctx context.Context) (Transcript, error) {
	if s == nil {
		return Transcript{}, fmt.Errorf("nil doubao asr")
	}
	s.writeMu.Lock()
	if s.isClosed() {
		s.writeMu.Unlock()
		return Transcript{}, fmt.Errorf("doubao asr stream is closed")
	}
	s.sequence++
	seq := s.sequence
	if err := s.writeAudioPacketLocked(ctx, seq, true, nil); err != nil {
		s.writeMu.Unlock()
		return Transcript{}, err
	}
	s.writeMu.Unlock()

	return s.waitFinalTranscript(ctx)
}

func (s *DoubaoASR) Reset() error {
	return fmt.Errorf("doubao asr stream reset requires creating a new stream")
}

func (s *DoubaoASR) Close() error {
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

func (s *DoubaoASR) sendInitialRequest(ctx context.Context) error {
	payload := map[string]any{
		"user": map[string]any{
			"uid": s.cfg.ConnectID,
		},
		"audio": map[string]any{
			"format":  s.cfg.Format,
			"codec":   s.cfg.Codec,
			"rate":    s.cfg.SampleRate,
			"bits":    s.cfg.Bits,
			"channel": s.cfg.Channel,
		},
		"request": map[string]any{
			"model_name":  s.cfg.ModelName,
			"language":    s.cfg.Language,
			"enable_punc": s.cfg.EnablePunc,
		},
	}
	for k, v := range s.cfg.RequestExtra {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal doubao asr request: %w", err)
	}
	packet, err := doubaoBuildPacket(doubaoMsgFullClientRequest, doubaoFlagNoSequence, doubaoSerializationJSON, doubaoCompressionGzip, 0, raw)
	if err != nil {
		return err
	}
	return s.writePacket(ctx, packet)
}

func (s *DoubaoASR) writeAudioPacketLocked(ctx context.Context, seq int32, final bool, audio []byte) error {
	flags := doubaoFlagPositiveSequence
	if final {
		flags = doubaoFlagNegativeWithSequence
		seq = -seq
	}
	compression := doubaoCompressionNone
	if s.cfg.CompressAudio {
		compression = doubaoCompressionGzip
	}
	packet, err := doubaoBuildAudioPacket(flags, compression, seq, audio)
	if err != nil {
		return err
	}
	return s.writePacket(ctx, packet)
}

func (s *DoubaoASR) writePacket(ctx context.Context, packet []byte) error {
	deadline, cancel := contextDeadline(ctx, s.cfg.WriteTimeout)
	defer cancel()
	if !deadline.IsZero() {
		_ = s.conn.SetWriteDeadline(deadline)
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, packet)
}

func (s *DoubaoASR) readLoop() {
	defer close(s.transcriptCh)
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if !s.isClosed() {
				s.publishFinal(doubaoASRResult{err: fmt.Errorf("read doubao asr response: %w", err)})
			}
			return
		}
		resp, err := doubaoParsePacket(data)
		if err != nil {
			s.publishFinal(doubaoASRResult{err: err})
			return
		}
		if resp.Error != "" {
			s.publishFinal(doubaoASRResult{err: fmt.Errorf("doubao asr error: %s", resp.Error)})
			return
		}
		if resp.Text != "" {
			transcript := Transcript{
				Text:       resp.Text,
				Confidence: resp.Confidence,
				IsFinal:    resp.IsFinal,
			}
			s.updateLatest(transcript)
			s.publishTranscript(transcript)
		}
		if resp.IsFinal {
			latest := s.latestTranscript()
			if latest.Text == "" {
				continue
			}
			latest.IsFinal = true
			s.publishFinal(doubaoASRResult{transcript: latest})
			return
		}
	}
}

func (s *DoubaoASR) waitFinalTranscript(ctx context.Context) (Transcript, error) {
	if deadline, ok := ctx.Deadline(); ok {
		select {
		case result := <-s.finalCh:
			return result.transcript, result.err
		case <-time.After(time.Until(deadline)):
			return Transcript{}, ctx.Err()
		}
	}
	timer := time.NewTimer(s.cfg.ReadTimeout)
	defer timer.Stop()
	select {
	case result := <-s.finalCh:
		return result.transcript, result.err
	case <-ctx.Done():
		return Transcript{}, ctx.Err()
	case <-timer.C:
		return Transcript{}, fmt.Errorf("doubao asr final transcript timeout after %s", s.cfg.ReadTimeout)
	}
}

func (s *DoubaoASR) updateLatest(transcript Transcript) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if transcript.Text != "" {
		s.latest.Text = transcript.Text
	}
	if transcript.Confidence != 0 {
		s.latest.Confidence = transcript.Confidence
	}
}

func (s *DoubaoASR) latestTranscript() Transcript {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.latest
}

func (s *DoubaoASR) isClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closed
}

func (s *DoubaoASR) publishFinal(result doubaoASRResult) {
	select {
	case s.finalCh <- result:
	default:
	}
}

func (s *DoubaoASR) publishTranscript(transcript Transcript) {
	select {
	case s.transcriptCh <- transcript:
	default:
	}
}

type doubaoPacketResponse struct {
	MessageType int
	Sequence    int32
	Text        string
	Confidence  float64
	IsFinal     bool
	Error       string
	Payload     map[string]any
}

func doubaoBuildAudioPacket(flags, compression int, seq int32, payload []byte) ([]byte, error) {
	encoded := payload
	var err error
	if compression == doubaoCompressionGzip {
		encoded, err = gzipBytes(payload)
		if err != nil {
			return nil, err
		}
	}
	buf := bytes.NewBuffer(make([]byte, 0, 12+len(encoded)))
	buf.WriteByte(byte(doubaoProtocolVersion<<4 | doubaoHeaderSize))
	buf.WriteByte(byte(doubaoMsgAudioOnlyRequest<<4 | flags))
	buf.WriteByte(byte(doubaoSerializationNone<<4 | compression))
	buf.WriteByte(0)
	_ = binary.Write(buf, binary.BigEndian, seq)
	_ = binary.Write(buf, binary.BigEndian, int32(len(encoded)))
	buf.Write(encoded)
	return buf.Bytes(), nil
}

func doubaoBuildPacket(messageType, flags, serialization, compression int, seq int32, payload []byte) ([]byte, error) {
	encoded := payload
	var err error
	if compression == doubaoCompressionGzip {
		encoded, err = gzipBytes(payload)
		if err != nil {
			return nil, err
		}
	}
	buf := bytes.NewBuffer(make([]byte, 0, 12+len(encoded)))
	buf.WriteByte(byte(doubaoProtocolVersion<<4 | doubaoHeaderSize))
	buf.WriteByte(byte(messageType<<4 | flags))
	buf.WriteByte(byte(serialization<<4 | compression))
	buf.WriteByte(0)
	if flags&0x01 != 0 {
		_ = binary.Write(buf, binary.BigEndian, seq)
	}
	_ = binary.Write(buf, binary.BigEndian, int32(len(encoded)))
	buf.Write(encoded)
	return buf.Bytes(), nil
}

func doubaoParsePacket(packet []byte) (doubaoPacketResponse, error) {
	if len(packet) < 8 {
		return doubaoPacketResponse{}, fmt.Errorf("doubao asr response too short")
	}
	headerSize := int(packet[0]&0x0f) * 4
	if headerSize <= 0 || len(packet) < headerSize+4 {
		return doubaoPacketResponse{}, fmt.Errorf("invalid doubao asr response header")
	}
	messageType := int(packet[1] >> 4)
	flags := int(packet[1] & 0x0f)
	serialization := int(packet[2] >> 4)
	compression := int(packet[2] & 0x0f)
	offset := headerSize
	var seq int32
	if flags&0x01 != 0 {
		if len(packet) < offset+4 {
			return doubaoPacketResponse{}, fmt.Errorf("invalid doubao asr response sequence")
		}
		seq = int32(binary.BigEndian.Uint32(packet[offset : offset+4]))
		offset += 4
	}
	resp := doubaoPacketResponse{MessageType: messageType, Sequence: seq, IsFinal: seq < 0 || flags&0x02 != 0}
	if len(packet) == offset {
		return resp, nil
	}
	if messageType == doubaoMsgServerError {
		if len(packet) < offset+8 {
			return doubaoPacketResponse{}, fmt.Errorf("invalid doubao asr error payload: len=%d offset=%d head=%s", len(packet), offset, hex.EncodeToString(packet[:min(len(packet), 48)]))
		}
		errorCode := int32(binary.BigEndian.Uint32(packet[offset : offset+4]))
		offset += 4
		size := int(int32(binary.BigEndian.Uint32(packet[offset : offset+4])))
		offset += 4
		if size < 0 || len(packet) < offset+size {
			return doubaoPacketResponse{}, fmt.Errorf("invalid doubao asr error payload length: code=%d len=%d offset=%d size=%d head=%s", errorCode, len(packet), offset, size, hex.EncodeToString(packet[:min(len(packet), 48)]))
		}
		payload := packet[offset : offset+size]
		if compression == doubaoCompressionGzip {
			var err error
			payload, err = gunzipBytes(payload)
			if err != nil {
				return doubaoPacketResponse{}, err
			}
		}
		resp.Error = fmt.Sprintf("code=%d %s", errorCode, string(payload))
		return resp, nil
	}
	if len(packet) < offset+4 {
		return doubaoPacketResponse{}, fmt.Errorf("invalid doubao asr response payload size: len=%d offset=%d message_type=%d flags=%d header_size=%d", len(packet), offset, messageType, flags, headerSize)
	}
	size := int(int32(binary.BigEndian.Uint32(packet[offset : offset+4])))
	offset += 4
	if size < 0 || len(packet) < offset+size {
		return doubaoPacketResponse{}, fmt.Errorf("invalid doubao asr response payload length: len=%d offset=%d size=%d message_type=%d flags=%d header_size=%d head=%s", len(packet), offset, size, messageType, flags, headerSize, hex.EncodeToString(packet[:min(len(packet), 48)]))
	}
	payload := packet[offset : offset+size]
	if compression == doubaoCompressionGzip {
		var err error
		payload, err = gunzipBytes(payload)
		if err != nil {
			return doubaoPacketResponse{}, err
		}
	}

	if serialization != doubaoSerializationJSON || len(payload) == 0 {
		return resp, nil
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return doubaoPacketResponse{}, fmt.Errorf("decode doubao asr response: %w message_type=%d flags=%d serialization=%d compression=%d payload_head=%s", err, messageType, flags, serialization, compression, hex.EncodeToString(payload[:min(len(payload), 64)]))
	}
	resp.Payload = body
	resp.Text = firstTextValue(body)
	resp.Confidence = firstFloatValue(body, "confidence", "score")
	resp.IsFinal = resp.IsFinal || firstBoolValue(body, "is_final", "final", "definite", "utterance_end")
	if errText := firstStringByKeys(body, "error", "message", "err_msg"); messageType == doubaoMsgServerError && errText != "" {
		resp.Error = errText
	}
	return resp, nil
}

func audioChunkPCM16LE(chunk vad.Chunk) ([]byte, error) {
	if len(chunk.PCM16LE) > 0 {
		if len(chunk.PCM16LE)%2 != 0 {
			return nil, fmt.Errorf("asr pcm16le length must be even")
		}
		return chunk.PCM16LE, nil
	}
	if len(chunk.Samples) == 0 {
		return nil, nil
	}
	out := make([]byte, len(chunk.Samples)*2)
	for i, sample := range chunk.Samples {
		if sample > 1 {
			sample = 1
		} else if sample < -1 {
			sample = -1
		}
		var raw int16
		if sample < 0 {
			raw = int16(math.Round(float64(sample * 32768)))
		} else {
			raw = int16(math.Round(float64(sample * 32767)))
		}
		binary.LittleEndian.PutUint16(out[i*2:], uint16(raw))
	}
	return out, nil
}

func gzipBytes(in []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(in); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(in []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		return nil, fmt.Errorf("gunzip doubao asr payload: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("read gunzip doubao asr payload: %w", err)
	}
	return out, nil
}

func contextDeadline(ctx context.Context, fallback time.Duration) (time.Time, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline, func() {}
	}
	if fallback <= 0 {
		return time.Time{}, func() {}
	}
	deadline := time.Now().Add(fallback)
	return deadline, func() {}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func firstTextValue(value any) string {
	if text := firstStringByKeys(value, "text", "result_text", "utterance_text"); text != "" {
		return text
	}
	return ""
}

func firstStringByKeys(value any, keys ...string) string {
	wanted := map[string]struct{}{}
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	var walk func(any) string
	walk = func(v any) string {
		switch typed := v.(type) {
		case map[string]any:
			for _, key := range keys {
				if raw, ok := typed[key]; ok {
					if s, ok := raw.(string); ok && s != "" {
						return s
					}
				}
			}
			for key, raw := range typed {
				if _, ok := wanted[key]; ok {
					continue
				}
				if got := walk(raw); got != "" {
					return got
				}
			}
		case []any:
			for _, item := range typed {
				if got := walk(item); got != "" {
					return got
				}
			}
		}
		return ""
	}
	return walk(value)
}

func firstBoolValue(value any, keys ...string) bool {
	var walk func(any) bool
	walk = func(v any) bool {
		switch typed := v.(type) {
		case map[string]any:
			for _, key := range keys {
				if raw, ok := typed[key]; ok {
					if b, ok := raw.(bool); ok && b {
						return true
					}
				}
			}
			for _, raw := range typed {
				if walk(raw) {
					return true
				}
			}
		case []any:
			for _, item := range typed {
				if walk(item) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

func firstFloatValue(value any, keys ...string) float64 {
	var walk func(any) float64
	walk = func(v any) float64 {
		switch typed := v.(type) {
		case map[string]any:
			for _, key := range keys {
				if raw, ok := typed[key]; ok {
					switch n := raw.(type) {
					case float64:
						return n
					case float32:
						return float64(n)
					case int:
						return float64(n)
					}
				}
			}
			for _, raw := range typed {
				if got := walk(raw); got != 0 {
					return got
				}
			}
		case []any:
			for _, item := range typed {
				if got := walk(item); got != 0 {
					return got
				}
			}
		}
		return 0
	}
	return walk(value)
}
