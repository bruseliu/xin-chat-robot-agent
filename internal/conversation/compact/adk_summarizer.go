package compact

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"xin-chat-robot-agent/internal/conversation"
)

// ADKSummarizer adapts an ADK model.LLM for low-priority conversation
// compaction. It never enables streaming or tools.
type ADKSummarizer struct {
	Model model.LLM
}

func (summarizer ADKSummarizer) Summarize(ctx context.Context, previousSummary string, turns []conversation.Turn) (string, error) {
	if summarizer.Model == nil {
		return "", fmt.Errorf("conversation summarizer model is required")
	}
	prompt := summaryPrompt(previousSummary, turns)
	var result strings.Builder
	for response, err := range summarizer.Model.GenerateContent(ctx, &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)},
	}, false) {
		if err != nil {
			return "", err
		}
		if response == nil || response.Content == nil {
			continue
		}
		for _, part := range response.Content.Parts {
			if part != nil {
				result.WriteString(part.Text)
			}
		}
	}
	if strings.TrimSpace(result.String()) == "" {
		return "", fmt.Errorf("conversation summarizer returned empty text")
	}
	return result.String(), nil
}

func summaryPrompt(previousSummary string, turns []conversation.Turn) string {
	var builder strings.Builder
	builder.WriteString("请把以下历史对话压缩为简洁的中文上下文摘要。保留未解决请求、决定/承诺、人物实体和当前话题；不要编造事实，不要保留寒暄、被打断回复或工具元数据。输出仅为摘要，最多1600字。\n")
	if previousSummary != "" {
		builder.WriteString("已有摘要：\n")
		builder.WriteString(previousSummary)
		builder.WriteString("\n")
	}
	for _, turn := range turns {
		builder.WriteString("用户：")
		builder.WriteString(turn.UserText)
		builder.WriteString("\n助手：")
		builder.WriteString(turn.AssistantText)
		builder.WriteString("\n")
	}
	return builder.String()
}
