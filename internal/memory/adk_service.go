package memory

import (
	"context"
	"strings"
	"time"

	adkmemory "google.golang.org/adk/memory"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

type ADKService struct {
	store   Store
	agentID string
	topK    int
}

func NewADKService(store Store, agentID string, topK int) adkmemory.Service {
	return &ADKService{
		store:   store,
		agentID: agentID,
		topK:    topK,
	}
}

func (s *ADKService) AddSessionToMemory(ctx context.Context, curSession session.Session) error {
	messages := make([]Message, 0, curSession.Events().Len())
	for event := range curSession.Events().All() {
		if event.LLMResponse.Content == nil {
			continue
		}
		text := contentText(event.LLMResponse.Content)
		if text == "" {
			continue
		}
		messages = append(messages, Message{
			Role:    mem0Role(event.LLMResponse.Content.Role),
			Content: text,
		})
	}
	if len(messages) == 0 {
		return nil
	}
	return s.store.Add(ctx, AddRequest{
		UserID:   curSession.UserID(),
		AgentID:  firstNonEmpty(s.agentID, curSession.AppName()),
		RunID:    curSession.ID(),
		Messages: messages,
		Metadata: map[string]any{
			"app_name":   curSession.AppName(),
			"session_id": curSession.ID(),
			"source":     "adk_session",
		},
	})
}

func (s *ADKService) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	memories, err := s.store.Search(ctx, SearchRequest{
		UserID:  req.UserID,
		AgentID: firstNonEmpty(s.agentID, req.AppName),
		Query:   req.Query,
		TopK:    s.topK,
	})
	if err != nil {
		return nil, err
	}

	resp := &adkmemory.SearchResponse{
		Memories: make([]adkmemory.Entry, 0, len(memories)),
	}
	for _, item := range memories {
		timestamp := parseTime(item.UpdatedAt)
		if timestamp.IsZero() {
			timestamp = parseTime(item.CreatedAt)
		}
		resp.Memories = append(resp.Memories, adkmemory.Entry{
			ID:             item.ID,
			Content:        genai.NewContentFromText(item.Text, genai.RoleUser),
			Author:         item.UserID,
			Timestamp:      timestamp,
			CustomMetadata: item.Metadata,
		})
	}
	return resp, nil
}

func contentText(content *genai.Content) string {
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

func mem0Role(role string) string {
	switch role {
	case genai.RoleModel:
		return "assistant"
	case genai.RoleUser:
		return "user"
	default:
		return "assistant"
	}
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed
	}
	parsed, _ = time.Parse(time.RFC3339, value)
	return parsed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
