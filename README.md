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
- `internal/voice`: voice pipeline boundary for ASR/TTS/VAD. Text turn handling is implemented first as the narrowest testable path.
- `internal/api`: HTTP API entry point.


## ADK Graph

The current ADK graph is built in `internal/agent/graph.go` with the official ADK v2 workflow API:

```go
memoryContext := workflow.NewFunctionNode("memory_context", memoryContextFn, nodeConfig)
dialogue := workflow.NewFunctionNode("dialogue", dialogueFn, nodeConfig)
memoryCommit := workflow.NewFunctionNode("memory_commit", memoryCommitFn, nodeConfig)

edges := workflow.Chain(workflow.Start, memoryContext, dialogue, memoryCommit)
root, err := workflowagent.New(workflowagent.Config{
    Name:  "voice_agent_graph",
    Edges: edges,
})
```

The workflow edges are:

```text
START -> memory_context
memory_context -> dialogue
dialogue -> memory_commit
```

The next concrete replacement is:

```text
dialogue placeholder -> llmagent.New(...) -> runner.New(...)
```

Then `NewADKService(...)` from `internal/memory/adk_service.go` can be passed to `runner.Config.MemoryService` so mem0 is available through ADK memory APIs.

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
4. Add Redis/Postgres session/event storage.
