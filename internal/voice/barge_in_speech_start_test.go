package voice

import (
	"context"
	"log/slog"
	"testing"

	"xin-chat-robot-agent/internal/tts"
	"xin-chat-robot-agent/internal/vad"
)

func TestSpeechStartInterruptsActiveTurn(t *testing.T) {
	runner := newCancellableRunner()
	pipeline := NewPipeline(runner, nil, slog.Default())
	asr := newSegmentEndASR()
	speech := newCancellableTTS()
	processor := &fakeVoiceVAD{out: vad.AudioOutput{Chunk: vad.Chunk{Samples: []float32{0.1}, SampleRate: 16000}, VADStart: true}}
	session, err := NewVoiceSession(pipeline, processor, asr, VoiceSessionConfig{
		UserID:  "alice",
		BargeIn: BargeInConfig{Enabled: true},
		TTSFactory: func(context.Context) (tts.Stream, error) {
			return speech, nil
		},
	})
	if err != nil {
		t.Fatalf("NewVoiceSession: %v", err)
	}
	defer session.Close()

	asr.sentences <- Transcript{Text: "第一句", IsFinal: true}
	runner.awaitStart(t)
	if _, err := session.ProcessAudio(context.Background(), AudioChunk{Samples: []float32{0.1}, SampleRate: 16000}); err != nil {
		t.Fatalf("ProcessAudio: %v", err)
	}

	types := collectTypes(t, session.Outputs(), func(types []string) bool {
		return contains(types, "interrupt")
	})
	if !contains(types, "interrupt") {
		t.Fatalf("no interrupt emitted, types = %v", types)
	}
	if !speech.cancelled.Load() || !speech.closed.Load() {
		t.Fatalf("TTS not interrupted: cancelled=%v closed=%v", speech.cancelled.Load(), speech.closed.Load())
	}
	calls, cancels := runner.counts()
	if calls != 1 || cancels != 1 {
		t.Fatalf("runner calls/cancels = %d/%d, want 1/1", calls, cancels)
	}
}
