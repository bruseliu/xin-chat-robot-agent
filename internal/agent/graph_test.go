package agent

import (
	"context"
	"testing"

	"xin-chat-robot-agent/internal/vad"
)

func TestNewGraphFanOutAfterLLM(t *testing.T) {
	graph, err := NewGraph(GraphConfig{AgentID: "test-agent"})
	if err != nil {
		t.Fatalf("NewGraph() error = %v", err)
	}

	edges := edgeSet(graph)
	for _, want := range []string{
		"START->vad",
		"vad->asr",
		"asr->memory_context",
		"memory_context->llm",
		"llm->memory_commit",
		"llm->tts",
	} {
		if !edges[want] {
			t.Fatalf("missing edge %q; got %v", want, edges)
		}
	}
	if edges["START->asr"] {
		t.Fatal("vad must be the first business node; got START->asr edge")
	}
	if edges["memory_commit->tts"] {
		t.Fatal("memory_commit must not gate tts; got memory_commit->tts edge")
	}
}

func edgeSet(graph *Graph) map[string]bool {
	set := make(map[string]bool, len(graph.Edges))
	for _, edge := range graph.Edges {
		set[edge.From.Name()+"->"+edge.To.Name()] = true
	}
	return set
}

func TestRunVADPassesThroughText(t *testing.T) {
	got, err := runVAD(context.Background(), &fakeVAD{}, "hello")
	if err != nil {
		t.Fatalf("runVAD() error = %v", err)
	}
	if got != "hello" {
		t.Fatalf("runVAD() = %v, want text passthrough", got)
	}
}

func TestRunVADRejectsAudioWithoutDetector(t *testing.T) {
	_, err := runVAD(context.Background(), nil, vad.Chunk{Samples: make([]float32, 512), SampleRate: 16000})
	if err == nil {
		t.Fatal("runVAD() error = nil, want missing detector error")
	}
}

func TestRunVADProcessesAudioChunk(t *testing.T) {
	processor := &fakeVAD{output: vad.AudioOutput{Chunk: vad.Chunk{Samples: make([]float32, 512), SampleRate: 16000}}}
	got, err := runVAD(context.Background(), processor, vad.Chunk{Samples: make([]float32, 512), SampleRate: 16000})
	if err != nil {
		t.Fatalf("runVAD() error = %v", err)
	}
	if processor.calls != 1 {
		t.Fatalf("processor calls = %d, want 1", processor.calls)
	}
	output, ok := got.(vad.AudioOutput)
	if !ok {
		t.Fatalf("runVAD() type = %T, want vad.AudioOutput", got)
	}
	if len(output.Chunk.Samples) != 512 {
		t.Fatalf("output samples = %d, want current audio chunk", len(output.Chunk.Samples))
	}
	if output.VADEnd {
		t.Fatalf("VADEnd = true, want false")
	}
}

type fakeVAD struct {
	output vad.AudioOutput
	calls  int
}

func (f *fakeVAD) Process(ctx context.Context, chunk vad.Chunk) (vad.AudioOutput, error) {
	f.calls++
	return f.output, nil
}

func (f *fakeVAD) Reset() error { return nil }
func (f *fakeVAD) Close() error { return nil }
