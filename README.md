# Xin Chat Robot Agent

Go service skeleton for a streaming voice chatbot agent.

Target architecture:

```text
audio stream -> VAD -> streaming ASR -> ADK agent -> streaming TTS
                              |
                              +-> mem0 long-term memory
```

The first commit keeps external systems behind local interfaces:

- `internal/agent`: ADK runner boundary. The current runner is a placeholder planner so the service can compile before model credentials are wired.
- `internal/memory`: mem0 HTTP client. It supports the self-hosted server in `/data/liuxin/mem0/server` and mem0 Cloud v3.
- `internal/sessionstore`: ADK v2 PostgreSQL-backed `session.Service` construction and optional active-window helpers.
- `internal/voice`: voice pipeline boundary for ASR/TTS/VAD. Text turn handling is implemented first as the narrowest testable path.
- `internal/api`: HTTP API entry point.


## ADK Graph

The current ADK graph is built in `internal/agent/graph.go` with the official ADK v2 workflow API. The main voice pipeline stages are first-class workflow nodes:

```go
asr := workflow.NewFunctionNode("asr", asrFn, nodeConfig)
memoryContext := workflow.NewFunctionNode("memory_context", memoryContextFn, nodeConfig)
llm := workflow.NewFunctionNode("llm", llmFn, nodeConfig)
memoryCommit := workflow.NewFunctionNode("memory_commit", memoryCommitFn, nodeConfig)
tts := workflow.NewFunctionNode("tts", ttsFn, nodeConfig)

edges := workflow.Chain(workflow.Start, asr, memoryContext, llm, memoryCommit, tts)
root, err := workflowagent.New(workflowagent.Config{
    Name:  "voice_agent_graph",
    Edges: edges,
})
```

The workflow edges are:

```text
START -> asr
asr -> memory_context
memory_context -> llm
llm -> memory_commit
memory_commit -> tts
```

`memory_context` calls ADK v2 `ctx.SearchMemory(...)`. The actual search strategy stays in the mem0-backed memory adapter: identity filters (`user_id`, `agent_id`, `run_id`), configured `topK`, and the selected mem0 mode (`self_hosted` or `cloud_v3`).

Node payloads use text-first envelopes (`TurnContext`, `LLMInput`, `LLMOutput`) with optional `AttachmentRef` values. Image memory is not implemented now, but future image/audio/video blobs can be attached by reference without changing the workflow shape.

The next concrete replacements are:

```text
asr placeholder -> streaming ASR adapter
llm placeholder -> llmagent.New(...) or model-backed workflow node
tts placeholder -> streaming TTS adapter
```

Then `NewADKService(...)` from `internal/memory/adk_service.go` can be passed to `runner.Config.MemoryService` so mem0 is available through ADK memory APIs.

## ADK Session Storage

Short-term ADK session state is configured as a PostgreSQL-backed ADK v2 `session.Service` using the official `session/database` package:

```go
sessionSvc, err := sessionstore.NewPostgresService(sessionstore.PostgresConfig{
    DSN:         cfg.Session.PostgresDSN,
    AutoMigrate: cfg.Session.AutoMigrate,
})
```

The service is intended to be passed to ADK runner together with the mem0-backed memory service:

```go
runner.Config{
    Agent:          graph.Root,
    SessionService: sessionSvc,
    MemoryService:  memory.NewADKService(mem0Client, agentID, topK),
}
```

This keeps complete ADK session events in PostgreSQL instead of growing Go process memory through `session.InMemoryService()`. The current text endpoint still uses the placeholder runner, but startup now constructs both ADK services so the real runner can use them directly.

## mem0 Modes

Self-hosted mem0 server from `/data/liuxin/mem0/server`:

```env
MEM0_MODE=self_hosted
MEM0_BASE_URL=http://localhost:8888
MEM0_API_KEY=<server api key>
```

This uses:

- `POST /search`
- `POST /memories`
- `X-API-Key: <key>`

mem0 Cloud v3:

```env
MEM0_MODE=cloud_v3
MEM0_BASE_URL=https://api.mem0.ai
MEM0_API_KEY=<cloud api key>
```

This uses:

- `POST /v3/memories/search/`
- `POST /v3/memories/add/`
- `Authorization: Token <key>`

## Run

```bash
cp .env.example .env
go run ./cmd/server
```

Health check:

```bash
curl http://localhost:8080/healthz
```

Text-turn smoke test:

```bash
curl -X POST http://localhost:8080/v1/chat/text \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"alice","session_id":"demo","text":"我喜欢安静一点的说话方式"}'
```

## Next Work

1. Replace `echoPlanner` with a real ADK 2.0 Go runner.
2. Add WebSocket or gRPC streaming audio endpoint.
3. Implement ASR/TTS adapters.
4. Replace `echoPlanner` path with real ADK runner using the configured PostgreSQL SessionService and mem0 MemoryService.
