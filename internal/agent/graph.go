package agent

import (
	"context"
	"fmt"
	"strings"

	"xin-chat-robot-agent/internal/vad"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/workflowagent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
)

type GraphConfig struct {
	AgentID               string
	VAD                   vad.AudioProcessor
	VADPreRollMs          int
	VADAudioBufferSeconds int
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

type TTSOutput struct {
	Text  string         `json:"text"`
	Audio *AttachmentRef `json:"audio,omitempty"`
}

func NewGraph(cfg GraphConfig) (*Graph, error) {
	nodeConfig := workflow.NodeConfig{
		RetryConfig: workflow.DefaultRetryConfig(),
	}

	vadNode := workflow.NewFunctionNode(
		"vad",
		func(ctx adkagent.Context, input any) (any, error) {
			return runVAD(ctx, cfg.VAD, input)
		},
		nodeConfig,
	)
	asr := workflow.NewFunctionNode(
		"asr",
		func(ctx adkagent.Context, input any) (TurnContext, error) {
			return TurnContext{Text: normalizeTextInput(input)}, nil
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
			// Side-effect node: future memory writes should not gate or compete with TTS output.
			return nil, nil
		},
		nodeConfig,
	)
	tts := workflow.NewFunctionNode(
		"tts",
		func(ctx adkagent.Context, output LLMOutput) (TTSOutput, error) {
			return TTSOutput{Text: output.Text}, nil
		},
		nodeConfig,
	)

	rootName := cfg.AgentID
	if rootName == "" {
		rootName = "voice_agent_graph"
	}

	edges := workflow.Concat(
		workflow.Chain(workflow.Start, vadNode, asr, memoryContext, llm),
		[]workflow.Edge{
			{From: llm, To: memoryCommit},
			{From: llm, To: tts},
		},
	)
	root, err := workflowagent.New(workflowagent.Config{
		Name:        rootName,
		Description: "Voice chatbot ADK workflow graph: VAD -> ASR -> memory_context -> LLM fan-out to memory_commit and TTS.",
		Edges:       edges,
	})
	if err != nil {
		return nil, fmt.Errorf("create ADK workflow graph: %w", err)
	}

	return &Graph{Root: root, Edges: edges}, nil
}

func runVAD(ctx context.Context, processor vad.AudioProcessor, input any) (any, error) {
	switch value := input.(type) {
	case vad.Chunk:
		if processor == nil {
			return nil, fmt.Errorf("vad audio input requires a configured detector")
		}
		return processor.Process(ctx, value)
	case *vad.Chunk:
		if value == nil {
			return nil, fmt.Errorf("vad input chunk is nil")
		}
		if processor == nil {
			return nil, fmt.Errorf("vad audio input requires a configured detector")
		}
		return processor.Process(ctx, *value)
	default:
		return input, nil
	}
}

func normalizeTextInput(input any) string {
	switch value := input.(type) {
	case string:
		if value != "" {
			return value
		}
	case TurnContext:
		if value.Text != "" {
			return value.Text
		}
	case vad.AudioOutput:
		if len(value.Chunk.Samples) > 0 || len(value.Chunk.PCM16LE) > 0 || value.VADEnd {
			return "asr node: placeholder transcript from VAD-gated audio input."
		}
	case map[string]any:
		if text, ok := value["text"].(string); ok && text != "" {
			return text
		}
	}
	return "asr node: placeholder transcript from streaming audio input."
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
