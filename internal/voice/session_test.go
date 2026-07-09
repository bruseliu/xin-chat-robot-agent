package voice

import (
	"context"
	"log/slog"
	"testing"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/memory"
	"xin-chat-robot-agent/internal/vad"
)

func TestVoiceSessionWritesASRUntilFinalAudio(t *testing.T) {
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

func TestVoiceSessionFinalizesASROnVADEnd(t *testing.T) {
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
		t.Fatal("Final = false, want true for VAD turn")
	}
	if out.Response.Text != "agent: 你好" {
		t.Fatalf("response text = %q, want agent response", out.Response.Text)
	}
	if asr.writes != 1 || asr.finalizes != 1 {
		t.Fatalf("ASR writes/finalizes = %d/%d, want 1/1", asr.writes, asr.finalizes)
	}
	if runner.calls != 1 || runner.last.Text != "你好" {
		t.Fatalf("runner calls/text = %d/%q, want 1/你好", runner.calls, runner.last.Text)
	}
}

func TestVoiceSessionRotatesASRAfterVADEnd(t *testing.T) {
	runner := &fakeRunner{}
	pipeline := NewPipeline(runner, &fakeMemory{}, slog.Default())
	processor := &fakeVoiceVAD{out: vad.AudioOutput{
		Chunk:  vad.Chunk{Samples: []float32{0.1}, SampleRate: 16000},
		VADEnd: true,
	}}
	streams := []*fakeStreamingASR{
		{transcript: Transcript{Text: "第一句"}},
		{transcript: Transcript{Text: "第二句"}},
		{transcript: Transcript{Text: "第三句"}},
	}
	idx := 0
	session, err := NewVoiceSessionWithASRFactory(context.Background(), pipeline, processor, func(context.Context) (StreamingASR, error) {
		stream := streams[idx]
		idx++
		return stream, nil
	}, VoiceSessionConfig{UserID: "alice", SessionID: "s1"})
	if err != nil {
		t.Fatalf("NewVoiceSessionWithASRFactory() error = %v", err)
	}

	for range 2 {
		if _, err := session.ProcessAudio(context.Background(), AudioChunk{Samples: []float32{0.1}, SampleRate: 16000}); err != nil {
			t.Fatalf("ProcessAudio() error = %v", err)
		}
	}
	if runner.calls != 2 {
		t.Fatalf("runner calls = %d, want 2", runner.calls)
	}
	if streams[0].finalizes != 1 || streams[1].finalizes != 1 {
		t.Fatalf("stream finalizes = %d/%d, want 1/1", streams[0].finalizes, streams[1].finalizes)
	}
}

func TestVoiceSessionRunsTextTurnOnFinalAudio(t *testing.T) {
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

	out, err := session.ProcessAudio(context.Background(), AudioChunk{Samples: []float32{0.1}, SampleRate: 16000, Final: true})
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

func (f *fakeRunner) RunTurn(ctx context.Context, input agent.TurnInput) (agent.TurnOutput, error) {
	return f.RunTurnStream(ctx, input, nil)
}

func (f *fakeRunner) RunTurnStream(_ context.Context, input agent.TurnInput, yield func(agent.StreamEvent) bool) (agent.TurnOutput, error) {
	f.calls++
	f.last = input
	text := "agent: " + input.Text
	if yield != nil {
		yield(agent.StreamEvent{Type: agent.StreamEventLLMFinal, Text: text, Final: true})
	}
	return agent.TurnOutput{Text: text}, nil
}

type fakeMemory struct{}

func (fakeMemory) Search(context.Context, memory.SearchRequest) ([]memory.Memory, error) {
	return nil, nil
}

func (fakeMemory) Add(context.Context, memory.AddRequest) error {
	return nil
}
