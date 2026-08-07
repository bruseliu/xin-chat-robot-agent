package context

import (
	stdcontext "context"
	"testing"

	"xin-chat-robot-agent/internal/conversation"
)

type fakeReader struct {
	value conversation.Context
}

func (reader fakeReader) LoadContext(stdcontext.Context, string, string, string, conversation.ContextLimits) (conversation.Context, error) {
	return reader.value, nil
}

func TestBuilderBudgetMixedText(t *testing.T) {
	builder, err := New(fakeReader{value: conversation.Context{
		Summary:     conversation.Summary{Text: "summary"},
		RecentTurns: []conversation.Turn{{Status: conversation.TurnCompleted, UserText: "你好 hello", AssistantText: "world 世界"}},
	}}, Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, err := builder.LoadContext(stdcontext.Background(), "app", "user", "conversation", conversation.ContextLimits{MaxTextUnits: 7})
	if err != nil {
		t.Fatalf("LoadContext() error = %v", err)
	}
	if units := contextUnits(got); units > 7 {
		t.Fatalf("context units = %d, want <= 7", units)
	}
}

func TestBuilderRetainsNewestCompletedTurn(t *testing.T) {
	turns := []conversation.Turn{
		{TurnNo: 1, Status: conversation.TurnCompleted, UserText: "old old old", AssistantText: "old old old"},
		{TurnNo: 2, Status: conversation.TurnCompleted, UserText: "newest", AssistantText: "answer"},
	}
	builder, err := New(fakeReader{value: conversation.Context{RecentTurns: turns}}, Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, err := builder.LoadContext(stdcontext.Background(), "app", "user", "conversation", conversation.ContextLimits{MaxTextUnits: 4})
	if err != nil {
		t.Fatalf("LoadContext() error = %v", err)
	}
	if len(got.RecentTurns) != 1 || got.RecentTurns[0].TurnNo != 2 {
		t.Fatalf("recent turns = %+v, want newest turn only", got.RecentTurns)
	}
}

func TestBuilderFiltersNonCompletedTurns(t *testing.T) {
	builder, err := New(fakeReader{value: conversation.Context{RecentTurns: []conversation.Turn{
		{TurnNo: 1, Status: conversation.TurnInterrupted, UserText: "discard"},
		{TurnNo: 2, Status: conversation.TurnFailed, UserText: "discard"},
		{TurnNo: 3, Status: conversation.TurnCompleted, UserText: "keep"},
	}}}, Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, err := builder.LoadContext(stdcontext.Background(), "app", "user", "conversation", conversation.ContextLimits{})
	if err != nil {
		t.Fatalf("LoadContext() error = %v", err)
	}
	if len(got.RecentTurns) != 1 || got.RecentTurns[0].TurnNo != 3 {
		t.Fatalf("recent turns = %+v, want completed turn only", got.RecentTurns)
	}
}

func TestBuilderOversizedTurnIsValidUTF8AndMarked(t *testing.T) {
	builder, err := New(fakeReader{value: conversation.Context{RecentTurns: []conversation.Turn{{
		Status:        conversation.TurnCompleted,
		UserText:      string([]byte{'a', 0xff, 'b', 'c', 'd', 'e'}),
		AssistantText: "very long assistant response",
	}}}}, Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, err := builder.LoadContext(stdcontext.Background(), "app", "user", "conversation", conversation.ContextLimits{MaxTextUnits: 8})
	if err != nil {
		t.Fatalf("LoadContext() error = %v", err)
	}
	turn := got.RecentTurns[0]
	if units := contextUnits(got); units > 8 {
		t.Fatalf("context units = %d, want <= 8", units)
	}
	if turn.UserText == "" && turn.AssistantText == "" {
		t.Fatal("oversized turn was dropped")
	}
	if turn.UserText != "" && !containsMarker(turn.UserText) && turn.AssistantText != "" && !containsMarker(turn.AssistantText) {
		t.Fatal("oversized turn text has no truncation marker")
	}
}

func TestBuilderEmptyContext(t *testing.T) {
	builder, err := New(fakeReader{}, Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, err := builder.LoadContext(stdcontext.Background(), "app", "user", "conversation", conversation.ContextLimits{})
	if err != nil {
		t.Fatalf("LoadContext() error = %v", err)
	}
	if got.Summary.Text != "" || len(got.RecentTurns) != 0 {
		t.Fatalf("LoadContext() = %+v, want empty context", got)
	}
}

func containsMarker(text string) bool {
	return len(text) >= len(truncationMarker) && text[len(text)-len(truncationMarker):] == truncationMarker
}
