# Development Status

## Current Stack Decision

- Language: Go
- Agent framework: ADK Go package, `google.golang.org/adk/v2`
- Long-term memory: mem0
- Current service entry: `cmd/server/main.go`

Note: the project now resolves `google.golang.org/adk/v2 v2.0.0`.

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

`NewADKService(...)` returns `google.golang.org/adk/v2/memory.Service`.

This matches the ADK source interface:

```go
type Service interface {
    AddSessionToMemory(ctx context.Context, s session.Session) error
    SearchMemory(ctx context.Context, req *SearchRequest) (*SearchResponse, error)
}
```

Source checked:

- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/memory/service.go`

### ADK SessionService Storage

Implemented in `internal/sessionstore/postgres.go`.

The configured ADK session backend is the official ADK v2 `session/database` service with a PostgreSQL GORM dialector. Startup constructs it from `ADK_SESSION_POSTGRES_DSN` and optionally runs `database.AutoMigrate(...)` when `ADK_SESSION_AUTO_MIGRATE=true`.

Use it as the runner session service:

```go
sessionSvc, err := sessionstore.NewPostgresService(sessionstore.PostgresConfig{
    DSN:         cfg.Session.PostgresDSN,
    AutoMigrate: cfg.Session.AutoMigrate,
})

runner.Config{
    Agent:          graph.Root,
    SessionService: sessionSvc,
    MemoryService:  memory.NewADKService(...),
}
```

This moves complete ADK session events out of Go process memory and into PostgreSQL. The optional `ManagedService` wrapper in `internal/sessionstore/managed.go` remains available for active-window experiments, but the main architecture is PostgreSQL-backed SessionService plus mem0-backed MemoryService.

### ADK Graph Skeleton

Implemented in `internal/agent/graph.go` with the official ADK v2 workflow API.

Current ADK workflow graph:

```text
START -> vad -> asr -> memory_context -> llm
llm -> memory_commit
llm -> tts
```

This is built with:

```go
vad := workflow.NewFunctionNode("vad", vadFn, nodeConfig)
asr := workflow.NewFunctionNode("asr", asrFn, nodeConfig)
memoryContext := workflow.NewFunctionNode("memory_context", memoryContextFn, nodeConfig)
llm := workflow.NewFunctionNode("llm", llmFn, nodeConfig)
memoryCommit := workflow.NewFunctionNode("memory_commit", memoryCommitFn, nodeConfig)
tts := workflow.NewFunctionNode("tts", ttsFn, nodeConfig)

edges := workflow.Concat(
    workflow.Chain(workflow.Start, vad, asr, memoryContext, llm),
    []workflow.Edge{
        {From: llm, To: memoryCommit},
        {From: llm, To: tts},
    },
)
root, err := workflowagent.New(workflowagent.Config{
    Name:  rootName,
    Edges: edges,
})
```

Workflow entry semantics are now explicitly documented in `README.md`: the first business node is whichever node is connected from `workflow.Start`. The current graph is single-entry because `workflow.Start -> vad`. If the graph later needs multiple entry nodes, represent that as `workflow.Start` fan-out. Every successor of `workflow.Start` receives the initial user input and may run concurrently. Branches that feed one downstream step should be joined deliberately, and terminal outputs must remain unambiguous because ADK errors when multiple terminal nodes produce final output in the same completed run.

`vad` now calls the optional `internal/vad.AudioProcessor` configured on `GraphConfig`, implemented by `internal/vad.AudioGate`. Non-audio input still passes through so the text smoke-test path keeps working. Audio input uses `vad.Chunk`; the VAD node forwards `vad.AudioOutput{Chunk, VADEnd}` to ASR. On speech start, after 250 ms of confirmed speech, `AudioGate` prepends an 800 ms pre-roll from a 5-second circular audio buffer to avoid losing the beginning of speech. The Silero adapter is included in the default cgo build using the repo-local ONNX Runtime; `CGO_ENABLED=0` builds cannot use `VAD_PROVIDER=silero`.

The post-LLM graph now fans out: `llm -> memory_commit` and `llm -> tts`. `memory_commit` is intentionally side-effect-only and returns no workflow output, so it can write long-term memory without gating TTS. `tts` is the only terminal branch expected to produce user-facing output.

`memory_context` calls ADK v2 `ctx.SearchMemory(...)` and converts the mem0-backed results into the `LLMInput` passed to the `llm` node. Search policy remains centralized in `internal/memory`:

- `Mem0Client.Search(...)` applies identity filters and `topK`.
- `NewADKService(...)` adapts the local mem0 store to ADK v2 `memory.Service`.
- Search failures degrade to an empty memory list so the turn can continue.

Node payloads are text-first but attachment-compatible:

- `TurnContext` carries current turn text and optional `AttachmentRef` values.
- `LLMInput` carries text, long-term memories, and optional attachment refs.
- `LLMOutput` and `TTSOutput` keep response text separate from future blob refs.

Current image memory storage is intentionally out of scope. Future blob storage can be added behind an object-store interface and represented in workflow payloads through `AttachmentRef`.

Source checked:

- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/workflow/function_node.go`
- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/workflow/edgebuilder.go`
- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/agent/workflowagent/workflow.go`

## Important Limitation

The current ADK graph is a skeleton, not the final production graph.

The workflow nodes are currently placeholder function nodes:

- `vad`
- `asr`
- `memory_context`
- `llm`
- `memory_commit` side-effect branch
- `tts` response branch

They are real ADK v2 workflow nodes. The VAD node has a streaming detector interface and Silero adapter, while ASR, LLM, memory commit, and TTS remain placeholder production integrations.

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
    SessionService: sessionstore.NewPostgresService(...),
    MemoryService:  memory.NewADKService(...),
}
```

### Real Dialogue Agent

The `llm` placeholder should be replaced with `llmagent.New(...)` or another model-backed ADK node.

Expected responsibilities:

- Use current user text
- Use retrieved memory through ADK memory APIs or preload memory tool
- Call tools if needed
- Stream or aggregate assistant response

### Streaming VAD

Implemented in `internal/vad`:

- `AudioProcessor` interface for graph-level injection.
- `AudioGate` single 5-second circular audio buffer that accumulates Silero inference windows, snapshots the latest 800 ms on speech start, and emits ASR-facing `AudioOutput{Chunk, VADEnd}` payloads.
- Silero adapter using the repo-local `third_party/silero-vad-go` copy with an exported single-frame `Detector.Infer(...)`, wired through `internal/vad.AudioGate` for true window-by-window processing without replaying full audio history.
- Default cgo builds include the real Silero adapter using the repo-local ONNX Runtime at `third_party/onnxruntime/linux-x64-1.18.1` and the repo-local Silero VAD v5 ONNX model; `CGO_ENABLED=0` builds cannot use `VAD_PROVIDER=silero`.
- Runtime config in `.env.example`: `VAD_PROVIDER`, `SILERO_VAD_MODEL_PATH`, `VAD_SAMPLE_RATE`, `VAD_THRESHOLD`, `VAD_MIN_SILENCE_MS`, `VAD_MIN_SPEECH_MS`, `VAD_SPEECH_PAD_MS`, `VAD_PRE_ROLL_MS`, and `VAD_AUDIO_BUFFER_SECONDS`.
- Unit tests cover single-buffer inference windowing, 800 ms pre-roll audio gating and 250 ms min-speech gating, VAD end forwarding, cross-chunk speech start/end, invalid PCM input, infer error propagation, graph text pass-through, graph audio chunk detector calls, and three real 16 kHz mono WAV fixtures under `internal/vad/testdata`. The real-audio Silero test writes detected speech segments to `tmp/vad-test-segments/<case-name>` for inspection; set `VAD_SEGMENT_TEST_DIR` to override the output path.

The adapter uses 512-sample windows at 16 kHz and 256-sample windows at 8 kHz, matching Silero's supported sample rates while preserving streaming state between chunks.

### Voice Streaming

Still needed:

- WebSocket or gRPC streaming endpoint
- Streaming ASR adapter
- Streaming TTS adapter
- Barge-in / interruption control

Current implemented endpoint is text-only for smoke testing.

### Session/Event Storage

Still needed:

- Tune PostgreSQL indexes/retention once production event volume is known
- Optional Postgres event log for durable conversation history and event-level compaction
- Optional object storage for raw audio

### Tests

Current verification is compile/smoke level plus focused session wrapper unit tests:

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

1. Replace the `llm` placeholder with `llmagent.New(...)` or a model-backed workflow node.
2. Create a `runner.Runner` with `runner.New(...)`.
3. Attach the configured PostgreSQL-backed `session.Service` as `runner.Config.SessionService`.
4. Attach `NewADKService(mem0Client, agentID, topK)` as `runner.Config.MemoryService`.
5. Convert `/v1/chat/text` to call `runner.Run(...)`.
6. Extract final assistant text from emitted ADK `session.Event` values.
