package voice

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"xin-chat-robot-agent/internal/tts"
)

func TestVoiceSessionTTSFactoryFailureDoesNotRetainStream(t *testing.T) {
	pipeline := NewPipeline(streamingTTSRunner{}, nil, slog.Default())
	asr := &fakeServerEndpointingASR{sentences: make(chan Transcript, 1), partials: make(chan Transcript)}
	close(asr.partials)
	session, err := NewVoiceSession(pipeline, &fakeVoiceVAD{}, asr, VoiceSessionConfig{
		UserID: "alice",
		TTSFactory: func(context.Context) (tts.Stream, error) {
			return nil, errors.New("tts dial failed")
		},
	})
	if err != nil {
		t.Fatalf("NewVoiceSession: %v", err)
	}
	defer session.Close()

	asr.sentences <- Transcript{Text: "问题", IsFinal: true}
	close(asr.sentences)

	deadline := time.After(time.Second)
	for {
		select {
		case out, ok := <-session.Outputs():
			if !ok {
				t.Fatal("outputs closed before turn_error")
			}
			if out.Type == "turn_error" {
				if out.Error == "" {
					t.Fatal("turn_error has empty error")
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for TTS factory error")
		}
	}
}
