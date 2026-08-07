package conversation

import "testing"

func TestConversationKeyValidate(t *testing.T) {
	valid := ConversationKey{
		AppName:        "voice-agent",
		UserID:         "user-1",
		ConversationID: "session-1",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}

	tests := []struct {
		name string
		key  ConversationKey
		want error
	}{
		{name: "missing app", key: ConversationKey{UserID: "user", ConversationID: "session"}, want: ErrMissingAppName},
		{name: "missing user", key: ConversationKey{AppName: "app", ConversationID: "session"}, want: ErrMissingUserID},
		{name: "missing conversation", key: ConversationKey{AppName: "app", UserID: "user"}, want: ErrMissingConversationID},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.key.Validate(); err != test.want {
				t.Fatalf("Validate() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestTurnStatusTransitions(t *testing.T) {
	terminal := []TurnStatus{TurnCompleted, TurnInterrupted, TurnFailed}
	for _, next := range terminal {
		if !TurnPending.CanTransitionTo(next) {
			t.Errorf("pending -> %s should be allowed", next)
		}
	}
	if TurnCompleted.CanTransitionTo(TurnInterrupted) {
		t.Error("completed -> interrupted should not be allowed")
	}
	if TurnFailed.CanTransitionTo(TurnCompleted) {
		t.Error("failed -> completed should not be allowed")
	}
	if !TurnCompleted.CanTransitionTo(TurnCompleted) {
		t.Error("same-state transition should be allowed for idempotency")
	}
	if TurnStatus("unknown").CanTransitionTo(TurnCompleted) {
		t.Error("unknown status transition should not be allowed")
	}
}

func TestRequestValidation(t *testing.T) {
	key := ConversationKey{AppName: "app", UserID: "user", ConversationID: "conversation"}
	begin := BeginTurnRequest{
		ConversationKey: key,
		UserText:        "hello",
		Source:          "text",
		IdempotencyKey:  "request-1",
	}
	if err := begin.Validate(); err != nil {
		t.Fatalf("BeginTurnRequest.Validate() error = %v, want nil", err)
	}

	finish := FinishTurnRequest{
		Handle: TurnHandle{ConversationKey: key, TurnNo: 1},
		Status: TurnCompleted,
	}
	if err := finish.Validate(); err != nil {
		t.Fatalf("FinishTurnRequest.Validate() error = %v, want nil", err)
	}

	finish.Status = TurnPending
	if err := finish.Validate(); err == nil {
		t.Error("FinishTurnRequest.Validate() accepted pending status")
	}
}

func TestOptionalDependenciesCanBeNil(t *testing.T) {
	var recorder TurnRecorder
	var reader ContextReader
	var trigger CompactionTrigger
	if recorder != nil || reader != nil || trigger != nil {
		t.Fatal("zero-value optional dependencies should be nil")
	}
}
