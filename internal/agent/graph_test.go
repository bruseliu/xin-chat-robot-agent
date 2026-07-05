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

func TestRunVADProcessesAudioChunk(t *testing.T) {
	detector := &fakeVAD{decision: vad.Decision{SpeechActive: true, SampleRate: 16000}}
	got, err := runVAD(context.Background(), detector, vad.Chunk{Samples: make([]float32, 512), SampleRate: 16000})
	if err != nil {
		t.Fatalf("runVAD() error = %v", err)
	}
	if detector.calls != 1 {
		t.Fatalf("detector calls = %d, want 1", detector.calls)
	}
	decision, ok := got.(vad.Decision)
	if !ok {
		t.Fatalf("runVAD() type = %T, want vad.Decision", got)
	}
	if !decision.SpeechActive {
		t.Fatalf("decision = %+v, want active speech", decision)
	}
}

type fakeVAD struct {
	decision vad.Decision
	calls    int
}

func (f *fakeVAD) Process(ctx context.Context, chunk vad.Chunk) (vad.Decision, error) {
	f.calls++
	return f.decision, nil
}

func (f *fakeVAD) Reset() error { return nil }
func (f *fakeVAD) Close() error { return nil }
