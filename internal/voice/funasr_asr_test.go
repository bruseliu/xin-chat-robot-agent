package voice

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/vad"
)

func TestFunASRASRStreamingLifecycle(t *testing.T) {
	upgrader := websocket.Upgrader{}
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage(START) error = %v", err)
			return
		}
		if messageType != websocket.TextMessage || string(data) != "START" {
			t.Errorf("start = type %d %q, want text START", messageType, data)
			return
		}
		if err := conn.WriteJSON(map[string]any{"event": "started"}); err != nil {
			t.Errorf("WriteJSON(started) error = %v", err)
			return
		}

		messageType, data, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage(audio) error = %v", err)
			return
		}
		if messageType != websocket.BinaryMessage || string(data) != string([]byte{1, 0, 2, 0}) {
			t.Errorf("audio = type %d %v", messageType, data)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"sentences": []map[string]any{{"text": "第一句", "start": 0, "end": 800}},
			"partial":   "第二",
			"is_final":  false,
		}); err != nil {
			t.Errorf("WriteJSON(sentence) error = %v", err)
			return
		}

		messageType, data, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage(STOP) error = %v", err)
			return
		}
		if messageType != websocket.TextMessage || string(data) != "STOP" {
			t.Errorf("stop = type %d %q, want text STOP", messageType, data)
			return
		}
		if err := conn.WriteJSON(map[string]any{
			"sentences": []map[string]any{{"text": "第二句", "start": 0, "end": 600}},
			"is_final":  true,
		}); err != nil {
			t.Errorf("WriteJSON(final) error = %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"event": "stopped"}); err != nil {
			t.Errorf("WriteJSON(stopped) error = %v", err)
		}
	}))
	defer server.Close()

	stream, err := NewFunASRASR(context.Background(), FunASRASRConfig{
		URL:          "ws" + strings.TrimPrefix(server.URL, "http"),
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		DialTimeout:  time.Second,
	}, nil)
	if err != nil {
		t.Fatalf("NewFunASRASR() error = %v", err)
	}
	defer stream.Close()

	if err := stream.Write(context.Background(), vad.Chunk{PCM16LE: []byte{1, 0, 2, 0}, SampleRate: 16000}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if transcript := mustReceiveFunASRSentence(t, stream.SentenceTranscripts()); transcript.Text != "第一句" || !transcript.IsFinal {
		t.Fatalf("first sentence = %+v", transcript)
	}

	transcript, err := stream.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if transcript.Text != "第二句" || !transcript.IsFinal {
		t.Fatalf("Finalize() transcript = %+v", transcript)
	}
	if transcript := mustReceiveFunASRSentence(t, stream.SentenceTranscripts()); transcript.Text != "第二句" || !transcript.IsFinal {
		t.Fatalf("final sentence = %+v", transcript)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("test server did not complete")
	}
}

func TestFunASRASRPublishesRepeatedSegmentTimes(t *testing.T) {
	stream := &FunASRASR{
		logger:    slog.Default(),
		sentences: make(chan Transcript, 2),
	}
	sentence := funASRSentence{Text: "你好", Start: 0, End: 800}
	stream.publishSentence(sentence)
	stream.publishSentence(sentence)

	for i := 0; i < 2; i++ {
		if transcript := mustReceiveFunASRSentence(t, stream.SentenceTranscripts()); transcript.Text != "你好" || !transcript.IsFinal {
			t.Fatalf("sentence %d = %+v", i+1, transcript)
		}
	}
}

func TestFunASRASRRequires16K(t *testing.T) {
	if _, err := NewFunASRASR(context.Background(), FunASRASRConfig{SampleRate: 8000}, nil); err == nil {
		t.Fatal("NewFunASRASR() error = nil, want invalid sample rate")
	}
}

func mustReceiveFunASRSentence(t *testing.T, transcripts <-chan Transcript) Transcript {
	t.Helper()
	select {
	case transcript := <-transcripts:
		return transcript
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for FunASR sentence")
		return Transcript{}
	}
}
