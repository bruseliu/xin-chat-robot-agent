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

`internal/voice.VoiceSession` owns the VAD processor and ASR stream for one client connection. It writes only VAD-gated speech audio to ASR. When VAD emits `VADEnd`, the session finalizes ASR, then starts one ADK text turn with the final transcript.

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

The current ASR provider implementation is `internal/voice.DoubaoASR`, a connection-scoped `StreamingASR` adapter for the Doubao/Volcengine WebSocket ASR protocol. It sends VAD-gated PCM audio chunks and finalizes the ASR stream when VAD emits `VADEnd`.

The next concrete replacements are:

```text
WebSocket audio endpoint -> VoiceSession with per-connection VAD + DoubaoASR
llm placeholder -> llmagent.New(...) or model-backed ADK runner path
TTS placeholder -> transport-level streaming TTS adapter outside ADK graph
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
VAD_MIN_SPEECH_MS=250
VAD_SPEECH_PAD_MS=30
VAD_PRE_ROLL_MS=800
VAD_AUDIO_BUFFER_SECONDS=5
VAD_MAX_SPEECH_MS=15000
```

ASR configuration:

```env
ASR_PROVIDER=doubao
DOUBAO_ASR_URL=wss://openspeech.bytedance.com/api/v3/sauc/bigmodel
DOUBAO_ASR_APP_KEY=<app key>
DOUBAO_ASR_ACCESS_KEY=<access key>
DOUBAO_ASR_RESOURCE_ID=volc.bigasr.sauc.duration
DOUBAO_ASR_MODEL_NAME=bigmodel
DOUBAO_ASR_LANGUAGE=zh-CN
DOUBAO_ASR_SAMPLE_RATE=16000
DOUBAO_ASR_ENABLE_PUNC=true
DOUBAO_ASR_COMPRESS_AUDIO=true
DOUBAO_ASR_TIMEOUT_SECONDS=30
```

With `VAD_PROVIDER=none`, no detector is created; audio sessions must provide a VAD processor before they can process microphone input.

Streaming detection and ASR-facing audio gating are implemented by `internal/vad.AudioGate`, which maintains one 5-second circular audio buffer. The same buffer is used to accumulate Silero inference windows, snapshot the 800 ms speech-start pre-roll, and emit only `vad.AudioOutput{Chunk, VADEnd}` to the connection-level ASR stream. The repository carries a local copy of `github.com/streamer45/silero-vad-go` under `third_party/silero-vad-go` and uses a `go.mod` `replace` directive so the Silero detector exposes a single-frame `Infer(...)` method for true online processing:

- 16 kHz: 512 samples per inference window
- 8 kHz: 256 samples per inference window
- input formats: normalized `[]float32` or 16-bit little-endian mono PCM bytes
- incomplete trailing samples remain in the same circular audio buffer until enough samples exist for the next inference window; full windows are not reprocessed
- speech-start output requires 250 ms of continuous speech and includes the previous 800 ms of audio from a 5-second circular buffer plus the current speech-start chunk
- continuous active speech is force-cut at `VAD_MAX_SPEECH_MS` by emitting `VADEnd`, so ASR can flush long utterances instead of buffering indefinitely

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
2. Validate `/v1/chat/voice` against the real Doubao ASR service.
3. Implement streaming TTS adapter.
4. Replace `echoPlanner` with a real ADK runner using the configured PostgreSQL SessionService and mem0 MemoryService.
4. Replace `echoPlanner` path with real ADK runner using the configured PostgreSQL SessionService and mem0 MemoryService.
