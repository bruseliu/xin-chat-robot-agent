package voice

import (
	"context"
	"time"

	"xin-chat-robot-agent/internal/conversation"

	"github.com/google/uuid"
)

func (p *Pipeline) finishTurn(ctx context.Context, handle conversation.TurnHandle, recorded bool, assistantText string, status conversation.TurnStatus, errorCode string) {
	if !recorded {
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := p.recorder.FinishTurn(finishCtx, conversation.FinishTurnRequest{
		Handle:        handle,
		AssistantText: assistantText,
		Status:        status,
		ErrorCode:     errorCode,
	}); err != nil {
		p.logger.Warn("record conversation turn finish failed", "error", err, "user_id", handle.UserID, "session_id", handle.ConversationID, "status", status)
	}
}

func turnIdempotencyKey(metadata map[string]string) string {
	if metadata != nil && metadata["idempotency_key"] != "" {
		return metadata["idempotency_key"]
	}
	return uuid.NewString()
}

func turnSource(metadata map[string]string) string {
	if metadata != nil && metadata["source"] != "" {
		return metadata["source"]
	}
	return "text_turn"
}
