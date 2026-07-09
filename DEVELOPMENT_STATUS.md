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

### Runtime Architecture

The realtime voice path has been moved out of ADK workflow edges and into a connection-level runtime:

```text
client audio stream
  -> internal/voice.VoiceSession
      -> VAD processor
      -> ASR stream
      -> ASR final transcript
  -> ADK runner / agent graph
      -> memory_context
      -> llm / tools / session events
      -> memory_commit
  -> outer output layer
      -> streaming TTS
      -> client audio
```

`VoiceSession` owns the per-connection VAD processor and `StreamingASR` stream. It writes VAD-gated speech audio to ASR, treats `VADEnd` as a VAD segment boundary, and calls `Finalize(...)` only when the whole client audio stream ends. This lets several VAD speech segments share one ASR connection before the final transcript is passed to the existing text turn path, which is the boundary where ADK starts.

`internal/voice.DoubaoASR` implements `StreamingASR` for Doubao/Volcengine WebSocket ASR. Runtime configuration is loaded from `ASR_PROVIDER=doubao` plus the `DOUBAO_ASR_*` environment variables in `.env.example`. It exposes streaming recognition updates through `Transcripts() <-chan Transcript` while `Finalize(...)` still waits for the service-side final response.

`GET /v1/chat/voice` now creates a per-connection VAD detector and ASR stream, constructs `VoiceSession`, accepts PCM16LE binary WebSocket messages, forwards only VAD-gated chunks to ASR, and emits JSON `VoiceTurnOutput` only after whole-stream ASR finalization.

This avoids relying on ADK workflow edges as realtime pipes. ADK workflow successors are scheduled after upstream node completion, so continuous VAD->ASR and LLM->TTS streams are handled by the voice runtime/transport layer instead.

### ADK Graph Skeleton

Implemented in `internal/agent/graph.go` with the official ADK v2 workflow API.

Current ADK workflow graph:

```text
START -> turn_context -> memory_context -> llm -> memory_commit
```

`memory_context` calls ADK v2 `ctx.SearchMemory(...)` and converts the mem0-backed results into the `LLMInput` passed to the `llm` node. Search policy remains centralized in `internal/memory`:

- `Mem0Client.Search(...)` applies identity filters and `topK`.
- `NewADKService(...)` adapts the local mem0 store to ADK v2 `memory.Service`.
- Search failures degrade to an empty memory list so the turn can continue.

`memory_commit` is intentionally side-effect-only and returns no workflow output, so it can write long-term memory without gating response streaming. TTS is no longer represented as a normal ADK terminal node; the outer connection runtime consumes assistant text events and drives streaming TTS.

Node payloads are text-first but attachment-compatible:

- `TurnContext` carries current turn text and optional `AttachmentRef` values.
- `LLMInput` carries text, long-term memories, and optional attachment refs.
- `LLMOutput` keeps response text separate from future blob refs.

Current image memory storage is intentionally out of scope. Future blob storage can be added behind an object-store interface and represented in workflow payloads through `AttachmentRef`.

Source checked:

- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/workflow/function_node.go`
- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/workflow/edgebuilder.go`
- `/home/liuxin/go/pkg/mod/google.golang.org/adk/v2@v2.0.0/agent/workflowagent/workflow.go`

## Important Limitation

The current ADK graph is a skeleton, not the final production graph.

The ADK workflow nodes are currently placeholder function nodes for the text reasoning layer:

- `turn_context`
- `memory_context`
- `llm`
- `memory_commit` side-effect branch

They are real ADK v2 workflow nodes. VAD/ASR streaming now lives in `internal/voice.VoiceSession`, while LLM, memory commit, and provider-backed streaming output remain placeholder production integrations.

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

- `AudioProcessor` interface for connection-level voice runtime injection.
- `AudioGate` single 5-second circular audio buffer that accumulates Silero inference windows, snapshots the latest 800 ms on speech start, and emits ASR-facing `AudioOutput{Chunk, VADEnd}` payloads.
- Silero adapter using the repo-local `third_party/silero-vad-go` copy with an exported single-frame `Detector.Infer(...)`, wired through `internal/vad.AudioGate` for true window-by-window processing without replaying full audio history.
- Default cgo builds include the real Silero adapter using the repo-local ONNX Runtime at `third_party/onnxruntime/linux-x64-1.18.1` and the repo-local Silero VAD v5 ONNX model; `CGO_ENABLED=0` builds cannot use `VAD_PROVIDER=silero`.
- Runtime config in `.env.example`: `VAD_PROVIDER`, `SILERO_VAD_MODEL_PATH`, `VAD_SAMPLE_RATE`, `VAD_THRESHOLD`, `VAD_MIN_SILENCE_MS`, `VAD_MIN_SPEECH_MS`, `VAD_SPEECH_PAD_MS`, `VAD_PRE_ROLL_MS`, `VAD_AUDIO_BUFFER_SECONDS`, and `VAD_MAX_SPEECH_MS`.
- Unit tests cover single-buffer inference windowing, 800 ms pre-roll audio gating and 250 ms min-speech gating, VAD end forwarding, continuous-speech force cuts, cross-chunk speech start/end, invalid PCM input, infer error propagation, VoiceSession forwarding multiple VAD segments into one ASR stream, `/v1/chat/voice` WebSocket session wiring, Doubao ASR WebSocket packet lifecycle/parsing, streaming transcript events, and real 16 kHz mono WAV fixtures under `internal/vad/testdata`. The real-audio Silero test writes detected VAD speech segments to `tmp/vad-test-segments/<case-name>` for inspection; set `VAD_SEGMENT_TEST_DIR` to override the output path.

The adapter uses 512-sample windows at 16 kHz and 256-sample windows at 8 kHz, matching Silero's supported sample rates while preserving streaming state between chunks. Continuous active speech is force-cut at `VAD_MAX_SPEECH_MS` by emitting `VADEnd`; in the current voice runtime this is a segment marker and does not close the ASR connection by itself.

### Voice Streaming

Implemented/validated:

- `DoubaoASR` follows the official `/home/liuxin/sauc_go` packet shape for audio-only messages: positive audio packets use flag `0b0001` with a positive sequence; the final audio packet uses flag `0b0011` with a negative sequence.
- `DoubaoASR.Transcripts()` exposes partial/final service responses for realtime display while audio is still being uploaded.
- `TestSileroVADToDoubaoASRRealService` runs a gated real-service smoke path: WAV -> Silero VAD -> multiple VAD chunks/segments -> one Doubao ASR connection -> one final transcript at whole-audio end.
- WAV-file real-service tests pace packets by default with a `200ms` segment duration to match the official `sauc_go` ticker example. Disable with `VAD_ASR_REAL_PACE=false` or `DOUBAO_ASR_REAL_PACE=false` when intentionally stress-testing fast upload.

Current ASR product direction:

- The target voice UX is now a single long-lived ASR connection per client microphone stream. The ASR provider should perform endpointing/sentence detection, emit a final result for each spoken sentence, and only close the recognition task when the client stops the whole audio stream.
- This shifts the preferred path away from local VAD-gated ASR segments. Local VAD/Silero can remain available as an optional gate or fallback, but the main provider integration should support service-side sentence boundaries from the raw microphone stream, including silence.
- Alibaba Cloud NLS `SpeechTranscriber` is the best protocol match found so far: one WebSocket, `StartTranscription`, continuous binary audio frames, `SentenceBegin` / `TranscriptionResultChanged` / `SentenceEnd`, then `StopTranscription` / `TranscriptionCompleted`. `SentenceEnd` maps directly to a user utterance final transcript, while `TranscriptionCompleted` maps to whole-stream shutdown.
- Tencent Cloud ASR WebSocket is also viable for this no-local-VAD mode. It exposes `result.slice_type=2` for a stable sentence/end-of-utterance result and top-level `final=1` for the whole audio stream after sending `{"type":"end"}`. Configure Tencent service-side endpointing with `needvad=1` and `vad_silence_time`.
- iFlytek RTASR is a possible secondary option. It uses a long WebSocket stream with result `type=0` for final results and a final `{"end": true}` upload marker for whole-stream completion. Its event model is less explicit than Alibaba NLS `SentenceEnd`.
- Doubao/Volcengine SAUC remains implemented, but it is not the preferred fit for the updated requirement until a real-service smoke test proves stable multi-sentence final events on one connection without using client-side negative sequence packets as per-segment endings. The current adapter is still useful for segment-scoped or whole-stream finalization.

Still needed:

- Choose and implement the primary service-side endpointing ASR provider, with Alibaba NLS as the current default recommendation and Tencent Cloud ASR as the next candidate.
- Adjust `VoiceSession` so service-side sentence final events can trigger one text turn while the ASR connection remains open until whole-stream completion.
- Validate `/v1/chat/voice` end to end with a real WebSocket client and real microphone stream.
- Streaming TTS adapter.
- Barge-in / interruption control.

### Session/Event Storage

Still needed:

- Tune PostgreSQL indexes/retention once production event volume is known
- Optional Postgres event log for durable conversation history and event-level compaction
- Optional object storage for raw audio

### Tests

Current verification includes normal unit tests plus gated real-service ASR/VAD+ASR tests:

```bash
go test ./...

set -a
source .env
set +a

VAD_ASR_REAL_TEST=1 \
SILERO_VAD_MODEL_PATH=models/silero-vad/silero_vad.onnx \
go test ./internal/voice -run TestSileroVADToDoubaoASRRealService -count=1 -v
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
