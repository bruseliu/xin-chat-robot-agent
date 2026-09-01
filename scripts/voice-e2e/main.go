package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultWAVPath = "internal/vad/testdata/test2.wav"
	sampleRate     = 16000
	chunkSize      = 6400
	chunkInterval  = 200 * time.Millisecond
	maxWait        = 90 * time.Second
	settlePeriod   = time.Second
)

type voiceMessage struct {
	Type       string `json:"type"`
	Transcript struct {
		Text string `json:"text"`
	} `json:"transcript"`
	Response struct {
		Text string `json:"text"`
	} `json:"response"`
	Final bool   `json:"final"`
	Error string `json:"error"`
}

type metrics struct {
	mu sync.Mutex

	startedAt     time.Time
	lastEventAt   time.Time
	lastTTSEndAt  time.Time
	firstASRFinal time.Time
	firstLLMDelta time.Time
	firstTTSStart time.Time
	firstTTSAudio time.Time
	asrFinals     int
	llmDeltas     int
	ttsStarts     int
	ttsEnds       int
	interrupts    int
	ttsPCMBytes   int
	errors        []string
}

func main() {
	if os.Getenv("VOICE_E2E_REAL_TEST") != "1" {
		fatalf("refusing to call real services; rerun with VOICE_E2E_REAL_TEST=1")
	}

	wavPath := defaultWAVPath
	if len(os.Args) > 1 {
		wavPath = os.Args[1]
	}
	pcm, err := readPCM16LE(wavPath)
	if err != nil {
		fatalf("read WAV: %v", err)
	}

	endpoint, err := voiceEndpoint()
	if err != nil {
		fatalf("voice endpoint: %v", err)
	}
	fmt.Printf("[e2e] audio=%s, duration=%.1fs\n", wavPath, float64(len(pcm))/2/sampleRate)
	fmt.Printf("[e2e] connecting to %s\n", endpoint)

	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(context.Background(), endpoint, nil)
	if err != nil {
		fatalf("dial voice service: %v", err)
	}
	defer conn.Close()

	m := &metrics{startedAt: time.Now()}
	readErr := make(chan error, 1)
	go readOutputs(conn, m, readErr)

	for offset := 0; offset < len(pcm); offset += chunkSize {
		end := offset + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[offset:end]); err != nil {
			fatalf("send PCM: %v", err)
		}
		time.Sleep(chunkInterval)
	}
	sentAt := time.Now()
	fmt.Printf("[e2e] audio upload complete after %s; waiting for final TTS...\n", elapsed(m.startedAt, sentAt))

	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-readErr:
			if err != nil {
				m.addError(err.Error())
				m.printSummary(sentAt)
				fatalf("read voice output: %v", err)
			}
		case <-ticker.C:
			if m.settledAfter(sentAt) {
				m.printSummary(sentAt)
				m.validate()
				return
			}
		case <-deadline.C:
			m.addError("timed out waiting for final TTS")
			m.printSummary(sentAt)
			fatalf("timed out after %s", maxWait)
		}
	}
}

func voiceEndpoint() (string, error) {
	base := envString("VOICE_E2E_URL", "ws://127.0.0.1:8080/v1/chat/voice")
	endpoint, err := url.Parse(base)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return "", fmt.Errorf("invalid VOICE_E2E_URL %q", base)
	}
	id := fmt.Sprintf("voice-e2e-%d", time.Now().UnixNano())
	query := endpoint.Query()
	query.Set("user_id", envString("VOICE_E2E_USER_ID", id))
	query.Set("session_id", envString("VOICE_E2E_SESSION_ID", id))
	query.Set("sample_rate", fmt.Sprint(sampleRate))
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

func readPCM16LE(path string) ([]byte, error) {
	wav, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(wav) <= 44 {
		return nil, fmt.Errorf("WAV is too short: %d bytes", len(wav))
	}
	return wav[44:], nil
}

func readOutputs(conn *websocket.Conn, m *metrics, result chan<- error) {
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			result <- err
			return
		}
		at := time.Now()
		if messageType == websocket.BinaryMessage {
			m.recordTTSAudio(at, len(payload))
			continue
		}
		var msg voiceMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			result <- fmt.Errorf("decode voice event: %w", err)
			return
		}
		m.recordMessage(at, msg)
	}
}

func (m *metrics) recordMessage(at time.Time, msg voiceMessage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastEventAt = at
	switch msg.Type {
	case "partial":
		if msg.Transcript.Text != "" {
			fmt.Printf("[+%s] ASR partial: %s\n", elapsed(m.startedAt, at), msg.Transcript.Text)
		}
	case "sentence":
		if !msg.Final {
			m.asrFinals++
			if m.firstASRFinal.IsZero() {
				m.firstASRFinal = at
			}
			fmt.Printf("[+%s] ASR final #%d: %s\n", elapsed(m.startedAt, at), m.asrFinals, msg.Transcript.Text)
		}
	case "llm_delta":
		if msg.Response.Text != "" {
			m.llmDeltas++
			if m.firstLLMDelta.IsZero() {
				m.firstLLMDelta = at
				fmt.Printf("[+%s] first LLM delta: %q\n", elapsed(m.startedAt, at), msg.Response.Text)
			}
		}
	case "tts_start":
		m.ttsStarts++
		if m.firstTTSStart.IsZero() {
			m.firstTTSStart = at
		}
		fmt.Printf("[+%s] TTS start #%d\n", elapsed(m.startedAt, at), m.ttsStarts)
	case "tts_end":
		m.ttsEnds++
		m.lastTTSEndAt = at
		fmt.Printf("[+%s] TTS end #%d\n", elapsed(m.startedAt, at), m.ttsEnds)
	case "interrupt":
		m.interrupts++
		fmt.Printf("[+%s] interrupt #%d: %s\n", elapsed(m.startedAt, at), m.interrupts, msg.Error)
	case "tts_error", "turn_error":
		m.errors = append(m.errors, msg.Type+": "+msg.Error)
		fmt.Printf("[+%s] %s: %s\n", elapsed(m.startedAt, at), msg.Type, msg.Error)
	}
}

func (m *metrics) recordTTSAudio(at time.Time, bytes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastEventAt = at
	m.ttsPCMBytes += bytes
	if m.firstTTSAudio.IsZero() {
		m.firstTTSAudio = at
		fmt.Printf("[+%s] first TTS PCM: %d bytes\n", elapsed(m.startedAt, at), bytes)
	}
}

func (m *metrics) settledAfter(audioSentAt time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.asrFinals > 0 && m.llmDeltas > 0 && m.ttsStarts > 0 && m.ttsPCMBytes > 0 &&
		!m.lastTTSEndAt.Before(audioSentAt) && time.Since(m.lastEventAt) >= settlePeriod
}

func (m *metrics) validate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.asrFinals == 0 || m.llmDeltas == 0 || m.ttsStarts == 0 || m.ttsPCMBytes == 0 || m.ttsEnds == 0 || len(m.errors) > 0 {
		fatalf("incomplete voice E2E result")
	}
}

func (m *metrics) addError(value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors = append(m.errors, value)
}

func (m *metrics) printSummary(audioSentAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Println("\n[e2e] summary")
	fmt.Printf("  ASR finals: %d, LLM deltas: %d, TTS starts/ends: %d/%d, interrupts: %d\n", m.asrFinals, m.llmDeltas, m.ttsStarts, m.ttsEnds, m.interrupts)
	fmt.Printf("  TTS PCM: %d bytes\n", m.ttsPCMBytes)
	fmt.Printf("  audio start -> first ASR final: %s\n", elapsed(m.startedAt, m.firstASRFinal))
	fmt.Printf("  first ASR final -> first LLM delta: %s\n", elapsed(m.firstASRFinal, m.firstLLMDelta))
	fmt.Printf("  first LLM delta -> first TTS PCM: %s\n", elapsed(m.firstLLMDelta, m.firstTTSAudio))
	fmt.Printf("  audio upload complete -> final TTS end: %s\n", elapsed(audioSentAt, m.lastTTSEndAt))
	if len(m.errors) > 0 {
		fmt.Printf("  errors: %v\n", m.errors)
	}
}

func elapsed(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return "n/a"
	}
	return end.Sub(start).Round(time.Millisecond).String()
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[e2e] "+format+"\n", args...)
	os.Exit(1)
}
