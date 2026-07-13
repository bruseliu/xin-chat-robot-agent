package voice

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/tts"
)

func TestVoiceSessionStreamsLLMDeltasThroughTTS(t *testing.T) {
	runner := streamingTTSRunner{}
	pipeline := NewPipeline(runner, nil, slog.Default())
	asr := &fakeServerEndpointingASR{sentences: make(chan Transcript, 1), partials: make(chan Transcript)}
	close(asr.partials)
	fake := newFakeTTSStream()
	session, err := NewVoiceSession(pipeline, &fakeVoiceVAD{}, asr, VoiceSessionConfig{UserID: "alice", TTSFactory: func(context.Context) (tts.Stream, error) { return fake, nil }})
	if err != nil {
		t.Fatalf("NewVoiceSession: %v", err)
	}
	defer session.Close()
	asr.sentences <- Transcript{Text: "问题", IsFinal: true}
	close(asr.sentences)

	var types []string
	var pcm []byte
	deadline := time.After(time.Second)
	for {
		select {
		case out, ok := <-session.Outputs():
			if !ok {
				t.Fatal("outputs closed before sentence")
			}
			types = append(types, out.Type)
			if len(out.AudioPCM) > 0 {
				pcm = append(pcm, out.AudioPCM...)
			}
			if out.Type == "sentence" && out.Final {
				if out.Response.Text != "你好" {
					t.Fatalf("response = %q", out.Response.Text)
				}
				if string(pcm) != string([]byte{1, 2, 3, 4}) {
					t.Fatalf("pcm = %v", pcm)
				}
				fake.mu.Lock()
				texts := append([]string(nil), fake.texts...)
				fake.mu.Unlock()
				if len(texts) != 1 || texts[0] != "你好" {
					t.Fatalf("tts texts = %v", texts)
				}
				want := []string{"sentence", "tts_start", "llm_delta", "llm_delta", "tts_audio", "tts_end", "sentence"}
				if len(types) != len(want) {
					t.Fatalf("types = %v", types)
				}
				for i := range want {
					if types[i] != want[i] {
						t.Fatalf("types = %v", types)
					}
				}
				return
			}
		case <-deadline:
			t.Fatalf("timeout; types = %v", types)
		}
	}
}

type streamingTTSRunner struct{}

func (streamingTTSRunner) RunTurn(ctx context.Context, in agent.TurnInput) (agent.TurnOutput, error) {
	return streamingTTSRunner{}.RunTurnStream(ctx, in, nil)
}
func (streamingTTSRunner) RunTurnStream(_ context.Context, _ agent.TurnInput, yield func(agent.StreamEvent) bool) (agent.TurnOutput, error) {
	if yield != nil {
		yield(agent.StreamEvent{Type: agent.StreamEventLLMDelta, TextDelta: "你", Partial: true})
		yield(agent.StreamEvent{Type: agent.StreamEventLLMDelta, TextDelta: "好", Partial: true})
	}
	return agent.TurnOutput{Text: "你好"}, nil
}

type fakeTTSStream struct {
	mu    sync.Mutex
	texts []string
	audio chan tts.AudioChunk
	errs  chan error
}

func newFakeTTSStream() *fakeTTSStream {
	return &fakeTTSStream{audio: make(chan tts.AudioChunk, 2), errs: make(chan error)}
}
func (f *fakeTTSStream) WriteText(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return nil
}
func (f *fakeTTSStream) Finish(context.Context) error {
	f.audio <- tts.AudioChunk{PCM: []byte{1, 2, 3, 4}}
	f.audio <- tts.AudioChunk{Final: true}
	close(f.audio)
	close(f.errs)
	return nil
}
func (f *fakeTTSStream) Cancel(context.Context) error { return nil }
func (f *fakeTTSStream) Audio() <-chan tts.AudioChunk { return f.audio }
func (f *fakeTTSStream) Errors() <-chan error         { return f.errs }
func (f *fakeTTSStream) SampleRate() int              { return 16000 }
func (f *fakeTTSStream) Close() error                 { return nil }
