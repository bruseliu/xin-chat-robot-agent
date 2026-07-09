package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const defaultDoubaoBaseURL = "https://ark.cn-beijing.volces.com/api/v3"

type DoubaoConfig struct {
	BaseURL    string
	APIKey     string
	Model      string
	Timeout    time.Duration
	HTTPClient *http.Client
}

type DoubaoLLM struct {
	cfg    DoubaoConfig
	client *http.Client
}

var _ model.LLM = (*DoubaoLLM)(nil)

func NewDoubaoLLM(_ context.Context, cfg DoubaoConfig) (*DoubaoLLM, error) {
	cfg = normalizeDoubaoConfig(cfg)
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("doubao LLM API key is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("doubao LLM model is required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &DoubaoLLM{cfg: cfg, client: client}, nil
}

func normalizeDoubaoConfig(cfg DoubaoConfig) DoubaoConfig {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultDoubaoBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	return cfg
}

func (m *DoubaoLLM) Name() string {
	return m.cfg.Model
}

func (m *DoubaoLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		chatReq, err := m.newChatRequest(req, stream)
		if err != nil {
			yield(nil, err)
			return
		}
		body, err := json.Marshal(chatReq)
		if err != nil {
			yield(nil, fmt.Errorf("marshal doubao LLM request: %w", err))
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			yield(nil, err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)

		resp, err := m.client.Do(httpReq)
		if err != nil {
			yield(nil, fmt.Errorf("call doubao LLM: %w", err))
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			message := readErrorBody(resp.Body)
			yield(nil, fmt.Errorf("doubao LLM HTTP %d: %s", resp.StatusCode, message))
			return
		}
		if stream {
			m.readStream(resp.Body, yield)
			return
		}
		m.readOnce(resp.Body, yield)
	}
}

func (m *DoubaoLLM) newChatRequest(req *model.LLMRequest, stream bool) (doubaoChatRequest, error) {
	if req == nil {
		return doubaoChatRequest{}, fmt.Errorf("nil LLM request")
	}
	messages := make([]doubaoMessage, 0, len(req.Contents)+1)
	if req.Config != nil && req.Config.SystemInstruction != nil {
		if text := contentText(req.Config.SystemInstruction); text != "" {
			messages = append(messages, doubaoMessage{Role: "system", Content: text})
		}
	}
	for _, content := range req.Contents {
		text := contentText(content)
		if text == "" {
			continue
		}
		messages = append(messages, doubaoMessage{Role: doubaoRole(content.Role), Content: text})
	}
	if len(messages) == 0 {
		return doubaoChatRequest{}, fmt.Errorf("empty LLM messages")
	}
	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		modelName = m.cfg.Model
	}
	out := doubaoChatRequest{
		Model:    modelName,
		Messages: messages,
		Stream:   stream,
	}
	return out, nil
}

func (m *DoubaoLLM) readOnce(body io.Reader, yield func(*model.LLMResponse, error) bool) {
	var resp doubaoChatResponse
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		yield(nil, fmt.Errorf("decode doubao LLM response: %w", err))
		return
	}
	if resp.Error != nil {
		yield(nil, fmt.Errorf("doubao LLM error: %s", resp.Error.Message))
		return
	}
	text := resp.firstMessageText()
	yield(&model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel), ModelVersion: resp.Model}, nil)
}

func (m *DoubaoLLM) readStream(body io.Reader, yield func(*model.LLMResponse, error) bool) {
	reader := bufio.NewReader(body)
	var full strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSpace(line)
			if line != "" {
				if strings.HasPrefix(line, "data:") {
					line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				}
				if line == "[DONE]" {
					break
				}
				if strings.HasPrefix(line, ":") {
					continue
				}
				var chunk doubaoChatResponse
				if decodeErr := json.Unmarshal([]byte(line), &chunk); decodeErr != nil {
					yield(nil, fmt.Errorf("decode doubao LLM stream chunk: %w", decodeErr))
					return
				}
				if chunk.Error != nil {
					yield(nil, fmt.Errorf("doubao LLM stream error: %s", chunk.Error.Message))
					return
				}
				delta := chunk.firstDeltaText()
				if delta != "" {
					full.WriteString(delta)
					if !yield(&model.LLMResponse{Content: genai.NewContentFromText(delta, genai.RoleModel), Partial: true, ModelVersion: chunk.Model}, nil) {
						return
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			yield(nil, fmt.Errorf("read doubao LLM stream: %w", err))
			return
		}
	}
	yield(&model.LLMResponse{Content: genai.NewContentFromText(full.String(), genai.RoleModel)}, nil)
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

func doubaoRole(role string) string {
	switch role {
	case genai.RoleModel:
		return "assistant"
	case "system":
		return "system"
	default:
		return "user"
	}
}

func readErrorBody(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil || len(data) == 0 {
		return "empty response body"
	}
	var payload struct {
		Error *doubaoError `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil && payload.Error != nil && payload.Error.Message != "" {
		return payload.Error.Message
	}
	return string(data)
}

type doubaoChatRequest struct {
	Model    string          `json:"model"`
	Messages []doubaoMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type doubaoMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type doubaoChatResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []doubaoChoice `json:"choices"`
	Error   *doubaoError   `json:"error,omitempty"`
}

type doubaoChoice struct {
	Message      doubaoMessage `json:"message"`
	Delta        doubaoMessage `json:"delta"`
	FinishReason string        `json:"finish_reason"`
}

type doubaoError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}

func (r doubaoChatResponse) firstMessageText() string {
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].Message.Content
}

func (r doubaoChatResponse) firstDeltaText() string {
	if len(r.Choices) == 0 {
		return ""
	}
	return r.Choices[0].Delta.Content
}
