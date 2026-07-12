package tts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDoubaoStreamLifecycle(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-App-Key") != "app" || r.Header.Get("X-Api-Access-Key") != "token" || r.Header.Get("X-Api-Resource-Id") != "resource" {
			t.Errorf("unexpected auth headers")
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		start := readClientFrame(t, conn)
		if start.Event != eventStartSession {
			t.Errorf("start event = %d", start.Event)
		}
		writeServerFrame(t, conn, messageFullServer, eventSessionStart, start.SessionID, []byte(`{}`))
		task := readClientFrame(t, conn)
		if task.Event != eventTaskRequest {
			t.Errorf("task event = %d", task.Event)
		}
		var payload doubaoPayload
		if err := json.Unmarshal(task.Payload, &payload); err != nil {
			t.Errorf("decode task: %v", err)
		}
		if payload.ReqParams.Text != "你好" || payload.ReqParams.Speaker != "voice" || payload.ReqParams.AudioParams["sample_rate"] != float64(16000) {
			t.Errorf("task payload = %+v", payload)
		}
		finish := readClientFrame(t, conn)
		if finish.Event != eventFinishSession {
			t.Errorf("finish event = %d", finish.Event)
		}
		writeServerFrame(t, conn, messageAudio, eventTTSResponse, start.SessionID, []byte{1, 2, 3, 4})
		writeServerFrame(t, conn, messageFullServer, eventSessionDone, start.SessionID, []byte(`{}`))
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	stream, err := NewDoubaoStream(context.Background(), DoubaoConfig{URL: url, AppID: "app", AccessToken: "token", ResourceID: "resource", Speaker: "voice", SampleRate: 16000, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewDoubaoStream: %v", err)
	}
	defer stream.Close()
	if err := stream.WriteText(context.Background(), "你好"); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if err := stream.Finish(context.Background()); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	first := <-stream.Audio()
	if string(first.PCM) != string([]byte{1, 2, 3, 4}) || first.Final {
		t.Fatalf("audio = %+v", first)
	}
	last := <-stream.Audio()
	if !last.Final {
		t.Fatalf("final = %+v", last)
	}
	if stream.SampleRate() != 16000 {
		t.Fatalf("sample rate = %d", stream.SampleRate())
	}
}

func TestParseDoubaoErrorFrame(t *testing.T) {
	raw := []byte{0x11, byte(messageError << 4), 0x10, 0}
	raw = appendInt32(raw, 401)
	raw = appendBytes(raw, []byte("denied"))
	frame, err := parseServerFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if frame.ErrorCode != 401 || string(frame.Payload) != "denied" {
		t.Fatalf("frame = %+v", frame)
	}
}

func readClientFrame(t *testing.T, conn *websocket.Conn) doubaoFrame {
	t.Helper()
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	frame, err := parseServerFrame(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return frame
}

func writeServerFrame(t *testing.T, conn *websocket.Conn, messageType byte, event int32, sessionID string, payload []byte) {
	t.Helper()
	raw := []byte{0x11, byte(messageType<<4 | flagWithEvent), 0x10, 0}
	raw = appendInt32(raw, event)
	raw = appendBytes(raw, []byte(sessionID))
	raw = appendBytes(raw, payload)
	if err := conn.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}
