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

func TestDoubaoASRStreamingLifecycle(t *testing.T) {
	upgrader := websocket.Upgrader{}
	seen := make(chan []doubaoPacketResponse, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-App-Key") != "app" {
			t.Errorf("X-Api-App-Key = %q, want app", r.Header.Get("X-Api-App-Key"))
		}
		if r.Header.Get("X-Api-Access-Key") != "access" {
			t.Errorf("X-Api-Access-Key = %q, want access", r.Header.Get("X-Api-Access-Key"))
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		var packets []doubaoPacketResponse
		for i := 0; i < 3; i++ {
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("ReadMessage() error = %v", err)
				return
			}
			packet, err := doubaoParsePacket(data)
			if err != nil {
				t.Errorf("doubaoParsePacket() error = %v", err)
				return
			}
			packets = append(packets, packet)
		}
		partialBody, _ := json.Marshal(map[string]any{
			"result": map[string]any{
				"text":       "你好",
				"confidence": 0.5,
			},
		})
		partial, err := doubaoBuildPacket(doubaoMsgFullServerResp, doubaoFlagPositiveSequence, doubaoSerializationJSON, doubaoCompressionGzip, 1, partialBody)
		if err != nil {
			t.Errorf("doubaoBuildPacket(partial) error = %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, partial); err != nil {
			t.Errorf("WriteMessage(partial) error = %v", err)
			return
		}
		finalBody, _ := json.Marshal(map[string]any{
			"result": map[string]any{
				"text":       "你好世界",
				"confidence": 0.98,
			},
			"is_final": true,
		})
		finalResp, err := doubaoBuildPacket(doubaoMsgFullServerResp, doubaoFlagNegativeSequence, doubaoSerializationJSON, doubaoCompressionGzip, -2, finalBody)
		if err != nil {
			t.Errorf("doubaoBuildPacket(final) error = %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, finalResp); err != nil {
			t.Errorf("WriteMessage(final) error = %v", err)
			return
		}
		seen <- packets
	}))
	defer server.Close()

	stream, err := NewDoubaoASR(context.Background(), DoubaoASRConfig{
		URL:          "ws" + strings.TrimPrefix(server.URL, "http"),
		AppKey:       "app",
		AccessKey:    "access",
		ResourceID:   "resource",
		ConnectID:    "connect-1",
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		DialTimeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDoubaoASR() error = %v", err)
	}
	defer stream.Close()

	if err := stream.Write(context.Background(), vad.Chunk{Samples: []float32{0.5, -0.5}, SampleRate: 16000}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	transcript, err := stream.Finalize(context.Background())
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if transcript.Text != "你好世界" || !transcript.IsFinal {
		t.Fatalf("transcript = %+v, want final text", transcript)
	}
	if transcript.Confidence != 0.98 {
		t.Fatalf("confidence = %v, want 0.98", transcript.Confidence)
	}
	partial := <-stream.Transcripts()
	if partial.Text != "你好" || partial.IsFinal {
		t.Fatalf("partial transcript = %+v, want non-final 你好", partial)
	}
	finalEvent := <-stream.Transcripts()
	if finalEvent.Text != "你好世界" || !finalEvent.IsFinal {
		t.Fatalf("final transcript event = %+v, want final 你好世界", finalEvent)
	}

	packets := <-seen
	if got := packets[0].MessageType; got != doubaoMsgFullClientRequest {
		t.Fatalf("initial message type = %d, want full client request", got)
	}
	if got := packets[1].MessageType; got != doubaoMsgAudioOnlyRequest {
		t.Fatalf("audio message type = %d, want audio only", got)
	}
	if got := packets[1].Sequence; got != 2 {
		t.Fatalf("audio sequence = %d, want 2", got)
	}
	if got := packets[2].Sequence; got != -3 {
		t.Fatalf("final sequence = %d, want -3", got)
	}
	if packets[1].IsFinal {
		t.Fatal("audio packet IsFinal = true, want false")
	}
	if !packets[2].IsFinal {
		t.Fatal("final packet IsFinal = false, want true")
	}
}

func TestAudioChunkPCM16LEConvertsFloatSamples(t *testing.T) {
	pcm, err := audioChunkPCM16LE(vad.Chunk{Samples: []float32{-1, 0, 1}})
	if err != nil {
		t.Fatalf("audioChunkPCM16LE() error = %v", err)
	}
	want := []byte{0x00, 0x80, 0x00, 0x00, 0xff, 0x7f}
	if string(pcm) != string(want) {
		t.Fatalf("pcm = %v, want %v", pcm, want)
	}
}
