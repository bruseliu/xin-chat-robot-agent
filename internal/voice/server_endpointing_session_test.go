package voice

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"xin-chat-robot-agent/internal/vad"
)

func TestVoiceSessionUsesProviderSentenceEndWithoutRotatingASR(t *testing.T) {
	runner := &fakeRunner{}
	pipeline := NewPipeline(runner, &fakeMemory{}, slog.Default())
	processor := &fakeVoiceVAD{out: vad.AudioOutput{
		Chunk:  vad.Chunk{Samples: []float32{0.1}, SampleRate: 16000},
		VADEnd: true,
	}}
	asr := &fakeServerEndpointingASR{sentences: make(chan Transcript, 2)}
	session, err := NewVoiceSession(pipeline, processor, asr, VoiceSessionConfig{UserID: "alice", SessionID: "s1"})
	if err != nil {
		t.Fatalf("NewVoiceSession() error = %v", err)
	}
	defer session.Close()

	out, err := session.ProcessAudio(context.Background(), AudioChunk{Samples: []float32{0.1}, SampleRate: 16000})
	if err != nil {
		t.Fatalf("ProcessAudio() error = %v", err)
	}
	if out.Final {
		t.Fatal("ProcessAudio() Final = true, want false before client stream final")
	}
	if asr.writes != 1 || asr.finalizes != 0 {
		t.Fatalf("ASR writes/finalizes = %d/%d, want 1/0", asr.writes, asr.finalizes)
	}

	asr.sentences <- Transcript{Text: "你好", IsFinal: false}
	asr.sentences <- Transcript{Text: "你好世界", IsFinal: true}
	select {
	case result := <-session.Outputs():
		if result.Transcript.Text != "你好世界" || result.Response.Text != "agent: 你好世界" || !result.Final {
			t.Fatalf("provider sentence output = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider sentence output")
	}
	if runner.calls != 1 || runner.last.Text != "你好世界" {
		t.Fatalf("runner calls/text = %d/%q, want 1/你好世界", runner.calls, runner.last.Text)
	}

	close(asr.sentences)
}

type fakeServerEndpointingASR struct {
	sentences chan Transcript
	writes    int
	finalizes int
}

func (f *fakeServerEndpointingASR) Write(context.Context, vad.Chunk) error {
	f.writes++
	return nil
}

func (f *fakeServerEndpointingASR) Finalize(context.Context) (Transcript, error) {
	f.finalizes++
	return Transcript{}, nil
}

func (f *fakeServerEndpointingASR) Reset() error { return nil }
func (f *fakeServerEndpointingASR) Close() error { return nil }

func (f *fakeServerEndpointingASR) SentenceTranscripts() <-chan Transcript {
	return f.sentences
}
