package agent

import "testing"

func TestNewGraphUsesTextReasoningStages(t *testing.T) {
	graph, err := NewGraph(GraphConfig{AgentID: "test-agent"})
	if err != nil {
		t.Fatalf("NewGraph() error = %v", err)
	}

	edges := edgeSet(graph)
	for _, want := range []string{
		"START->turn_context",
		"turn_context->memory_context",
		"memory_context->llm",
		"llm->memory_commit",
	} {
		if !edges[want] {
			t.Fatalf("missing edge %q; got %v", want, edges)
		}
	}
	for _, removed := range []string{
		"START->vad",
		"vad->asr",
		"llm->tts",
	} {
		if edges[removed] {
			t.Fatalf("edge %q should live in voice runtime, not ADK graph", removed)
		}
	}
}

func edgeSet(graph *Graph) map[string]bool {
	set := make(map[string]bool, len(graph.Edges))
	for _, edge := range graph.Edges {
		set[edge.From.Name()+"->"+edge.To.Name()] = true
	}
	return set
}

func TestNormalizeTurnContext(t *testing.T) {
	got, err := normalizeTurnContext("hello")
	if err != nil {
		t.Fatalf("normalizeTurnContext() error = %v", err)
	}
	if got.Text != "hello" {
		t.Fatalf("Text = %q, want hello", got.Text)
	}
}
