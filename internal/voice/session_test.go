package voice

import (
	"context"
	"log/slog"
	"testing"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/memory"
	"xin-chat-robot-agent/internal/vad"
)

func TestVoiceSessionWritesASRUntilVADEnd(t *testing.T) {
	pipeline := NewPipeline(&fakeRunner{}, &fakeMemory{}, slog.Default())
	processor := &fakeVoiceVAD{out: vad.AudioOutput{
		Chunk: vad.Chunk{Samples: []float32{0.1, 0.2}, SampleRate: 16000},
	}}
	asr := &fakeStreamingASR{}
	session, err := NewVoiceSession(pipeline, processor, asr, VoiceSessionConfig{UserID: "alice", SessionID: "s1"})
	if err != nil {
		t.Fatalf("NewVoiceSession() error = %v", err)
	}

	out, err := session.ProcessAudio(context.Background(), AudioChunk{Samples: []float32{0.1, 0.2}, SampleRate: 16000})
	if err != nil {
		t.Fatalf("ProcessAudio() error = %v", err)
	}
	if out.Final {
		t.Fatal("Final = true, want false before VAD end")
	}
	if asr.writes != 1 {
		t.Fatalf("ASR writes = %d, want 1", asr.writes)
	}
	if asr.finalizes != 0 {
		t.Fatalf("ASR finalizes = %d, want 0", asr.finalizes)
	}
}

func TestVoiceSessionRunsTextTurnOnFinalTranscript(t *testing.T) {
	runner := &fakeRunner{}
	pipeline := NewPipeline(runner, &fakeMemory{}, slog.Default())
	processor := &fakeVoiceVAD{out: vad.AudioOutput{
		Chunk:  vad.Chunk{Samples: []float32{0.1}, SampleRate: 16000},
		VADEnd: true,
	}}
	asr := &fakeStreamingASR{transcript: Transcript{Text: "你好"}}
	session, err := NewVoiceSession(pipeline, processor, asr, VoiceSessionConfig{UserID: "alice", SessionID: "s1"})
	if err != nil {
		t.Fatalf("NewVoiceSession() error = %v", err)
	}

	out, err := session.ProcessAudio(context.Background(), AudioChunk{Samples: []float32{0.1}, SampleRate: 16000})
	if err != nil {
		t.Fatalf("ProcessAudio() error = %v", err)
	}
	if !out.Final {
		t.Fatal("Final = false, want true")
	}
	if !out.Transcript.IsFinal {
		t.Fatal("Transcript.IsFinal = false, want true")
	}
	if out.Response.Text != "agent: 你好" {
		t.Fatalf("response text = %q, want agent response", out.Response.Text)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if runner.last.Text != "你好" {
		t.Fatalf("runner text = %q, want transcript", runner.last.Text)
	}
	if asr.writes != 1 || asr.finalizes != 1 {
		t.Fatalf("ASR writes/finalizes = %d/%d, want 1/1", asr.writes, asr.finalizes)
	}
}

type fakeVoiceVAD struct {
	out vad.AudioOutput
}

func (f *fakeVoiceVAD) Process(context.Context, vad.Chunk) (vad.AudioOutput, error) {
	return f.out, nil
}

func (f *fakeVoiceVAD) Reset() error { return nil }
func (f *fakeVoiceVAD) Close() error { return nil }

type fakeStreamingASR struct {
	transcript Transcript
	writes     int
	finalizes  int
}

func (f *fakeStreamingASR) Write(context.Context, vad.Chunk) error {
	f.writes++
	return nil
}

func (f *fakeStreamingASR) Finalize(context.Context) (Transcript, error) {
	f.finalizes++
	return f.transcript, nil
}

func (f *fakeStreamingASR) Reset() error { return nil }
func (f *fakeStreamingASR) Close() error { return nil }

type fakeRunner struct {
	calls int
	last  agent.TurnInput
}

func (f *fakeRunner) RunTurn(_ context.Context, input agent.TurnInput) (agent.TurnOutput, error) {
	f.calls++
	f.last = input
	return agent.TurnOutput{Text: "agent: " + input.Text}, nil
}

type fakeMemory struct{}

func (fakeMemory) Search(context.Context, memory.SearchRequest) ([]memory.Memory, error) {
	return nil, nil
}

func (fakeMemory) Add(context.Context, memory.AddRequest) error {
	return nil
}
