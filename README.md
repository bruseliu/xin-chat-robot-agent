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


## Runtime Architecture

The realtime audio path is handled by a connection-level runtime, not by ADK workflow edges:

```text
client audio stream
  -> VoiceSession
      -> VAD processor
      -> ASR stream
      -> ASR final transcript
  -> ADK runner / agent graph
      -> memory context
      -> LLM / tools / session events
      -> memory commit
  -> VoiceSession output layer
      -> TTS streaming
      -> client audio
```

`internal/voice.VoiceSession` owns one VAD processor and ASR stream per client connection. The selected FunASR path uses Silero as the sentence-boundary authority: only local-VAD speech audio is written to the long-lived FunASR WebSocket, and every `VADEnd` sends `SEGMENT_END` to force-decode that utterance without closing the connection. Each returned sentence starts one ADK text turn. The client `final` control message sends `STOP` only when the entire microphone session ends.

`GET /v1/chat/voice` is the current WebSocket entry point. It expects `user_id` and optional `session_id`/`sample_rate` query parameters. Binary messages are treated as PCM16LE audio chunks; a text control message `{"type":"final"}` flushes the current audio stream. When ASR returns a final transcript, the server responds with a JSON `VoiceTurnOutput`.

LLM belongs inside the ADK runner because it needs memory, tools, session state, and reasoning events. TTS stays outside the ADK graph: the connection runtime consumes assistant text events and streams them to TTS/audio output.

## ADK Graph

The current ADK graph is built in `internal/agent/graph.go` with the official ADK v2 workflow API. It now represents the text reasoning layer only:

```text
START -> turn_context -> memory_context -> llm -> memory_commit
```

`memory_context` calls ADK v2 `ctx.SearchMemory(...)`. The actual search strategy stays in the mem0-backed memory adapter: identity filters (`user_id`, `agent_id`, `run_id`), configured `topK`, and the selected mem0 mode (`self_hosted` or `cloud_v3`).

`memory_commit` is side-effect-only. Response audio is not a workflow terminal node; TTS is driven by the outer `VoiceSession`/transport layer so LLM-to-TTS streaming does not depend on ADK edge scheduling.

Node payloads use text-first envelopes (`TurnContext`, `LLMInput`, `LLMOutput`) with optional `AttachmentRef` values. Image memory is not implemented now, but future image/audio/video blobs can be attached by reference without changing the reasoning graph shape.

The selected ASR implementation is `internal/voice.FunASRASR`, a connection-scoped adapter for the private Fun-ASR-Nano real-time WebSocket service. It sends `START`, receives Silero-gated PCM frames, maps deduplicated `sentences[]` items to final user utterances, and sends `SEGMENT_END` at each local VAD boundary. It sends `STOP` only when the client finishes the entire microphone stream. `AliyunNLSASR` and `DoubaoASR` remain available as cloud fallbacks.

The next concrete replacements are:

```text
llm placeholder -> llmagent.New(...) or model-backed ADK runner path
TTS placeholder -> transport-level streaming TTS adapter outside ADK graph
/v1/chat/voice smoke path -> real microphone WebSocket client validation
```

Then `NewADKService(...)` from `internal/memory/adk_service.go` can be passed to `runner.Config.MemoryService` so mem0 is available through ADK memory APIs.

## Silero Streaming VAD

The VAD abstraction lives in `internal/vad`. The default cgo build includes the real Silero adapter; no `-tags silero` flag is required:

```bash
go test ./internal/vad
```

The repository carries ONNX Runtime v1.18.1 under `third_party/onnxruntime/linux-x64-1.18.1`, and the local `silero-vad-go` cgo directives point at that directory. It also needs the Silero VAD v5 ONNX model path at runtime.

Runtime configuration for the selected FunASR path:

```env
VAD_PROVIDER=silero
SILERO_VAD_MODEL_PATH=models/silero-vad/silero_vad.onnx
VAD_SAMPLE_RATE=16000
VAD_THRESHOLD=0.5
VAD_MIN_SILENCE_MS=350
VAD_MIN_SPEECH_MS=250
VAD_SPEECH_PAD_MS=30
VAD_PRE_ROLL_MS=800
VAD_AUDIO_BUFFER_SECONDS=5
VAD_MAX_SPEECH_MS=15000
```

For the selected FunASR path, keep `VAD_PROVIDER=silero`: it owns utterance boundaries and drives `SEGMENT_END`. `passthrough` is only for provider-owned endpointing experiments.

ASR configuration:

```env
ASR_PROVIDER=funasr
FUNASR_URL=ws://127.0.0.1:10095
FUNASR_SAMPLE_RATE=16000
FUNASR_TIMEOUT_SECONDS=30
```

Start the private service with [deploy/funasr/README.md](deploy/funasr/README.md).

With `VAD_PROVIDER=none`, no detector is created; audio sessions must provide a VAD processor before they can process microphone input.

Streaming detection and ASR-facing audio gating are implemented by `internal/vad.AudioGate`, which maintains one 5-second circular audio buffer. The same buffer is used to accumulate Silero inference windows, snapshot the 800 ms speech-start pre-roll, and emit only `vad.AudioOutput{Chunk, VADEnd}` to the connection-level ASR stream. The repository carries a local copy of `github.com/streamer45/silero-vad-go` under `third_party/silero-vad-go` and uses a `go.mod` `replace` directive so the Silero detector exposes a single-frame `Infer(...)` method for true online processing:

- 16 kHz: 512 samples per inference window
- 8 kHz: 256 samples per inference window
- input formats: normalized `[]float32` or 16-bit little-endian mono PCM bytes
- incomplete trailing samples remain in the same circular audio buffer until enough samples exist for the next inference window; full windows are not reprocessed
- speech-start output requires 250 ms of continuous speech and includes the previous 800 ms of audio from a 5-second circular buffer plus the current speech-start chunk
- continuous active speech is force-cut at `VAD_MAX_SPEECH_MS` by emitting `VADEnd`; in the current voice runtime this marks a segment boundary and does not close the ASR connection by itself

`VADEnd` is a segment boundary, not an ASR connection boundary. The voice runtime can forward several `AudioOutput.Chunk` values across several VAD segments into one ASR connection and call `Finalize(...)` only when the full audio input is complete.

Three real speech fixtures are checked in under `internal/vad/testdata` and covered by `TestSileroDetectsSpeechFromRealWAV` in the default cgo build. The full VAD+ASR real-service smoke test is gated by environment variables:

```bash
set -a
source .env
set +a

VAD_ASR_REAL_TEST=1 \
SILERO_VAD_MODEL_PATH=models/silero-vad/silero_vad.onnx \
go test ./internal/voice -run TestSileroVADToDoubaoASRRealService -count=1 -v
```

That test uses the repo-local Silero VAD, writes all VAD-gated chunks from one WAV into a single Doubao ASR connection, logs partial ASR results from `DoubaoASR.Transcripts()`, then finalizes once at whole-audio end. For WAV-file testing it paces packets by default to match the official `sauc_go` sample's `segmentDuration` ticker; set `VAD_ASR_REAL_PACE=false` to disable pacing. `VAD_ASR_REAL_CHUNK_DURATION` controls chunk size and defaults to `200ms`.

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

1. Validate `/v1/chat/voice` end to end with the configured Postgres and LLM services, using a real microphone WebSocket client.
2. Add a local ASR health check and restart policy.
3. Implement the streaming TTS adapter outside the ADK graph.
4. Add barge-in / interruption control.
