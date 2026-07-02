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

The current ADK graph is built in `internal/agent/graph.go` with ADK workflow agents:

```text
voice_agent_graph  // sequentialagent
  -> memory_context  // ADK MemoryService context load boundary
  -> dialogue        // main LLM agent placeholder
  -> memory_commit   // completed turn/session memory write boundary
```

In ADK Go there is no explicit `AddEdge(from, to)` API for this workflow shape. The edges are implied by the agent type and child order:

- `sequentialagent.New(...)` defines sequential execution semantics.
- `agent.Config.SubAgents` defines the ordered child list.
- The execution edges are therefore `memory_context -> dialogue -> memory_commit`.
- `google.golang.org/adk/memory.Service` provides the long-term memory boundary.

For readability, `internal/agent/graph.go` also keeps an explicit local edge list:

```text
voice_agent_graph -> memory_context (contains)
voice_agent_graph -> dialogue (contains)
voice_agent_graph -> memory_commit (contains)
memory_context -> dialogue (sequential_next)
dialogue -> memory_commit (sequential_next)
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
