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
    Name:  "voice_agent_graph",
    Edges: edges,
})
```

The workflow edges are:

```text
START -> vad
vad -> asr
asr -> memory_context
memory_context -> llm
llm -> memory_commit
llm -> tts
```

### Workflow Entry Nodes

ADK workflow entry is determined by edges from the special sentinel node `workflow.Start`, not by node creation order or by a node name such as `asr`. In the current graph, this edge makes `vad` the first business node:

```go
edges := workflow.Concat(
    workflow.Chain(workflow.Start, vad, asr, memoryContext, llm),
    []workflow.Edge{
        {From: llm, To: memoryCommit},
        {From: llm, To: tts},
    },
)
```

`workflow.Start` itself does not perform business work. On a fresh run, the workflow scheduler seeds `Start` with the user input, completes `Start`, then schedules every successor reachable through outgoing `Start` edges. The seed input is propagated to those successor nodes.

Multiple entry nodes are valid. Model them as fan-out from `workflow.Start`:

```go
edges := workflow.NewEdgeBuilder().
    AddFanOut(workflow.Start, intentRouter, memoryWarmup, profileLoad).
    Add(intentRouter, join).
    Add(memoryWarmup, join).
    Add(profileLoad, join).
    Add(join, llm).
    Build()
```

In that shape, `intentRouter`, `memoryWarmup`, and `profileLoad` all receive the initial workflow input and may run concurrently unless the workflow is configured with a concurrency limit. If multiple entry branches need to feed one downstream node, use a join/barrier node rather than relying on incidental completion order. Also keep terminal outputs unambiguous: the workflow errors if more than one terminal node produces final output in one completed run.

`vad` is wired to `internal/vad.StreamingDetector`. Non-audio input still passes through so the text smoke-test path keeps working. Audio input should be provided as `vad.Chunk`; when a detector is configured, the node returns a streaming `vad.Decision` with speech start/end state. The Silero implementation is available through `internal/vad.NewSileroStreamingDetector(...)` and is included in the default cgo build using the repo-local ONNX Runtime.

`memory_context` calls ADK v2 `ctx.SearchMemory(...)`. The actual search strategy stays in the mem0-backed memory adapter: identity filters (`user_id`, `agent_id`, `run_id`), configured `topK`, and the selected mem0 mode (`self_hosted` or `cloud_v3`).

After `llm`, the graph fans out to `memory_commit` and `tts`. `memory_commit` is a side-effect-only branch for future long-term memory writes and must not produce the workflow's final output. `tts` remains the user-facing output path, so memory persistence latency or failure should not gate speech/text response generation.

Node payloads use text-first envelopes (`TurnContext`, `LLMInput`, `LLMOutput`) with optional `AttachmentRef` values. Image memory is not implemented now, but future image/audio/video blobs can be attached by reference without changing the workflow shape.

The next concrete replacements are:

```text
asr placeholder -> streaming ASR adapter
llm placeholder -> llmagent.New(...) or model-backed workflow node
tts placeholder -> streaming TTS adapter
```

Then `NewADKService(...)` from `internal/memory/adk_service.go` can be passed to `runner.Config.MemoryService` so mem0 is available through ADK memory APIs.

## Silero Streaming VAD

The VAD abstraction lives in `internal/vad`. The default cgo build includes the real Silero adapter; no `-tags silero` flag is required:

```bash
go test ./internal/vad
```

The repository carries ONNX Runtime v1.18.1 under `third_party/onnxruntime/linux-x64-1.18.1`, and the local `silero-vad-go` cgo directives point at that directory. It also needs the Silero VAD v5 ONNX model path at runtime.

Runtime configuration:

```env
VAD_PROVIDER=silero
SILERO_VAD_MODEL_PATH=models/silero-vad/silero_vad.onnx
VAD_SAMPLE_RATE=16000
VAD_THRESHOLD=0.5
VAD_MIN_SILENCE_MS=200
VAD_SPEECH_PAD_MS=30
```

With `VAD_PROVIDER=none`, no detector is created and the graph VAD node passes input through.

Streaming behavior is implemented by the `internal/vad.Stream` window state machine. The repository carries a local copy of `github.com/streamer45/silero-vad-go` under `third_party/silero-vad-go` and uses a `go.mod` `replace` directive so the Silero detector exposes a single-frame `Infer(...)` method for true online processing:

- 16 kHz: 512 samples per inference window
- 8 kHz: 256 samples per inference window
- input formats: normalized `[]float32` or 16-bit little-endian mono PCM bytes
- incomplete trailing samples remain buffered until the next chunk; full windows are not reprocessed

The stream keeps detector state across chunks and emits `SpeechStarted`, `SpeechEnded`, active state, segment timestamps, processed sample count, and buffered sample count. Three real speech fixtures are checked in under `internal/vad/testdata` and covered by `TestSileroDetectsSpeechFromRealWAV` in the default cgo build. That test writes detected VAD speech segments as WAV files under `tmp/vad-test-segments/<case-name>` by default; override with `VAD_SEGMENT_TEST_DIR` when you want a different inspection directory.

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
