package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ModeSelfHosted = "self_hosted"
	ModeCloudV3    = "cloud_v3"
)

type Mem0Config struct {
	BaseURL string
	APIKey  string
	Mode    string
	TopK    int
	// Timeout bounds Search, which sits on the turn's critical path and must
	// fail fast rather than delay the response.
	Timeout time.Duration
	// WriteTimeout bounds Add. Writes run in the background, and mem0 spends
	// several seconds of LLM time per write extracting facts and reconciling
	// them against existing memories — a cost that grows with store size. This
	// is deliberately far more generous than Timeout; too tight a value silently
	// drops memories.
	WriteTimeout time.Duration
}

type Mem0Client struct {
	baseURL      *url.URL
	apiKey       string
	mode         string
	topK         int
	timeout      time.Duration
	writeTimeout time.Duration
	httpClient   *http.Client
	logger       *slog.Logger
}

func NewMem0Client(cfg Mem0Config, logger *slog.Logger) *Mem0Client {
	baseURL, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		baseURL, _ = url.Parse("http://localhost:8888")
	}
	mode := cfg.Mode
	if mode == "" {
		mode = ModeSelfHosted
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	writeTimeout := cfg.WriteTimeout
	if writeTimeout == 0 {
		writeTimeout = 60 * time.Second
	}
	topK := cfg.TopK
	if topK == 0 {
		topK = 8
	}
	return &Mem0Client{
		baseURL:      baseURL,
		apiKey:       cfg.APIKey,
		mode:         mode,
		topK:         topK,
		timeout:      timeout,
		writeTimeout: writeTimeout,
		// No client-level timeout: Search and Add need different budgets, so each
		// applies its own per-request deadline via context.
		httpClient: &http.Client{},
		logger:     logger,
	}
}

func (c *Mem0Client) Search(ctx context.Context, req SearchRequest) ([]Memory, error) {
	topK := req.TopK
	if topK == 0 {
		topK = c.topK
	}

	payload := map[string]any{
		"query":   req.Query,
		"filters": identityFilters(req.UserID, req.AgentID, req.RunID),
		"top_k":   topK,
	}

	ctx, cancel := c.withTimeout(ctx, c.timeout)
	defer cancel()

	var raw map[string]any
	if err := c.postJSON(ctx, c.searchPath(), payload, &raw); err != nil {
		return nil, err
	}
	return normalizeMemories(raw), nil
}

// withTimeout applies d as a deadline unless ctx already has a tighter one.
func (c *Mem0Client) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= d {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func (c *Mem0Client) Add(ctx context.Context, req AddRequest) error {
	if len(req.Messages) == 0 {
		return nil
	}

	payload := map[string]any{
		"messages": req.Messages,
	}
	if req.UserID != "" {
		payload["user_id"] = req.UserID
	}
	if req.AgentID != "" {
		payload["agent_id"] = req.AgentID
	}
	if req.RunID != "" {
		payload["run_id"] = req.RunID
	}
	if req.Metadata != nil {
		payload["metadata"] = req.Metadata
	}
	if req.Infer != nil {
		payload["infer"] = *req.Infer
	}

	ctx, cancel := c.withTimeout(ctx, c.writeTimeout)
	defer cancel()

	var raw map[string]any
	return c.postJSON(ctx, c.addPath(), payload, &raw)
}

func (c *Mem0Client) searchPath() string {
	if c.mode == ModeCloudV3 {
		return "/v3/memories/search/"
	}
	return "/search"
}

func (c *Mem0Client) addPath() string {
	if c.mode == ModeCloudV3 {
		return "/v3/memories/add/"
	}
	return "/memories"
}

func (c *Mem0Client) postJSON(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal mem0 payload: %w", err)
	}

	endpoint := c.baseURL.ResolveReference(&url.URL{Path: path})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create mem0 request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		if c.mode == ModeCloudV3 {
			httpReq.Header.Set("Authorization", "Token "+c.apiKey)
		} else {
			httpReq.Header.Set("X-API-Key", c.apiKey)
		}
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("call mem0 %s: %w", path, err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read mem0 response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("mem0 %s returned %d: %s", path, httpResp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if len(respBody) == 0 || out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode mem0 response: %w", err)
	}
	return nil
}

func identityFilters(userID, agentID, runID string) map[string]any {
	filters := make(map[string]any, 3)
	if userID != "" {
		filters["user_id"] = userID
	}
	if agentID != "" {
		filters["agent_id"] = agentID
	}
	if runID != "" {
		filters["run_id"] = runID
	}
	return filters
}

func normalizeMemories(raw map[string]any) []Memory {
	results, ok := raw["results"].([]any)
	if !ok {
		if direct, ok := raw["memories"].([]any); ok {
			results = direct
		}
	}
	memories := make([]Memory, 0, len(results))
	for _, item := range results {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		memories = append(memories, Memory{
			ID:        stringValue(row["id"]),
			Text:      firstString(row["memory"], row["text"], row["data"]),
			Score:     floatValue(row["score"]),
			UserID:    stringValue(row["user_id"]),
			AgentID:   stringValue(row["agent_id"]),
			RunID:     stringValue(row["run_id"]),
			Metadata:  mapValue(row["metadata"]),
			CreatedAt: stringValue(row["created_at"]),
			UpdatedAt: stringValue(row["updated_at"]),
		})
	}
	return memories
}

func firstString(values ...any) string {
	for _, value := range values {
		switch v := value.(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if nested := stringValue(v["memory"]); nested != "" {
				return nested
			}
		}
	}
	return ""
}

func stringValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func floatValue(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	default:
		return 0
	}
}

func mapValue(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return nil
}
