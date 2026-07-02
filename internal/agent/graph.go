package agent

import (
	"fmt"
	"iter"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/workflowagents/sequentialagent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

type GraphConfig struct {
	AgentID string
}

type Edge struct {
	From string
	To   string
	Kind string
}

type Graph struct {
	Root  adkagent.Agent
	Edges []Edge
}

func NewGraph(cfg GraphConfig) (*Graph, error) {
	memoryContext, err := newGraphNode(
		"memory_context",
		"Loads long-term user memory through ADK MemoryService before the dialogue agent runs.",
		"memory_context node: ADK MemoryService is available to downstream agents.",
	)
	if err != nil {
		return nil, err
	}

	dialogue, err := newGraphNode(
		"dialogue",
		"Main conversational agent. This will be replaced by llmagent.New with model and tools.",
		"dialogue node: placeholder response from the ADK graph.",
	)
	if err != nil {
		return nil, err
	}

	memoryCommit, err := newGraphNode(
		"memory_commit",
		"Commits the completed turn/session back to long-term memory.",
		"memory_commit node: completed turn is ready for async memory write.",
	)
	if err != nil {
		return nil, err
	}

	rootName := cfg.AgentID
	if rootName == "" {
		rootName = "voice_agent_graph"
	}

	root, err := sequentialagent.New(sequentialagent.Config{
		AgentConfig: adkagent.Config{
			Name:        rootName,
			Description: "Voice chatbot ADK graph: memory_context -> dialogue -> memory_commit.",
			SubAgents: []adkagent.Agent{
				memoryContext,
				dialogue,
				memoryCommit,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create ADK graph: %w", err)
	}

	return &Graph{
		Root: root,
		Edges: []Edge{
			{From: rootName, To: "memory_context", Kind: "contains"},
			{From: rootName, To: "dialogue", Kind: "contains"},
			{From: rootName, To: "memory_commit", Kind: "contains"},
			{From: "memory_context", To: "dialogue", Kind: "sequential_next"},
			{From: "dialogue", To: "memory_commit", Kind: "sequential_next"},
		},
	}, nil
}

func newGraphNode(name, description, output string) (adkagent.Agent, error) {
	node, err := adkagent.New(adkagent.Config{
		Name:        name,
		Description: description,
		Run: func(ctx adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(yield func(*session.Event, error) bool) {
				yield(&session.Event{
					Author: name,
					LLMResponse: model.LLMResponse{
						Content: genai.NewContentFromText(output, genai.RoleModel),
						CustomMetadata: map[string]any{
							"graph_node": name,
						},
					},
				}, nil)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create graph node %s: %w", name, err)
	}
	return node, nil
}

func DescribeGraph(root adkagent.Agent) []string {
	if root == nil {
		return nil
	}
	lines := []string{root.Name()}
	for _, child := range root.SubAgents() {
		lines = append(lines, "  -> "+child.Name()+": "+child.Description())
	}
	return lines
}

func DescribeEdges(edges []Edge) []string {
	lines := make([]string, 0, len(edges))
	for _, edge := range edges {
		lines = append(lines, edge.From+" -> "+edge.To+" ("+edge.Kind+")")
	}
	return lines
}
