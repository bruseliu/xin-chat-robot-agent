# Development Status

## Current Stack Decision

- Language: Go
- Agent framework: ADK Go package, `google.golang.org/adk`
- Long-term memory: mem0
- Current service entry: `cmd/server/main.go`

Note: the installed ADK Go module version is `google.golang.org/adk v1.5.0`. This is the Go module version currently resolved by `go list`; it does not appear as a `v2.x` Go module.

## Completed

### Go Service Skeleton

The project now has a compilable Go service with:

- HTTP health endpoint: `GET /healthz`
- Text turn endpoint: `POST /v1/chat/text`
- Basic config loader from environment variables
- Structured JSON logging
- Graceful shutdown

### mem0 Client

Implemented in `internal/memory/mem0_client.go`.

Supported modes:

- `self_hosted`
  - `POST /search`
  - `POST /memories`
  - auth header: `X-API-Key`
- `cloud_v3`
  - `POST /v3/memories/search/`
  - `POST /v3/memories/add/`
  - auth header: `Authorization: Token <api-key>`

The self-hosted paths and auth mode were checked against the local mem0 source at `/data/liuxin/mem0/server/main.py` and `/data/liuxin/mem0/server/README.md`.

### Local Memory Interface

Implemented in `internal/memory/types.go`.

The project has its own small memory boundary:

```go
type Store interface {
    Search(ctx context.Context, req SearchRequest) ([]Memory, error)
    Add(ctx context.Context, req AddRequest) error
}
```

This keeps the voice pipeline and agent logic independent of mem0 deployment details.

### ADK MemoryService Adapter

Implemented in `internal/memory/adk_service.go`.

`NewADKService(...)` returns `google.golang.org/adk/memory.Service`.

This matches the ADK source interface:

```go
type Service interface {
    AddSessionToMemory(ctx context.Context, s session.Session) error
    SearchMemory(ctx context.Context, req *SearchRequest) (*SearchResponse, error)
}
```

Source checked:

- `/home/liuxin/go/pkg/mod/google.golang.org/adk@v1.5.0/memory/service.go`

### ADK Graph Skeleton

Implemented in `internal/agent/graph.go` with the official ADK v2 workflow API.

Current ADK workflow graph:

```text
START -> memory_context -> dialogue -> memory_commit
```

This is built with:

```go
memoryContext := workflow.NewFunctionNode("memory_context", memoryContextFn, nodeConfig)
dialogue := workflow.NewFunctionNode("dialogue", dialogueFn, nodeConfig)
memoryCommit := workflow.NewFunctionNode("memory_commit", memoryCommitFn, nodeConfig)

edges := workflow.Chain(workflow.Start, memoryContext, dialogue, memoryCommit)
root, err := workflowagent.New(workflowagent.Config{
    Name:  rootName,
    Edges: edges,
})
```

Source checked:

- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/workflow/function_node.go`
- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/workflow/edgebuilder.go`
- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/agent/workflowagent/workflow.go`

## Important Limitation

The current ADK graph is a skeleton, not the final production graph.

The three graph nodes are currently custom placeholder agents:

- `memory_context`
- `dialogue`
- `memory_commit`

They are real ADK `agent.Agent` objects, but they do not yet execute the full LLM workflow.

The main `/v1/chat/text` path still uses the local `ADKRunner` placeholder in `internal/agent/types.go`, not a fully wired ADK `runner.Run(...)` invocation.

## Not Yet Done

### Real ADK Runner Integration

Still needed:

```text
Gemini/OpenAI-compatible model
  -> llmagent.New(...)
  -> runner.New(...)
  -> runner.Run(...)
```

The final runner should pass:

```go
runner.Config{
    Agent:          graph.Root,
    SessionService: session.InMemoryService() or persistent implementation,
    MemoryService:  memory.NewADKService(...),
}
```

### Real Dialogue Agent

The `dialogue` placeholder should be replaced with `llmagent.New(...)`.

Expected responsibilities:

- Use current user text
- Use retrieved memory through ADK memory APIs or preload memory tool
- Call tools if needed
- Stream or aggregate assistant response

### Voice Streaming

Still needed:

- WebSocket or gRPC streaming endpoint
- VAD adapter
- Streaming ASR adapter
- Streaming TTS adapter
- Barge-in / interruption control

Current implemented endpoint is text-only for smoke testing.

### Session/Event Storage

Still needed:

- Redis or in-memory session state for active audio turns
- Postgres event log for durable conversation history
- Optional object storage for raw audio

### Tests

Current verification is compile/smoke level:

```bash
go test ./...
curl --noproxy '*' http://127.0.0.1:8080/healthz
```

Still needed:

- mem0 client unit tests with `httptest`
- ADK memory adapter tests
- graph construction tests
- text turn handler tests
- later: streaming voice integration tests

## Current Runtime

The local service is currently expected to run on:

```text
http://127.0.0.1:8080
```

Health endpoint:

```bash
curl --noproxy '*' http://127.0.0.1:8080/healthz
```

Text endpoint:

```bash
curl --noproxy '*' -X POST http://127.0.0.1:8080/v1/chat/text \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"alice","session_id":"demo","text":"我喜欢安静一点的说话方式"}'
```

## Next Recommended Step

Replace the placeholder local `ADKRunner` with a real ADK runner path:

1. Create a real `dialogue` node with `llmagent.New(...)`.
2. Create a `runner.Runner` with `runner.New(...)`.
3. Attach `NewADKService(mem0Client, agentID, topK)` as `runner.Config.MemoryService`.
4. Convert `/v1/chat/text` to call `runner.Run(...)`.
5. Extract final assistant text from emitted ADK `session.Event` values.
