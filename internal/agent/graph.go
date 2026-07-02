package agent

import (
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/workflow"
)

type GraphConfig struct {
	AgentID string
}

type Graph struct {
	Root  adkagent.Agent
	Edges []workflow.Edge
}

func NewGraph(cfg GraphConfig) (*Graph, error) {
	nodeConfig := workflow.NodeConfig{
		RetryConfig: workflow.DefaultRetryConfig(),
	}

	memoryContext := workflow.NewFunctionNode(
		"memory_context",
		func(ctx adkagent.Context, input string) (string, error) {
			return "memory_context node: ADK MemoryService is available to downstream agents.", nil
		},
		nodeConfig,
	)
	dialogue := workflow.NewFunctionNode(
		"dialogue",
		func(ctx adkagent.Context, input string) (string, error) {
			return "dialogue node: placeholder response from the ADK workflow graph.", nil
		},
		nodeConfig,
	)
	memoryCommit := workflow.NewFunctionNode(
		"memory_commit",
		func(ctx adkagent.Context, input string) (string, error) {
			return "memory_commit node: completed turn is ready for async memory write.", nil
		},
		nodeConfig,
	)

	rootName := cfg.AgentID
	if rootName == "" {
		rootName = "voice_agent_graph"
	}

	edges := workflow.Chain(workflow.Start, memoryContext, dialogue, memoryCommit)
	root, err := workflowagent.New(workflowagent.Config{
		Name:        rootName,
		Description: "Voice chatbot ADK workflow graph: memory_context -> dialogue -> memory_commit.",
		Edges:       edges,
	})
	if err != nil {
		return nil, fmt.Errorf("create ADK workflow graph: %w", err)
	}

	return &Graph{Root: root, Edges: edges}, nil
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

func DescribeEdges(edges []workflow.Edge) []string {
	lines := make([]string, 0, len(edges))
	for _, edge := range edges {
		if edge.Route == nil {
			lines = append(lines, edge.From.Name()+" -> "+edge.To.Name())
			continue
		}
		lines = append(lines, edge.From.Name()+" -> "+edge.To.Name()+" (route)")
	}
	return lines
}
