package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/vad"
	"xin-chat-robot-agent/internal/vision"
	"xin-chat-robot-agent/internal/voice"
)

func TestVoiceWebSocketRunsVoiceSession(t *testing.T) {
	pipeline := voice.NewPipeline(fakeAPIRunner{}, nil, slog.Default())
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

func TestVoiceWebSocketPassesCameraFrameToTurn(t *testing.T) {
	runner := &apiCameraFrameRunner{}
	pipeline := voice.NewPipeline(runner, nil, slog.Default())
	server := NewServer(Config{SampleRate: 16000, Vision: vision.Limits{Enabled: true, MaxFrameBytes: 1024, MaxWidth: 32, MaxHeight: 32}}, pipeline, slog.Default(),
		func() (vad.AudioProcessor, error) {
			return &apiFakeVAD{out: vad.AudioOutput{Chunk: vad.Chunk{PCM16LE: []byte{1, 0}, SampleRate: 16000}, VADEnd: true}}, nil
		},
		func(context.Context) (voice.StreamingASR, error) {
			return &apiFakeASR{transcript: voice.Transcript{Text: "画面里有什么"}}, nil
		},
	)
	httpServer := httptest.NewServer(server.httpServer.Handler)
	defer httpServer.Close()

	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/chat/voice?user_id=alice&session_id=s1&sample_rate=16000"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]string{"type": "video_frame", "mime_type": "image/jpeg", "data": apiTestJPEGBase64(t)}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	var response voice.VoiceTurnOutput
	if err := conn.ReadJSON(&response); err != nil {
		t.Fatal(err)
	}
	if !response.Final || runner.input.Frame == nil {
		t.Fatalf("response/frame = %+v/%+v, want completed turn with frame", response, runner.input.Frame)
	}
	if runner.input.Frame.Width != 2 || runner.input.Frame.Height != 1 {
		t.Fatalf("frame dimensions = %dx%d, want 2x1", runner.input.Frame.Width, runner.input.Frame.Height)
	}
}

func apiTestJPEGBase64(t *testing.T) string {
	t.Helper()
	imageData := image.NewRGBA(image.Rect(0, 0, 2, 1))
	imageData.Set(0, 0, color.RGBA{R: 255, A: 255})
	var output bytes.Buffer
	if err := jpeg.Encode(&output, imageData, nil); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(output.Bytes())
}

type apiCameraFrameRunner struct {
	input agent.TurnInput
}

func (r *apiCameraFrameRunner) RunTurn(ctx context.Context, input agent.TurnInput) (agent.TurnOutput, error) {
	return r.RunTurnStream(ctx, input, nil)
}

func (r *apiCameraFrameRunner) RunTurnStream(_ context.Context, input agent.TurnInput, yield func(agent.StreamEvent) bool) (agent.TurnOutput, error) {
	r.input = input
	text := "agent: " + input.Text
	if yield != nil {
		yield(agent.StreamEvent{Type: agent.StreamEventLLMFinal, Text: text, Final: true})
	}
	return agent.TurnOutput{Text: text}, nil
}

func TestDiscardQueuedVoiceMedia(t *testing.T) {
	media := make(chan voiceWSMessage, 3)
	media <- voiceWSMessage{msgType: websocket.BinaryMessage, data: []byte{1}}
	media <- voiceWSMessage{msgType: websocket.BinaryMessage, data: []byte{2}}
	media <- voiceWSMessage{msgType: websocket.TextMessage, data: []byte(`{"type":"tts_end"}`)}

	if got := discardQueuedVoiceMedia(media); got != 3 {
		t.Fatalf("discardQueuedVoiceMedia() = %d, want 3", got)
	}
	if len(media) != 0 {
		t.Fatalf("media queue length = %d, want 0", len(media))
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
