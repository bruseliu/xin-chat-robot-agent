package agent

import (
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

type GraphConfig struct {
	AgentID string
}

type Graph struct {
	Root  adkagent.Agent
	Edges []workflow.Edge
}

type AttachmentRef struct {
	ID       string            `json:"id,omitempty"`
	Kind     string            `json:"kind,omitempty"`
	URI      string            `json:"uri,omitempty"`
	MIMEType string            `json:"mime_type,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type TurnContext struct {
	Text        string          `json:"text"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}

type LLMInput struct {
	Text        string          `json:"text"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
	Memories    []MemoryInput   `json:"memories,omitempty"`
}

type MemoryInput struct {
	ID    string  `json:"id,omitempty"`
	Text  string  `json:"text"`
	Score float64 `json:"score,omitempty"`
}

type LLMOutput struct {
	Text        string          `json:"text"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}

func NewGraph(cfg GraphConfig) (*Graph, error) {
	nodeConfig := workflow.NodeConfig{
		RetryConfig: workflow.DefaultRetryConfig(),
	}

	turnContext := workflow.NewFunctionNode(
		"turn_context",
		func(ctx adkagent.Context, input any) (TurnContext, error) {
			return normalizeTurnContext(input)
		},
		nodeConfig,
	)
	memoryContext := workflow.NewFunctionNode(
		"memory_context",
		func(ctx adkagent.Context, turn TurnContext) (LLMInput, error) {
			memories, err := ctx.SearchMemory(ctx, turn.Text)
			if err != nil {
				return LLMInput{Text: turn.Text, Attachments: turn.Attachments}, nil
			}
			return LLMInput{
				Text:        turn.Text,
				Attachments: turn.Attachments,
				Memories:    memoryInputs(memories),
			}, nil
		},
		nodeConfig,
	)
	llm := workflow.NewFunctionNode(
		"llm",
		func(ctx adkagent.Context, input LLMInput) (LLMOutput, error) {
			return LLMOutput{Text: "llm node: placeholder response from the ADK workflow graph."}, nil
		},
		nodeConfig,
	)
	memoryCommit := workflow.NewFunctionNode(
		"memory_commit",
		func(ctx adkagent.Context, output LLMOutput) (any, error) {
			// Side-effect node: future memory writes should not gate response streaming.
			return nil, nil
		},
		nodeConfig,
	)

	rootName := cfg.AgentID
	if rootName == "" {
		rootName = "text_agent_graph"
	}

	edges := workflow.Chain(workflow.Start, turnContext, memoryContext, llm, memoryCommit)
	root, err := workflowagent.New(workflowagent.Config{
		Name:        rootName,
		Description: "Text chatbot ADK workflow graph: turn_context -> memory_context -> LLM -> memory_commit.",
		Edges:       edges,
	})
	if err != nil {
		return nil, fmt.Errorf("create ADK workflow graph: %w", err)
	}

	return &Graph{Root: root, Edges: edges}, nil
}

func normalizeTurnContext(input any) (TurnContext, error) {
	switch value := input.(type) {
	case string:
		return TurnContext{Text: value}, nil
	case TurnContext:
		return value, nil
	case *TurnContext:
		if value == nil {
			return TurnContext{}, fmt.Errorf("turn context is nil")
		}
		return *value, nil
	case map[string]any:
		turn := TurnContext{}
		if text, ok := value["text"].(string); ok {
			turn.Text = text
		}
		return turn, nil
	default:
		return TurnContext{}, fmt.Errorf("unsupported turn input type %T", input)
	}
}

func memoryInputs(resp *adkmemory.SearchResponse) []MemoryInput {
	if resp == nil || len(resp.Memories) == 0 {
		return nil
	}
	items := make([]MemoryInput, 0, len(resp.Memories))
	for _, memory := range resp.Memories {
		text := contentText(memory.Content)
		if text == "" {
			continue
		}
		items = append(items, MemoryInput{
			ID:   memory.ID,
			Text: text,
		})
	}
	return items
}

func contentText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var b strings.Builder
	for _, part := range content.Parts {
		if part == nil || part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part.Text)
	}
	return b.String()
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
