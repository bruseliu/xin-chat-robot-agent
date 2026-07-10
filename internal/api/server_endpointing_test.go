package api

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/vad"
	"xin-chat-robot-agent/internal/voice"
)

func TestVoiceWebSocketStreamsProviderSentenceEnd(t *testing.T) {
	pipeline := voice.NewPipeline(fakeAPIRunner{}, fakeAPIMemory{}, slog.Default())
	asr := &apiServerEndpointingASR{sentences: make(chan voice.Transcript, 1)}
	server := NewServer(Config{SampleRate: 16000}, pipeline, slog.Default(),
		func() (vad.AudioProcessor, error) {
			return &apiFakeVAD{out: vad.AudioOutput{Chunk: vad.Chunk{PCM16LE: []byte{1, 0}, SampleRate: 16000}}}, nil
		},
		func(context.Context) (voice.StreamingASR, error) {
			return asr, nil
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
	var response voice.VoiceTurnOutput
	if err := conn.ReadJSON(&response); err != nil {
		t.Fatalf("ReadJSON() error = %v", err)
	}
	if response.Type != "sentence" || response.Transcript.Text != "FunASR句末" || response.Response.Text != "agent: FunASR句末" || !response.Final {
		t.Fatalf("response = %+v", response)
	}
}

type apiServerEndpointingASR struct {
	sentences    chan voice.Transcript
	partials     chan voice.Transcript
	partialsOnce sync.Once
	closeOnce    sync.Once
}

func (f *apiServerEndpointingASR) Write(context.Context, vad.Chunk) error {
	select {
	case f.sentences <- voice.Transcript{Text: "FunASR句末", IsFinal: true}:
	default:
	}
	return nil
}

func (*apiServerEndpointingASR) Finalize(context.Context) (voice.Transcript, error) {
	return voice.Transcript{}, nil
}

func (*apiServerEndpointingASR) Reset() error { return nil }

func (f *apiServerEndpointingASR) Close() error {
	f.closeOnce.Do(func() { close(f.sentences) })
	return nil
}

func (f *apiServerEndpointingASR) SentenceTranscripts() <-chan voice.Transcript {
	return f.sentences
}

func (f *apiServerEndpointingASR) PartialTranscripts() <-chan voice.Transcript {
	f.partialsOnce.Do(func() {
		f.partials = make(chan voice.Transcript)
		close(f.partials)
	})
	return f.partials
}
