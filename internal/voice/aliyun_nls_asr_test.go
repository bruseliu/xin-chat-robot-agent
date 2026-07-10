package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/vad"
)

func TestAliyunNLSASRServerEndpointingLifecycle(t *testing.T) {
	upgrader := websocket.Upgrader{}
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if got := r.URL.Query().Get("token"); got != "token-value" {
			t.Errorf("token = %q, want token-value", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage(start) error = %v", err)
			return
		}
		if messageType != websocket.TextMessage {
			t.Errorf("start message type = %d, want text", messageType)
			return
		}
		var start aliyunNLSCommand
		if err := json.Unmarshal(data, &start); err != nil {
			t.Errorf("Unmarshal(start) error = %v", err)
			return
		}
		if start.Header.Name != "StartTranscription" || start.Header.Namespace != "SpeechTranscriber" {
			t.Errorf("start header = %+v", start.Header)
			return
		}
		if start.Header.AppKey != "app-key" || len(start.Header.TaskID) != 32 {
			t.Errorf("start appkey/task id = %q/%q", start.Header.AppKey, start.Header.TaskID)
			return
		}
		if err := writeAliyunNLSTestEvent(conn, "TranscriptionStarted", nil); err != nil {
			t.Errorf("WriteJSON(TranscriptionStarted) error = %v", err)
			return
		}

		messageType, audio, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage(audio) error = %v", err)
			return
		}
		if messageType != websocket.BinaryMessage || string(audio) != string([]byte{1, 0, 2, 0}) {
			t.Errorf("audio = type %d %v", messageType, audio)
			return
		}
		if err := writeAliyunNLSTestEvent(conn, "TranscriptionResultChanged", map[string]any{"result": "你好", "confidence": 0.5}); err != nil {
			t.Errorf("WriteJSON(TranscriptionResultChanged) error = %v", err)
			return
		}
		if err := writeAliyunNLSTestEvent(conn, "SentenceEnd", map[string]any{"result": "你好世界", "confidence": 0.98}); err != nil {
			t.Errorf("WriteJSON(SentenceEnd) error = %v", err)
			return
		}

		messageType, data, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("ReadMessage(stop) error = %v", err)
			return
		}
		if messageType != websocket.TextMessage {
			t.Errorf("stop message type = %d, want text", messageType)
			return
		}
		var stop aliyunNLSCommand
		if err := json.Unmarshal(data, &stop); err != nil {
			t.Errorf("Unmarshal(stop) error = %v", err)
			return
		}
		if stop.Header.Name != "StopTranscription" || stop.Header.TaskID != start.Header.TaskID {
			t.Errorf("stop header = %+v, start task id = %q", stop.Header, start.Header.TaskID)
			return
		}
		if err := writeAliyunNLSTestEvent(conn, "TranscriptionCompleted", nil); err != nil {
			t.Errorf("WriteJSON(TranscriptionCompleted) error = %v", err)
		}
	}))
	defer server.Close()

	stream, err := NewAliyunNLSASR(context.Background(), AliyunNLSASRConfig{
		URL:          "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:        "token-value",
		AppKey:       "app-key",
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		DialTimeout:  time.Second,
	})
	if err != nil {
		t.Fatalf("NewAliyunNLSASR() error = %v", err)
	}
	defer stream.Close()

	if err := stream.Write(context.Background(), vad.Chunk{PCM16LE: []byte{1, 0, 2, 0}, SampleRate: 16000}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	select {
	case transcript := <-stream.SentenceTranscripts():
		if transcript.Text != "你好世界" || !transcript.IsFinal || transcript.Confidence != 0.98 {
			t.Fatalf("SentenceEnd transcript = %+v", transcript)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SentenceEnd transcript")
	}

	transcript, err := stream.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if transcript.Text != "你好世界" || !transcript.IsFinal {
		t.Fatalf("Finalize() transcript = %+v", transcript)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("test server did not complete")
	}
}

func TestAliyunNLSASRRequiresCredentials(t *testing.T) {
	if _, err := NewAliyunNLSASR(context.Background(), AliyunNLSASRConfig{}); err == nil {
		t.Fatal("NewAliyunNLSASR() error = nil, want missing credential error")
	}
}

func writeAliyunNLSTestEvent(conn *websocket.Conn, name string, payload any) error {
	return conn.WriteJSON(aliyunNLSCommand{
		Header: aliyunNLSHeader{
			Name:          name,
			Namespace:     "SpeechTranscriber",
			Status:        aliyunNLSStatusOK,
			StatusMessage: "success",
		},
		Payload: payload,
	})
}
