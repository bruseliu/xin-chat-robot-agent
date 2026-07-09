package api

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/memory"
	"xin-chat-robot-agent/internal/vad"
	"xin-chat-robot-agent/internal/voice"
)

func TestVoiceWebSocketRunsVoiceSession(t *testing.T) {
	pipeline := voice.NewPipeline(fakeAPIRunner{}, fakeAPIMemory{}, slog.Default())
	server := NewServer(Config{SampleRate: 16000}, pipeline, slog.Default(),
		func() (vad.AudioProcessor, error) {
			return &apiFakeVAD{out: vad.AudioOutput{Chunk: vad.Chunk{PCM16LE: []byte{1, 0}, SampleRate: 16000}, VADEnd: true}}, nil
		},
		func(context.Context) (voice.StreamingASR, error) {
			return &apiFakeASR{transcript: voice.Transcript{Text: "你好"}}, nil
		},
	)
	httpServer := httptest.NewServer(server.httpServer.Handler)
	defer httpServer.Close()

	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/chat/voice?user_id=alice&session_id=s1&sample_rate=16000"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 0}); err != nil {
		t.Fatalf("WriteMessage() error = %v", err)
	}
	var resp voice.VoiceTurnOutput
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("ReadJSON() error = %v", err)
	}
	if !resp.Final {
		t.Fatal("Final = false, want true")
	}
	if resp.Transcript.Text != "你好" {
		t.Fatalf("transcript = %q, want 你好", resp.Transcript.Text)
	}
	if resp.Response.Text != "agent: 你好" {
		t.Fatalf("response = %q, want agent response", resp.Response.Text)
	}
}

type apiFakeVAD struct {
	out vad.AudioOutput
}

func (f *apiFakeVAD) Process(context.Context, vad.Chunk) (vad.AudioOutput, error) { return f.out, nil }
func (f *apiFakeVAD) Reset() error                                                { return nil }
func (f *apiFakeVAD) Close() error                                                { return nil }

type apiFakeASR struct {
	transcript voice.Transcript
}

func (f *apiFakeASR) Write(context.Context, vad.Chunk) error { return nil }
func (f *apiFakeASR) Finalize(context.Context) (voice.Transcript, error) {
	return f.transcript, nil
}
func (f *apiFakeASR) Reset() error { return nil }
func (f *apiFakeASR) Close() error { return nil }

type fakeAPIRunner struct{}

func (fakeAPIRunner) RunTurn(ctx context.Context, input agent.TurnInput) (agent.TurnOutput, error) {
	return fakeAPIRunner{}.RunTurnStream(ctx, input, nil)
}

func (fakeAPIRunner) RunTurnStream(_ context.Context, input agent.TurnInput, yield func(agent.StreamEvent) bool) (agent.TurnOutput, error) {
	text := "agent: " + input.Text
	if yield != nil {
		yield(agent.StreamEvent{Type: agent.StreamEventLLMFinal, Text: text, Final: true})
	}
	return agent.TurnOutput{Text: text}, nil
}

type fakeAPIMemory struct{}

func (fakeAPIMemory) Search(context.Context, memory.SearchRequest) ([]memory.Memory, error) {
	return nil, nil
}

func (fakeAPIMemory) Add(context.Context, memory.AddRequest) error { return nil }
