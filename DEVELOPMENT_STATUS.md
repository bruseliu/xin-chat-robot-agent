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

`VoiceSession` now has two paths. Legacy providers use local VAD-gated segments. The selected private FunASR path uses `VAD_PROVIDER=passthrough`, forwards raw PCM including silence to one long-lived ASR task, starts a text turn for every locked FunASR sentence, and calls `Finalize(...)` only when the whole client stream ends.

`internal/voice.FunASRASR` implements the selected private long-lived provider path for Fun-ASR-Nano. Runtime configuration uses `ASR_PROVIDER=funasr` plus `FUNASR_*` variables. It sends `START`, accepts continuous PCM binary frames, emits final transcripts from locked `sentences[]` items, then sends `STOP` at whole-stream shutdown. `AliyunNLSASR` and `DoubaoASR` remain available as fallback providers.

`GET /v1/chat/voice` creates a per-connection VAD processor and ASR stream. For FunASR it asynchronously emits one JSON `VoiceTurnOutput` per locked sentence while continuing to accept microphone PCM, and uses the client `final` control message only for whole-stream shutdown.

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
- The selected primary provider is private `FunAudioLLM/Fun-ASR-Nano-2512`: its official streaming WebSocket server supplies service-side endpointing and runs as one RTX 5080 16 GB deployment with speaker diarization disabled.
- Alibaba Cloud NLS `SpeechTranscriber` is the best protocol match found so far: one WebSocket, `StartTranscription`, continuous binary audio frames, `SentenceBegin` / `TranscriptionResultChanged` / `SentenceEnd`, then `StopTranscription` / `TranscriptionCompleted`. `SentenceEnd` maps directly to a user utterance final transcript, while `TranscriptionCompleted` maps to whole-stream shutdown.
- Tencent Cloud ASR WebSocket is also viable for this no-local-VAD mode. It exposes `result.slice_type=2` for a stable sentence/end-of-utterance result and top-level `final=1` for the whole audio stream after sending `{"type":"end"}`. Configure Tencent service-side endpointing with `needvad=1` and `vad_silence_time`.
- iFlytek RTASR is a possible secondary option. It uses a long WebSocket stream with result `type=0` for final results and a final `{"end": true}` upload marker for whole-stream completion. Its event model is less explicit than Alibaba NLS `SentenceEnd`.
- Doubao/Volcengine SAUC remains implemented, but it is not the preferred fit for the updated requirement until a real-service smoke test proves stable multi-sentence final events on one connection without using client-side negative sequence packets as per-segment endings. The current adapter is still useful for segment-scoped or whole-stream finalization.

Implemented:

- Private Fun-ASR-Nano-2512 is now the primary ASR provider through `FunASRASR`; unit tests verify the START -> audio -> locked sentence -> STOP WebSocket lifecycle.
- `deploy/funasr/run-funasr-nano.sh` starts the official service with one GPU, loopback-only binding, and a vLLM sampler compatible with hosts that lack `nvcc`; a real 16 kHz WAV smoke test has verified final sentence output from the local service.
- `VoiceSession` keeps the FunASR connection open across locked sentences and starts one text turn for each new sentence.
- `VAD_PROVIDER=passthrough` forwards raw microphone PCM, including silence, so FunASR performs endpointing.
- Partial transcript streaming: `FunASRASR` and `AliyunNLSASR` both implement `PartialTranscripts() <-chan Transcript`. `VoiceSession.consumeServerEndpointingTranscripts` selects on both sentences and partials channels, emitting `VoiceTurnOutput{Type: "partial"}` for real-time display.

### Hybrid VAD + SEGMENT_END (2026-07-10)

**Problem**: FunASR 内置的 FSMN-VAD 在带噪音的音频上无法检测静音帧（神经网络模型限制，不是参数问题 — 即使 `speech_noise_thres=0.1` + `silence_schedule=[(inf, 200)]` 也不行）。而用 `STOP` + 重连的方式每句有建连开销（WebSocket 握手 + START 等待）。

**设计决策**: 区分"VAD 断句"和"音频流结束"为两个独立概念：
- `SEGMENT_END`（文本消息）= 客户端本地 VAD 检测到一句话说完了，要求服务端 decode 当前段并返回 sentence，但连接不断
- `STOP`（文本消息）= 整个音频会话结束，关闭连接

**协议流程**:
```
客户端                              服务端
  |-- START ----------------------->|
  |<-- {"event":"started"} ---------|
  |-- binary PCM (speech) --------->|  (Silero VAD 检测到语音，发送音频)
  |<-- {"partial":"一二三..."} ------|  (定时 decode 返回中间结果)
  |-- SEGMENT_END ----------------->|  (Silero VAD 检测到 speech end)
  |<-- {"event":"segment_end",      |  (服务端 force decode + lock sentence)
  |     "sentences":[...]} ---------|
  |-- binary PCM (next speech) ---->|  (下一段语音，同一连接)
  |<-- {"partial":"你好..."} --------|
  |-- SEGMENT_END ----------------->|
  |<-- {"event":"segment_end",...} --|
  |-- STOP ------------------------>|  (会话彻底结束)
  |<-- {"is_final":true,...} --------|
  |<-- {"event":"stopped"} ---------|
```

**注意**: `segment_end` 响应中的 `sentences` 数组是**累积的**（包含本次会话所有已 lock 的句子），客户端需要去重。Go 客户端通过 `seen` map（key = `start:end:text`）做去重。

---

#### 服务端改动 (`/data/liuxin/Fun-ASR/serve_realtime_ws.py`)

Git commit: `3f95db6` on `/data/liuxin/Fun-ASR` main branch.

**1. `RealtimeASRSession.force_segment_end()` 方法** (line ~386):

```python
def force_segment_end(self):
    # 如果当前没有活跃语音段，直接返回
    if self.vad.current_speech_start is None:
        return self._build_response(is_final=False)

    # 计算当前段的 end 时间 = buffer 总长度对应的毫秒数
    end_ms = int(len(self.audio_buffer) * 1000 / self.sample_rate)
    seg = [self.vad.current_speech_start, end_ms]

    # 用 vLLM 推理 decode 整个段
    seg_text = self._decode_segment(seg)
    if seg_text.strip():
        self.locked_sentences.append({"text": seg_text, "start": int(seg[0]), "end": int(seg[1])})

    # 重置 VAD 状态，为下一段做准备
    self.vad.current_speech_start = None
    self.prev_text = ""
    self.last_partial_text = ""
    self.first_decode_done = False
    self._force_speech_on_next_audio = True  # 关键：下一个 audio 进来直接 mark speech start
    return self._build_response(is_final=False)
```

**2. `_force_speech_on_next_audio` 机制** (line ~253 in `add_audio`):

SEGMENT_END 之后 FSMN-VAD 的 `current_speech_start` 被清空了。如果等 FSMN-VAD 自己重新检测 speech start，在噪音音频上它永远检不到。所以加了一个 flag：下一个 audio chunk 到达时，立即设置 `current_speech_start = 当前位置`，不依赖 FSMN-VAD。

```python
def add_audio(self, pcm_bytes):
    ...
    if self._force_speech_on_next_audio and self.vad.current_speech_start is None:
        self.vad.current_speech_start = <当前 buffer 位置对应的 ms>
        self._force_speech_on_next_audio = False
    ...
```

这在"客户端用 Silero VAD gate 音频、只发 speech 帧"的场景下是正确的 — SEGMENT_END 之后到达的下一个 audio 一定是新语音。

**3. `handle_client` 命令处理** (line ~514):

```python
elif cmd.upper() == "SEGMENT_END":
    if session.is_active:
        result = await asyncio.to_thread(session.force_segment_end)
        result["event"] = "segment_end"
        await websocket.send(json.dumps(result))
```

**4. 其他改动** (同一 commit):
- `--vad-silence-schedule` 参数：动态 silence 检测时长，格式 `limit_ms:silence_ms,...`（例如 `inf:200`）
- `--vad-speech-noise-thres` 参数：FSMN-VAD 语音/噪声阈值（默认 0.5，越低越敏感）
- 绑定 `FUNASR_BIND_HOST` 环境变量（默认 `127.0.0.1`，不再 `0.0.0.0`）

---

#### Go 客户端改动 (`internal/voice/funasr_asr.go`)

**`NotifySegmentEnd` 方法**:

```go
func (s *FunASRASR) NotifySegmentEnd(ctx context.Context) error {
    s.writeMu.Lock()
    defer s.writeMu.Unlock()
    if s.isClosed() || s.isStopping() {
        return fmt.Errorf("funasr stream not available")
    }
    return s.writeMessage(ctx, websocket.TextMessage, []byte("SEGMENT_END"))
}
```

**去重逻辑** (已有，在 `publishSentence` 中):

```go
func (s *FunASRASR) publishSentence(sentence funASRSentence) {
    key := fmt.Sprintf("%d:%d:%s", sentence.Start, sentence.End, text)
    if _, seen := s.seen[key]; seen {
        return  // 跳过重复
    }
    s.seen[key] = struct{}{}
    // ... 发送到 sentences channel
}
```

`readLoop` 中 `segment_end` 事件的 sentences 会走正常的 `publishSentence` 路径，自动去重。

---

#### Session 集成 (`internal/voice/session.go`)

**新增接口**:

```go
// SegmentEndNotifier 由支持客户端 VAD 驱动断句的 ASR provider 实现
type SegmentEndNotifier interface {
    NotifySegmentEnd(ctx context.Context) error
}
```

**ProcessAudio 中的逻辑** (endpointing 模式):

```go
if s.endpointing != nil {
    if chunk.Final {
        // 整个音频流结束
        s.asr.Finalize(ctx)
        return VoiceTurnOutput{Final: true}, nil
    }
    if vadOut.VADEnd {
        // 本地 Silero VAD 检测到断句 → 通知服务端
        if notifier, ok := s.endpointing.(SegmentEndNotifier); ok {
            notifier.NotifySegmentEnd(ctx)
        }
    }
    return VoiceTurnOutput{Final: false}, nil
}
```

这意味着 `FunASRASR`（实现了 `SegmentEndNotifier`）会在 Silero VAD 断句时收到 `SEGMENT_END`。`AliyunNLSASR` 没有实现这个接口，所以对它无影响。

---

#### 运行时架构 (整体数据流)

```
麦克风 PCM 16kHz
  → WebSocket /v1/chat/voice
  → VoiceSession.ProcessAudio()
      → Silero VAD (gate: 只在 speech 时输出音频, VADEnd=true 表示一句话说完)
          → 有音频 → FunASRASR.Write() → WebSocket binary → FunASR 服务
          → VADEnd  → FunASRASR.NotifySegmentEnd() → "SEGMENT_END" 文本消息
  → FunASR 服务返回:
      → partial → partials channel → VoiceTurnOutput{Type:"partial"}
      → segment_end sentences → sentences channel → 去重 → VoiceTurnOutput{Type:"sentence"}
          → Pipeline.HandleTextTurn() → LLM → response
```

---

#### 测试验证

**测试脚本**:
- `scripts/funasr-segment-end` — 协议级验证：对半切音频 + SEGMENT_END + 继续发送 + STOP
- `scripts/vad-funasr-e2e` — 完整链路：Silero VAD 驱动 + FunASR + SEGMENT_END + 延迟测量 + 去重

**运行命令**:
```bash
# 协议验证
go run ./scripts/funasr-segment-end internal/vad/testdata/test2.wav

# 完整 E2E (Silero VAD + FunASR)
go run ./scripts/vad-funasr-e2e internal/vad/testdata/test2.wav
```

**E2E 测试结果** (`test2.wav`, 8.6s, 中文 3 句):
```
audio: internal/vad/testdata/test2.wav, duration: 8.6s
  … partial: 一二三四五六七，七六五四三二一。
[vad] speech end at 4.11s → SEGMENT_END #1
  ✓ SENTENCE [78ms]: 一二三四五六七，七六五四三二一。
[vad] speech end at 5.88s → SEGMENT_END #2
  ✓ SENTENCE [90ms]: 你好，你好。
[vad] speech end at 7.74s → SEGMENT_END #3
  ✓ SENTENCE [64ms]: 今天天气怎么样？
```

**延迟**: SEGMENT_END 发出到 sentence 返回 = **64–90ms** (RTX 5080, gpu-memory-utilization=0.6, Fun-ASR-Nano-2512 via vLLM)

---

#### FunASR 服务启动方式

```bash
# 方式 1: 用 deploy 脚本 (会自动 apply bind-loopback patch)
FUNASR_HOME=/data/liuxin/Fun-ASR \
FUNASR_GPU_MEMORY_UTILIZATION=0.6 \
FUNASR_VAD_SILENCE_SCHEDULE="inf:200" \
FUNASR_VAD_SPEECH_NOISE_THRES=0.2 \
bash deploy/funasr/run-funasr-nano.sh

# 方式 2: 直接运行 (需要先 cd 到 Fun-ASR 并激活 venv)
cd /data/liuxin/Fun-ASR
FUNASR_BIND_HOST=127.0.0.1 VLLM_USE_FLASHINFER_SAMPLER=0 \
.venv/bin/python serve_realtime_ws.py \
  --port 10095 --language 中文 \
  --model FunAudioLLM/Fun-ASR-Nano-2512 \
  --disable-spk --tensor-parallel-size 1 \
  --gpu-memory-utilization 0.6 \
  --decode-interval 0.8 \
  --vad-silence-schedule "inf:200" \
  --vad-speech-noise-thres 0.2
```

注意事项:
- 启动约需 40-60 秒（加载 vLLM + VAD + Audio encoder）
- 停止后 vLLM EngineCore 子进程可能残留（`ps aux | grep EngineCore`），需手动 `kill -9`
- GPU 显存占用 ~9.8GB (0.6 * 16GB)
- 服务日志输出到 stderr

---

#### 已知限制和后续工作

1. **FSMN-VAD 形同虚设**: 在 SEGMENT_END 模式下，服务端的 FSMN-VAD 完全不参与断句决策。它的唯一作用是在非 SEGMENT_END 的 periodic decode 中确定 partial 的起始位置。如果将来想完全去掉它可以考虑。

2. **sentences 累积返回**: 每次 `segment_end` / periodic decode 响应都包含所有历史 sentences。对于超长会话，这个列表会越来越大。可以考虑只返回新增的，或者客户端用 index 追踪。

3. **Silero VAD 参数对中文的适配**: 当前用默认 `MinSilenceDurationMs=300`。中文口语中停顿可能更短，可能需要调低到 200ms 做更激进的断句。

4. **未验证真实麦克风流**: 测试都是基于 WAV 文件模拟。真实麦克风有 jitter、丢包、噪声等问题，需要在 `/v1/chat/voice` WebSocket 端做集成测试。

Still needed:

- Validate `/v1/chat/voice` end to end with the configured Postgres and LLM services, using a real microphone WebSocket client.
- Add a local ASR health check and restart policy.
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

---

## Development Log: Doubao Streaming LLM + Bidirectional TTS (2026-07-12)

This section supersedes the older statements above that streaming TTS and the provider-backed LLM path were not implemented. The realtime output path is now implemented and validated against the real Doubao services.

### Scope Completed Today

The following path is now implemented:

```text
FunASR final sentence
  -> VoiceSession.runTextTurn
  -> ADK runner / Doubao LLM SSE stream
  -> LLM delta events
  -> TTS text chunker
  -> Doubao bidirectional TTS WebSocket
  -> asynchronous PCM chunks
  -> /v1/chat/voice binary WebSocket frames
  -> client/player
```

The LLM and TTS streams remain active at the same time. LLM output is not collected into a complete answer before synthesis starts. Text deltas are accumulated only into short TTS-friendly phrase chunks, then written while a separate goroutine continuously consumes returned PCM audio.

### Doubao Bidirectional Streaming TTS Adapter

New implementation:

- `internal/tts/types.go`
- `internal/tts/doubao.go`
- `internal/tts/doubao_test.go`
- `internal/tts/doubao_real_test.go`
- `cmd/server/tts_doubao.go`

The provider adapter implements this connection-scoped interface:

```go
type Stream interface {
    WriteText(ctx context.Context, text string) error
    Finish(ctx context.Context) error
    Cancel(ctx context.Context) error
    Audio() <-chan AudioChunk
    Errors() <-chan error
    SampleRate() int
    Close() error
}
```

Implemented Doubao protocol behavior:

- Endpoint: `wss://openspeech.bytedance.com/api/v3/tts/bidirection`
- Authentication headers:
  - `X-Api-App-Key`
  - `X-Api-Access-Key`
  - `X-Api-Resource-Id`
  - `X-Api-Connect-Id`
- Session events:
  - `StartSession` (`100`)
  - `TaskRequest` (`200`)
  - `FinishSession` (`102`)
  - `CancelSession` (`101`)
  - `SessionStarted` (`150`)
  - `SessionFinished` (`152`)
  - `SessionFailed` (`153`)
  - `TTSResponse` (`352`)
- PCM output format: mono, signed 16-bit little-endian PCM at the configured sample rate.
- One reader goroutine parses server frames and publishes `AudioChunk` values while caller goroutines continue sending text.
- WebSocket writes are serialized by a mutex.
- Session start waits for `SessionStarted` and fails on provider error, context cancellation, or timeout.
- `Finish` drains provider audio through the final event; `Cancel` supports interrupted turns.

Runtime configuration added:

```bash
TTS_PROVIDER=doubao
DOUBAO_TTS_URL=wss://openspeech.bytedance.com/api/v3/tts/bidirection
DOUBAO_TTS_APP_ID=
DOUBAO_TTS_ACCESS_TOKEN=
DOUBAO_TTS_RESOURCE_ID=seed-tts-2.0
DOUBAO_TTS_SPEAKER=saturn_zh_female_cancan_tob
DOUBAO_TTS_SAMPLE_RATE=16000
```

Credentials belong in `.env`, not `.env.example`. `.gitignore` now ignores `.env` and `.env.*` while retaining `.env.example`. Because `.env` was already tracked before this correction, it must be removed from the Git index before a future commit:

```bash
git rm --cached .env
```

Any access token previously committed or displayed outside the local environment should be rotated.

### VoiceSession LLM-to-TTS Integration

Modified:

- `internal/voice/session.go`
- `internal/voice/session_tts_test.go`

`VoiceSession` now:

1. Receives `agent.StreamEventLLMDelta` events from `Pipeline.HandleTextTurnStream`.
2. Emits `VoiceTurnOutput{Type: "llm_delta"}` for transport/UI consumers.
3. Starts TTS lazily when the first usable LLM text arrives. This prevents a slow LLM first-token response from consuming the TTS WebSocket idle timeout before any text is sent.
4. Starts a separate audio-forwarding goroutine immediately after the TTS session starts.
5. Writes aggregated text chunks to the live TTS session while PCM is read concurrently.
6. Calls `Finish` only after the LLM stream and final pending text chunk are complete.
7. Waits for the TTS final event before emitting the final sentence output.
8. Cancels and closes the active TTS stream when the voice session closes or the turn context is cancelled.
9. Falls back to synthesizing the final LLM text when a provider returns no partial delta events.

Voice output event order is now conceptually:

```text
tts_start JSON
llm_delta JSON (repeated)
tts_audio binary frame (repeated and interleaved with deltas)
tts_end JSON
sentence JSON
```

Audio data is not JSON/base64 encoded. `internal/api/server.go` has one WebSocket writer goroutine; outputs with `AudioPCM` are sent as WebSocket binary messages and control/text outputs are sent as JSON. This preserves Gorilla WebSocket's single-writer requirement.

### LLM Delta Aggregation and TTS Backpressure

New implementation:

- `internal/tts/text_chunker.go`
- `internal/tts/text_chunker_test.go`

A real test initially exposed this failure mode:

```text
711 LLM deltas
  -> 711 TaskRequest writes to TTS
  -> TTS synthesis queue remains active after LLM completion
  -> 90 second test context expires
```

Doubao often emits character-sized deltas. Sending every character as a separate TTS task is protocol-valid but creates excessive request overhead and synthesis backlog. The production path now uses `TextChunker`:

- Flush at Chinese/English punctuation or newline.
- Flush when buffered text reaches 24 Unicode runes.
- Flush remaining text when the LLM stream ends.
- Continue emitting every original `llm_delta` event to clients; only the TTS request granularity changes.

Validated reduction in one real run:

```text
255 LLM deltas
  -> 37 aggregated TTS text chunks
  -> 2,605,280 PCM bytes
  -> completed in 57.927 seconds
  -> PASS
```

This is phrase-level streaming, not whole-answer buffering. TTS can begin before the LLM finishes, while avoiding hundreds of character-level protocol requests.

### Doubao LLM Streaming Updates

Modified:

- `internal/llm/doubao.go`
- `internal/tts/llm_tts_real_test.go`

The Doubao OpenAI-compatible request now maps ADK/genai `GenerateContentConfig.MaxOutputTokens` to the provider `max_tokens` field.

A fixed `max_tokens=256` test configuration was removed after it truncated a valid response in the middle of a sentence (`"圆明园...曾被誉为‘万"`). The real integration test now uses:

- Default `DOUBAO_LLM_TTS_MAX_TOKENS=512`.
- Default total test timeout `DOUBAO_LLM_TTS_TIMEOUT_SECONDS=180`.
- A system instruction requesting natural speech-ready Chinese, no Markdown, at most roughly 300 Chinese characters, and a complete ending.

These values are test controls. Production response length should primarily be controlled by the agent instruction and product policy, with `max_tokens` retained as a high safety ceiling rather than the normal method for producing concise speech.

### Real LLM + TTS Integration Test

Test file:

- `internal/tts/llm_tts_real_test.go`

The test performs a real full-duplex provider integration:

1. Starts a streaming Doubao LLM request.
2. Logs every partial LLM delta with sequence number and elapsed time.
3. Lazily starts the Doubao TTS session on the first completed text phrase.
4. Sends aggregated phrase chunks while LLM deltas continue arriving.
5. Reads PCM concurrently from `Audio()`.
6. Logs first LLM delta latency, first PCM latency, LLM delta count, TTS text chunk count, answer, PCM byte count, and total duration.

Custom prompt command:

```bash
set -a
source .env
set +a

DOUBAO_LLM_TTS_PROMPT="请介绍一下北京的著名景点" \
DOUBAO_LLM_TTS_MAX_TOKENS=512 \
DOUBAO_LLM_TTS_TIMEOUT_SECONDS=180 \
DOUBAO_LLM_TTS_REAL_TEST=1 \
go test ./internal/tts \
  -run TestDoubaoLLMToTTSRealStream -count=1 -v
```

Without playback mode, the test writes:

```text
tmp/doubao_llm_tts_stream.wav
```

The path is explicitly resolved from the Go package test working directory (`internal/tts`) back to the repository root. This fixes the earlier accidental output under `internal/tts/tmp`.

### Direct Streaming Playback

The real integration test supports direct playback without writing a WAV file:

```bash
set -a
source .env
set +a

DOUBAO_LLM_TTS_PLAY=1 \
DOUBAO_LLM_TTS_PROMPT="请介绍一下北京的著名景点" \
DOUBAO_LLM_TTS_REAL_TEST=1 \
go test ./internal/tts \
  -run TestDoubaoLLMToTTSRealStream -count=1 -v
```

Playback behavior:

```text
Doubao PCM chunk
  -> aplay stdin
  -> ALSA playback device
```

The command used by the test is equivalent to:

```bash
aplay -q -t raw -f S16_LE -r "$DOUBAO_TTS_SAMPLE_RATE" -c 1
```

When `DOUBAO_LLM_TTS_PLAY=1`, audio is streamed to the player and WAV output is skipped. When the variable is absent, the test keeps the deterministic WAV output behavior.

Installed local playback tools detected during development:

- `/usr/bin/aplay`
- `/usr/bin/pw-play`
- `/usr/bin/ffplay`

The current test uses `aplay`.

### Standalone Real TTS Test

`internal/tts/doubao_real_test.go` validates bidirectional TTS independently from the LLM. It starts the audio consumer before sending multiple text fragments, calls `Finish`, validates non-empty PCM, and writes a valid WAV file.

Run:

```bash
set -a
source .env
set +a
DOUBAO_TTS_REAL_TEST=1 \
go test ./internal/tts -run TestDoubaoStreamRealService -count=1 -v
```

Output:

```text
tmp/doubao_tts_real_test.wav
```

Validated example:

```text
received 105862 PCM bytes
RIFF/WAVE, Microsoft PCM, 16 bit, mono, 16000 Hz
```

### Tests Added or Updated

Coverage added today:

- Mock Doubao WebSocket validates authentication headers, session/task/finish events, payload fields, PCM frames, final event, and provider error frames.
- Real Doubao bidirectional TTS test.
- Real Doubao LLM-to-TTS streaming test.
- `TextChunker` Unicode/punctuation/maximum-length behavior.
- `VoiceSession` LLM delta -> TTS text -> PCM -> final event ordering.
- Voice WebSocket integration verifies PCM is sent as a binary frame.
- Voice session cancellation closes an active TTS stream.
- Fallback synthesis supports non-streaming/final-only LLM providers.

Relevant package verification:

```bash
go test ./internal/tts ./internal/llm ./internal/voice ./internal/api ./cmd/server -count=1
```

These packages pass. `git diff --check` also passes.

The repository-wide command is now a supported regression check:

```bash
go test ./...
```

Each standalone script now lives in its own Go command directory, so `go test ./...` compiles them independently. Run a tool with `go run ./scripts/<tool> [wav-path]`.

### FunASR Environment and Segment-End Status

The current workstation uses:

```text
/home/liuxin/fun-asr
```

The private Fun-ASR-Nano service was prepared in this environment and tested at:

```text
ws://127.0.0.1:10095
```

The deployed source includes the `SEGMENT_END` behavior documented earlier: local Silero VAD can force sentence decoding without closing the FunASR WebSocket. The Go end-to-end script uses 200 ms audio chunks and reports partial ASR results while audio is still uploading. A representative three-sentence run detected three VAD segment ends and returned three final sentences.

Note that older sections in this document reference `/data/liuxin/Fun-ASR`; that was the previous environment. The active source location for this workstation is `/home/liuxin/fun-asr`.

### Current Known Limitations

1. Real microphone -> FunASR -> LLM -> TTS -> client speaker has not yet been exercised as one continuous `/v1/chat/voice` session. Provider-level and transport-level components are tested separately.
2. Barge-in is not complete at the product level. `VoiceSession` can cancel an active TTS stream, but new user speech is not yet wired to trigger cancellation and flush already-buffered client audio.
3. The server sends raw PCM binary frames. Browser clients need an AudioWorklet or equivalent PCM playback buffer; native clients need ALSA/PulseAudio/PipeWire playback integration.
4. Backpressure policy is currently a fixed 24-rune/punctuation chunker. It should be tuned with real conversational prompts, especially for the tradeoff between first-audio latency and natural prosody.
5. TTS input cleanup relies on the LLM system instruction to avoid Markdown. A deterministic speech-text sanitizer should still remove Markdown, URLs, code blocks, and unsupported symbols before TTS.
6. Long answers produce long audio and can delay turn completion. Product-level response length should remain concise for voice interactions.
7. `.env` was previously tracked by Git. Ignoring it does not automatically remove it from the index or history.

### Next Recommended Step (Updated)

The next implementation milestone is a real microphone end-to-end client for `/v1/chat/voice`:

1. Capture mono 16 kHz PCM in 200 ms frames.
2. Send frames to `/v1/chat/voice` over one WebSocket.
3. Display FunASR partial/final transcripts and LLM deltas.
4. Feed server binary PCM frames directly into a jitter-aware playback queue.
5. Measure speech-end -> ASR final, ASR final -> first LLM delta, first delta -> first PCM, and speech-end -> audible response.
6. On new speech/VAD start, cancel the active TTS turn, clear queued playback PCM, and continue the existing microphone session.
7. Add a deterministic TTS text sanitizer before production tuning.

After this E2E client is stable, implement barge-in/interruption tests and then validate PostgreSQL session storage plus mem0 behavior under multi-turn voice conversations.

---

## Development Log: 全链路日志 + Python 客户端 + In-Memory Session Fallback (2026-07-13)

### 本轮改动概要

1. **VAD 切换到 Silero** — `.env` 中 `VAD_PROVIDER=passthrough` → `VAD_PROVIDER=silero`
2. **全链路结构化日志** — 在 voice pipeline 各层加入 `slog` 日志
3. **Python 实时语音客户端** — `client/voice_client.py`
4. **In-Memory Session Fallback** — 无 PostgreSQL 时自动用内存 session store 启动

---

### 1. VAD 切换

改动文件: `.env`

```diff
-VAD_PROVIDER=passthrough
+VAD_PROVIDER=silero
```

`passthrough` 模式下 Silero VAD 不参与，所有音频原封发到 FunASR，`VADEnd` 永远不触发，`SEGMENT_END` 因此也不会发出。切换到 `silero` 后，本地 Silero VAD 检测 speech end → 触发 `NotifySegmentEnd` → 服务端 force decode。

---

### 2. 全链路结构化日志

#### `internal/voice/session.go`

已有 `pipeline.logger`，新增：
- `logger *slog.Logger` 字段，挂在 VoiceSession 上
- 从 `pipeline.logger.With("user_id", ..., "session_id", ...)` 派生
- 日志点:
  - session 启动（mode = server_endpointing / local_vad）
  - ASR sentence 收到（text）
  - text turn 失败（error + text）
  - VAD end 检测到，发送 SEGMENT_END
  - SEGMENT_END 发送失败
  - TTS 创建成功（sample_rate）/ 失败（error）

#### `internal/voice/funasr_asr.go`

新增 `logger *slog.Logger` 字段，`NewFunASRASR` 签名变为:

```go
func NewFunASRASR(ctx context.Context, cfg FunASRASRConfig, logger *slog.Logger) (*FunASRASR, error)
```

日志点:
- 连接 FunASR（connecting）
- 连接失败（dial failed + error）
- session started
- 发送 SEGMENT_END
- 收到 sentence（text + start_ms + end_ms）
- 读循环错误（read error）

#### `internal/api/server.go`

日志点:
- voice WebSocket 连接建立（user_id, session_id, sample_rate, remote_addr）
- voice WebSocket 关闭（user_id + error reason）

#### `cmd/server/asr_doubao.go`

`newASRStream` 签名变为:

```go
func newASRStream(ctx context.Context, cfg config.ASRConfig, logger *slog.Logger) (voice.StreamingASR, error)
```

logger 从 `cmd/server/main.go` 传入。

---

### 3. Python 实时语音客户端

新增文件:
- `client/voice_client.py`
- `client/requirements.txt`

功能:
- PyAudio 录音（16kHz, 16bit, mono, 200ms chunk）
- websockets 异步连接 `ws://127.0.0.1:8080/v1/chat/voice`
- 接收并打印: ASR partial（\r 覆盖）、ASR sentence、LLM delta（inline）
- 接收 TTS PCM binary 并通过 PyAudio 播放
- Ctrl+C 发 `{"type":"final"}` 优雅退出

运行方式:

```bash
cd client
pip install -r requirements.txt
python voice_client.py
```

---

### 4. In-Memory Session Store Fallback

改动文件: `cmd/server/main.go`

当 `ADK_SESSION_POSTGRES_DSN` 为空时，不再报错退出，而是使用 `sessionstore.NewInMemoryManagedService` 作为 session backend:

```go
if cfg.Session.PostgresDSN != "" {
    sessionService = sessionstore.NewPostgresService(...)
} else {
    logger.Warn("ADK_SESSION_POSTGRES_DSN not set, using in-memory session store")
    sessionService = sessionstore.NewInMemoryManagedService(sessionstore.Config{})
}
```

这允许在没有 PostgreSQL 的开发环境中直接启动服务进行实时语音调试。会话数据不持久化，进程重启后丢失。

---

### 5. 编译验证

```bash
go build ./cmd/... ./internal/...   # 通过
go test ./internal/voice/ -run TestFunASR -v  # PASS
curl http://127.0.0.1:8080/healthz  # {"status":"ok"}
```

服务已在本地 `:8080` 运行。

---

### 当前状态

| 组件 | 状态 |
|------|------|
| Silero VAD | ✅ 已启用 |
| FunASR ASR | ✅ 连接中 (ws://127.0.0.1:10095) |
| SEGMENT_END 协议 | ✅ 已验证 (64-90ms) |
| Doubao LLM 流式 | ✅ SSE streaming |
| Doubao TTS 流式 | ✅ bidirectional WebSocket |
| 全链路日志 | ✅ 本轮新增 |
| Python 客户端 | ✅ 本轮新增 |
| In-Memory Session | ✅ 本轮新增 (fallback) |
| 记忆查询接入 LLM | ❌ mem0 查询已实现但结果未注入 prompt |
| Barge-in/打断 | ❌ 未实现 |
| PostgreSQL Session | ⚠️ 可选，需配置 DSN |

---

## Development Log: 全链路 E2E 调试 + Off-by-one Session Bug 修复 (2026-07-13 续)

### 本轮改动概要

通过 Python 客户端真实麦克风 E2E 测试，逐步发现并修复了 5 个 bug，最终实现完整的 VAD→ASR→LLM→TTS 实时语音对话。

---

### 1. FunASR readLoop 死亡修复

**问题**: 麦克风说话后服务端无响应。

**根因**: `parseFunASRResponse` 对 FunASR 的周期性 decode 响应（空 sentences + 空 partial）判定为错误，导致 readLoop goroutine 退出。后续 SEGMENT_END 响应中的实际 sentence 无人读取。

**修复** (`internal/voice/funasr_asr.go`): 移除了过于严格的验证检查。空响应是 FunASR 定时 decode 的正常行为，不应视为错误。

---

### 2. TTS 重复循环修复 (turnActive 机制)

**问题**: LLM 回答后 TTS 重复播放，ASR 产生幻觉文本。

**根因**: LLM+TTS 处理期间，麦克风音频继续流入 FunASR → FunASR 的 `_force_speech_on_next_audio` 机制将噪声标记为语音 → Silero VAD 检测到 VADEnd → 触发 SEGMENT_END → FunASR 从噪声中幻觉出文本 → 触发新一轮 LLM+TTS → 无限循环。

**修复**:
- 服务端 (`internal/voice/session.go`): 新增 `turnActive atomic.Bool` 字段。`ProcessAudio` 在 `turnActive=true` 时跳过 SEGMENT_END 发送。
- 客户端 (`client/voice_client.py`): `player.playing` flag 控制 TTS 播放期间不发送麦克风数据。

```go
// session.go - consumeServerEndpointingTranscripts
s.turnActive.Store(true)
_, err := s.runTextTurn(s.ctx, transcript)
s.turnActive.Store(false)

// session.go - ProcessAudio (endpointing 模式)
if s.turnActive.Load() {
    return VoiceTurnOutput{Final: false}, nil
}
```

---

### 3. LLM 中文指令

**问题**: LLM 用英文回复。

**修复** (`internal/config/config.go`): 默认 instruction 改为中文:

```go
Instruction: envString("AGENT_INSTRUCTION", "你是一个实时语音助手。请用自然、简洁的中文回答，适合语音朗读，不要使用 Markdown 格式。"),
```

---

### 4. ASR final 事件时序修复

**问题**: 客户端日志中 `[ASR final]` 出现在 `[TTS end]` 之后，影响 UI 展示。

**根因**: `consumeServerEndpointingTranscripts` 在 `runTextTurn` 完成后才发送 `sentence` 输出。

**修复** (`internal/voice/session.go`): 在调用 `runTextTurn` 之前就发送 `sentence` 输出:

```go
s.outputs <- VoiceTurnOutput{Type: "sentence", Transcript: transcript}
s.turnActive.Store(true)
_, err := s.runTextTurn(s.ctx, transcript)
s.turnActive.Store(false)
```

---

### 5. LLM Off-by-one 响应修复 (核心 bug)

**问题**: LLM 始终回答上一个问题而不是当前问题。Q1 正确，Q2 回答 Q1 的内容，Q3 回答 Q2 的内容，以此类推。

**根因**: `internal/sessionstore/managed.go` 的 `AppendEvent` 方法。

ADK runner 流程:
1. `ManagedService.Get()` 返回 session 副本 A → 传给 InvocationContext
2. `ManagedService.AppendEvent(sess=A, userEvent)` 内部重新调用 `s.inner.Get()` 获取**新副本 B** → event 追加到 B
3. LLM 读取 `ictx.Session().Events()` (仍是副本 A) → **看不到当前 user message**
4. LLM 只能看到上一轮及之前的历史 → 回答上一个问题

**修复** (`internal/sessionstore/managed.go`): 直接将 event 追加到调用者传入的 session 对象，不再重新获取副本:

```go
func (s *ManagedService) AppendEvent(ctx context.Context, sess adksession.Session, event *adksession.Event) error {
    if sess == nil {
        return fmt.Errorf("sessionstore: session is nil")
    }
    if event == nil {
        return fmt.Errorf("sessionstore: event is nil")
    }
    // 解包 sessionView，确保内层 service 收到可修改的具体 *session
    target := sess
    if sv, ok := sess.(sessionView); ok {
        target = sv.inner
    }
    if err := s.inner.AppendEvent(ctx, target, event); err != nil {
        return err
    }
    s.record(target)
    return nil
}
```

这确保 `inMemoryService.AppendEvent` 的 `sess.appendEvent(event)` 修改的是 runner 持有的同一个 session 对象，LLM 构建 prompt 时能看到包含当前 user message 的完整历史。

**测试**: `go test ./internal/sessionstore/... -v` 全部通过。

---

### 6. 客户端改动

改动文件: `client/voice_client.py`

- `SERVER_URL` → `ws://192.168.2.99:8080/v1/chat/voice` (支持远程客户端)
- 新增 `SESSION_ID = "test-session-1"` 并拼入 URL (多轮对话)
- `TTSPlayer.playing` flag + `send_mic_audio` 跳过播放期间的音频发送

---

### 当前状态 (E2E 验证通过)

| 组件 | 状态 |
|------|------|
| Silero VAD → SEGMENT_END | ✅ 正常断句 |
| FunASR readLoop | ✅ 修复后稳定 |
| LLM 多轮上下文 | ✅ off-by-one 修复后正确 |
| turnActive 防幻觉 | ✅ LLM+TTS 期间不触发 SEGMENT_END |
| 客户端静音 | ✅ TTS 播放期间不发音频 |
| 中文对话 | ✅ 正常回答 |
| 会话记忆 (in-memory) | ✅ 同 session_id 多轮对话正确 |

---

### 已知限制

1. **内存 session 无持久化** — 服务重启后丢失所有对话历史。需配置 `ADK_SESSION_POSTGRES_DSN` 启用持久化。
2. **session 无上限** — `Config{}` 未设 `MaxTurns`/`MaxEvents`，长对话可能导致 LLM token 超限。
3. **Barge-in 未实现** — 用户开口说话时不会中断正在播放的 TTS。
4. **mem0 记忆未注入** — 长期记忆查询已实现但结果未合并到 LLM prompt。

### 下一步

1. 给 `Config` 设 `MaxTurns` 限制上下文窗口
2. 实现 barge-in (新语音取消当前 TTS turn)
3. 接入 mem0 记忆注入到 LLM prompt
4. 配置 PostgreSQL 做 session 持久化

---

## Development Log: mem0 读取/异步写入与本地数据库计划 (2026-07-13)

> 本节覆盖上文关于“mem0 未注入 prompt”的旧状态。

### 已完成的工程接入

- `cmd/server/llm_gemini.go`：在 ADK `BeforeModelCallback` 中读取最后一条用户文本；文本少于 4 个字符时跳过。检索以 `user_id + query` 调用 mem0，并将返回记忆作为 `[记忆上下文]` 插入本轮 LLM 请求。
- `internal/voice/pipeline.go`：LLM 成功生成回复后，将 user/assistant 消息提交给 `memory.Writer`，不阻塞语音与 TTS 响应。
- `internal/memory/writer.go`：同一用户的写入串行化，跨用户最多 5 路并发，默认队列深度 64；队列满时丢弃最旧请求。
- `internal/memory/writer_test.go`：覆盖同用户写入顺序、队列满时淘汰最旧请求、全局并发限制；`go test -race ./internal/memory ./internal/voice ./internal/api` 通过。

### 当前检索和写入策略

- 检索不是全量历史注入：调用 mem0 `/search`，按当前 `user_id` 过滤，使用当前问题做语义检索，返回 `MEM0_TOP_K` 条（默认 8 条）。
- 当前 Go 服务不做二次排序、最低分阈值或 token 预算；mem0 的排序策略由其向量库/配置决定。
- 写入调用 mem0 `/memories`；是否使用 LLM 提炼、去重由 mem0 服务端的 `infer` 默认行为与其 LLM 配置决定。
- 成功检索会记录 `memory injected`（用户 ID 与条数）；检索失败和写入失败会记录 warning。当前尚未记录写入成功、记忆 ID 或相似度分数，且不应默认记录记忆正文以避免泄露用户隐私。

### 当前运行状态

应用配置为：

```text
MEM0_MODE=self_hosted
MEM0_BASE_URL=http://localhost:8888
```

截至本次记录，本机 `localhost:8888` 尚无 mem0 服务监听。因此应用可正常对话，但 mem0 检索会降级为空，异步写入会失败且不会持久化。应用侧“入队”不等于 mem0 已成功落库。

本机 mem0 源码位于 `/data/liuxin/mem0`。其 `server/docker-compose.yaml` 的 API 映射正是 `localhost:8888`，可与现有 Go 配置直接对接。

### FastEmbed 本地 embedding 方案

已决定避免豆包 embedding 计费，改用 `FastEmbed` 在 mem0 API 进程内运行本地 ONNX embedding：

- 不需要 embedding HTTP 服务，也不调用云端 embedding API。
- mem0 核心库已提供 `FastEmbedEmbedding`，但当前 `server/main.py` 的 API provider 白名单仅包含 `openai`、`gemini`，容器依赖也未安装 `fastembed` extra。
- 启动前需要修改 mem0 服务层：安装 `mem0ai[fastembed]`、将 `fastembed` 加入 `BUNDLED_EMBEDDER_PROVIDERS`、通过环境变量将默认 embedder 配置为 `fastembed` 和选定的多语言模型。
- 记忆提炼仍需要一个 LLM。可继续复用现有豆包 OpenAI 兼容接口；只有新增/更新记忆时会调用它，不用于 embedding。

### PostgreSQL / pgvector（原生部署）

mem0 的 `server/` API 不提供 SQLite 回退，需使用启用 `pgvector` 的 PostgreSQL。当前工作站为 Ubuntu 24.04，尚未安装 PostgreSQL。推荐原生安装 PostgreSQL 16 与 pgvector：

```bash
sudo apt update
sudo apt install -y postgresql-16 postgresql-client-16 postgresql-server-dev-16 build-essential git
git clone --branch v0.8.0 https://github.com/pgvector/pgvector.git /tmp/pgvector
cd /tmp/pgvector && make && sudo make install
sudo systemctl enable --now postgresql
```

初始化 mem0 数据库：

```sql
CREATE USER mem0 WITH PASSWORD '<strong-password>';
CREATE DATABASE mem0 OWNER mem0;
\c mem0
CREATE EXTENSION vector;
```

验证扩展：

```bash
psql "postgresql://mem0:<strong-password>@127.0.0.1:5432/mem0" \
  -c "SELECT extname FROM pg_extension WHERE extname = 'vector';"
```

### 后续步骤

1. ~~安装 PostgreSQL 16 与 pgvector，创建 `mem0` 数据库和受限用户。~~ ✅
2. ~~修改 `/data/liuxin/mem0/server` 以启用 `FastEmbed`，并配置本地多语言 embedding 模型。~~ ✅
3. ~~配置 mem0 的记忆提炼 LLM、认证和 PostgreSQL 连接；本地开发可用 `AUTH_DISABLED=true`，生产环境必须启用 API key/JWT。~~ ✅
4. ~~启动 mem0 API 到 `localhost:8888`，验证 `/docs`、`/search`、`/memories`。~~ ✅
5. 用真实多轮语音验证：写入成功、随后查询命中、`[记忆上下文]` 注入 LLM；补充 mem0 HTTP 集成测试和安全的结构化召回日志。

---

## Development Log: mem0 本地服务部署完成 (2026-07-14)

### 概要

mem0 服务已在本机 `localhost:8888` 成功运行，POST `/memories` 写入和 POST `/search` 检索均验证通过。

### 基础设施

| 组件 | 配置 |
|------|------|
| PostgreSQL | 16, 本机 `127.0.0.1:5432` |
| 数据库 | `mem0` (向量存储) + `mem0_app` (应用表/alembic) |
| 用户 | `mem0` / `mem0pass` |
| pgvector 扩展 | 已安装（从 `/data/liuxin/pgvector/` 编译） |
| Python 环境 | `/data/liuxin/mem0/.venv` (venv) |
| LLM | 豆包 `doubao-seed-2-0-mini-260428` via OpenAI-compatible API |
| Embedder | FastEmbed `thenlper/gte-large` (1024 dims, 本地 ONNX) |
| 模型缓存 | `/data/liuxin/fastembed_models` (通过 `HF_ENDPOINT=https://hf-mirror.com` 下载) |

### mem0 源码改动 (`/data/liuxin/mem0`)

#### 1. `server/main.py` — 服务配置

- `BUNDLED_EMBEDDER_PROVIDERS` 增加 `"fastembed"`
- `DEFAULT_CONFIG` 改为动态构建：支持 `OPENAI_BASE_URL` 和 fastembed provider
- `vector_store.config` 增加 `"embedding_model_dims": 1024`（默认 1536 是 OpenAI embedding 维度，gte-large 输出 1024 维）

#### 2. `mem0/vector_stores/pgvector.py` — psycopg3 向量类型适配

**问题**: FastEmbed 返回 numpy ndarray，psycopg3 不知道如何将 ndarray 序列化为 pgvector 的 `vector` 类型，报错：
```
psycopg.ProgrammingError: cannot adapt type 'ndarray' using placeholder '%s' (format: AUTO)
```

**修复**: 在 `_get_cursor` 的 psycopg3 分支中注册 pgvector 适配器：

```python
if PSYCOPG_VERSION == 3:
    from pgvector.psycopg import register_vector
    with self.connection_pool.connection() as conn:
        register_vector(conn)
        with conn.cursor() as cur:
            ...
```

需要额外安装 `pgvector` Python 包：`pip install pgvector`

#### 3. `server/.env` — 环境变量配置文件（新建）

包含 LLM、Embedder、Postgres、Auth、History、Telemetry 全部配置。`AUTH_DISABLED=true` 用于本地开发。

### 启动命令

```bash
cd /data/liuxin/mem0/server
source /data/liuxin/mem0/.venv/bin/activate
set -a && source .env && set +a
export HF_ENDPOINT=https://hf-mirror.com
uvicorn main:app --host 0.0.0.0 --port 8888
```

### 验证结果

```bash
# 写入
curl -X POST http://127.0.0.1:8888/memories \
  -H "Content-Type: application/json" \
  -d '{"messages": [{"role": "user", "content": "我叫小鱼，我最近工作压力很大"}], "user_id": "test-user-1"}'
# → {"results":[{"id":"...","memory":"User's name is Xiaoyu...","event":"ADD"}]}

# 检索
curl -X POST http://127.0.0.1:8888/search \
  -H "Content-Type: application/json" \
  -d '{"query": "用户叫什么名字", "user_id": "test-user-1"}'
# → {"results":[{"memory":"User's name is Xiaoyu...","score":0.83...}]}
```

### 注意事项

- mem0 源码改动未提交到 mem0 仓库，仅本地生效。`pip install --upgrade mem0ai` 会覆盖 `pgvector.py` 的修复。
- pgvector 适配器 bug 是 mem0 在 psycopg3 + fastembed 组合下的已知缺陷，未来版本可能修复。
- `embedding_model_dims` 必须与所选 embedding 模型输出维度一致，否则 INSERT 会报 `expected N dimensions, not M`。
- alembic 迁移需要先 source `.env` 再执行：`cd /data/liuxin/mem0/server && alembic upgrade head`

### 当前状态

| 组件 | 状态 |
|------|------|
| mem0 服务 | ✅ 运行中 (localhost:8888) |
| 记忆写入 | ✅ POST /memories 成功 |
| 记忆检索 | ✅ POST /search 成功 (score 0.83) |
| Go 服务 mem0 集成 | ✅ BeforeModelCallback + MemoryWriter 已提交 |
| E2E 语音+记忆 | ✅ 已验证（见下方） |

### E2E 记忆验证结果

验证流程（排除 session 记忆干扰）：

1. 用 Python 客户端 (user_id=test-user, session-A) 说"叫我 Trump" → mem0 写入成功
2. 重启 Go 服务（清空 in-memory session store）
3. 用新 session 问"你还记得我叫什么吗" → LLM 回答包含用户名

确认 BeforeModelCallback 从 mem0 检索记忆并注入 LLM prompt 生效。

### 记忆架构决策

**多语言策略**: mem0 默认用英文存储提炼后的记忆（如 "User's name is Xiaoyu"）。对于中/日/英多语言用户场景，英文作为记忆的"中间语言"是合理方案：
- `thenlper/gte-large` 是多语言 embedding 模型，跨语言检索能命中（中文 query 匹配英文记忆 score 0.83）
- 不需要按语种维护多套 custom_instructions
- LLM 根据当前对话语言自适应回复

**Session Store vs mem0 分工**:
- Session Store (in-memory): 当前对话上下文，支持指代、追问、话题延续
- mem0: 跨会话长期记忆，存提炼后的事实/偏好
- 两者不可互相替代——去掉 session store 会丢失当前对话连贯性

**Session Store 待优化**:
- 当前 `sessionstore.Config{}` 为空，无 TTL/MaxTurns 限制，session 永不释放
- 语音场景建议：WebSocket 关闭时直接删除对应 session，不依赖 TTL 定时清理
- 可选加 `MaxTurns: 20` 防止单次超长对话撑爆 LLM token 窗口

---

## Development Log: WebSocket Write Pump + TTS 播放修复 (2026-07-23)

### 问题与修复

#### 1. WebSocket Ping Timeout 断连（长 TTS 场景）

**问题**: 背诵长文本（如出师表）时，TTS 音频播放到一半 WebSocket 连接断开，客户端报 `keepalive ping timeout`。

**根因分析**:
1. TTS 产出音频速度 > 客户端实时播放速度（client `stream.write()` blocking）
2. 客户端读 WebSocket 被播放速率限制 → TCP 背压 → 服务端 `WriteMessage` 阻塞
3. gorilla/websocket 的默认 ping handler 在 `ReadMessage` 内部触发，通过 `WriteControl` 回 pong
4. `WriteControl` 需要获取写锁（`c.mu`），但写锁被 data frame 持续占用
5. pong 回复超过 1s deadline → 客户端 ping timeout → 断连

**修复** (`internal/api/server.go`):
- 引入 **write pump** 模式：所有 WebSocket 写操作通过一个 goroutine 序列化
- 分离 `pongCh`（容量 4，高优先级）和 `writeCh`（容量 256，数据帧）
- write pump 每次循环优先检查 pongCh，确保 pong 不被大量 audio frame 阻塞
- 自定义 `SetPingHandler`，将 pong 发送到 pongCh 而非走默认的 `WriteControl`

```go
// Write pump with pong priority
go func() {
    for {
        select {
        case msg := <-pongCh:  // priority
            conn.WriteMessage(msg.msgType, msg.data)
        default:
        }
        select {
        case msg := <-pongCh:
            conn.WriteMessage(msg.msgType, msg.data)
        case msg, ok := <-writeCh:
            if !ok { return }
            conn.WriteMessage(msg.msgType, msg.data)
        }
    }
}()
```

这是 gorilla/websocket 官方推荐的 write pump 模式——`Conn` 写操作不是并发安全的，所有写入必须由同一个 goroutine 执行。

#### 2. 客户端 TTS 播放阻塞 WebSocket 读取

**问题**: 客户端 `player.feed(pcm)` 调用 pyaudio `stream.write()` 是阻塞的（按实时播放速率），导致 `recv_messages` 中 `async for message in ws` 读取速度被限制。

**修复** (`client/voice_client.py`):
- `TTSPlayer` 改为独立线程播放，`feed()` 非阻塞入队
- 分离 `flush()`（等播完）和 `stop()`（立即打断/barge-in）
- `flush()` 在 `tts_end` 时通过 `run_in_executor` 调用，不阻塞 asyncio 事件循环

```python
class TTSPlayer:
    def feed(self, pcm):    # 非阻塞：入队
    def flush(self):        # 阻塞：等队列播完 + 写静音 flush 硬件缓冲区
    def stop(self):         # 立即中断（barge-in）
```

#### 3. TTS 播放尾部截断

**问题**: pyaudio `stream.write()` 返回只代表数据进入了硬件缓冲区，`stop_stream()` 会截断未播完的音频。

**修复**: 播放线程 drain 完队列后写 300ms 静音，确保硬件缓冲区中的真实音频播完后再关流。

#### 4. TTS 播放期间堆积用户音频

**问题**: `player.flush()` 同步阻塞 asyncio 事件循环 → `send_mic_audio` 任务暂停 → PyAudio 输入缓冲区堆积 → flush 结束后堆积音频被发送 → 服务端回答 TTS 期间的提问。

**修复**: `tts_end` 处理改为 `await asyncio.get_event_loop().run_in_executor(None, player.flush)`，事件循环不被阻塞，`send_mic_audio` 持续读取并丢弃麦克风数据（`player.playing=True` 期间不发送）。

### 其他改动

- **LLM 请求上下文日志**: `cmd/server/llm_gemini.go` 中 BeforeModelCallback 异步打印完整 LLM 请求内容（role + text），用于诊断 TTFT 延迟
- **客户端 ping 参数**: `ping_interval=30, ping_timeout=60`（加大容忍度作为额外保险）
- **mem0 custom_instructions**: 配置只提取用户事实，不存助手回复，按用户语言存储

### 当前已知问题

| 问题 | 状态 | 备注 |
|------|------|------|
| LLM 首 token ~10s | ✅ 已解决 (2026-07-31) | 思考模型的 reasoning 阶段，见下节 |
| mem0 写入超时 | ✅ 已解决 (2026-07-31) | search/add 共用 10s client 超时，见下节 |
| Barge-in | ❌ 未实现 | TTS 期间客户端不发音频，无法打断 |
| Echo/回声 | ⚠️ 已知 | 需要 AEC，开发阶段用耳机 |

---

## Development Log: 首 token 延迟 / session 窗口回归 / 记忆质量 (2026-07-31)

本轮共修复 4 个问题。第 2 个是本轮自己引入的回归，第 3、4 个是记忆质量的两次反向调整，记录在此以免重犯。

### 1. LLM 首 token 延迟 7–12s — 思考模型的 reasoning 阶段

**现象**: 第二轮对话开始首 token 明显变慢，复杂问题更严重。

**排查过程**（先排除了两个常见嫌疑）:

| 测试 | 结果 |
|------|------|
| mem0 search 基线 | 90ms |
| mem0 写入在飞时的 search / LLM TTFT | 无影响（100ms / 400ms），**排除资源竞争** |
| doubao TTFT vs 上下文长度（0→20 轮，181→3243 字符） | 全部 0.4s，**排除上下文长度** |

**根因**: `doubao-seed-2-0-mini-260428` 是**思考模型**。首包 SSE 里 `content` 为空，装的是 `reasoning_content`：

```json
{"delta":{"content":"","reasoning_content":"用户","role":"assistant"}}
```

适配器只取 `content`，所以要等 reasoning 全部吐完才有第一个可送 TTS 的字。TTFT ≈ 思考字数 ÷ 100 字/秒：

| 提问 | 首包 SSE | 首个 content | 思考字数 |
|---|---|---|---|
| 你好 | 0.38s | 1.29s | 94 |
| 你还记得我吗？ | 0.58s | 2.57s | 153 |
| 跟我聊聊天儿 | 0.34s | 2.85s | 202 |
| 背诵完整的出师表 | 0.41s | **12.24s** | 1260 |

这解释了"第二轮开始变慢"：第一轮多是短招呼（思考 94 字），第二轮起是开放式提问（200+ 字）。

**修复**: 加 `thinking` 请求参数，默认 `disabled`

- `internal/llm/doubao.go` — `doubaoChatRequest` 加 `Thinking *doubaoThinking`，`DoubaoConfig` 加 `Thinking string`
- `internal/config/config.go` — `DOUBAO_LLM_THINKING`，默认 `disabled`
- `cmd/server/llm_gemini.go` — 透传
- `.env` / `.env.example` — 显式声明

| 提问 | 开思考 | 关思考 |
|---|---|---|
| 你还记得我吗？ | 3.47s | **0.48s** |
| 跟我聊聊天儿 | 3.05s | **0.42s** |
| 背诵完整的出师表 | 12.72s | **0.67s** |

实测服务端 `agent turn` → `tts started` 从 7s 降到 **0.57s**。

> 若将来换回思考模型或需要 reasoning，应把 `reasoning_content` 也作为独立事件类型透出（用于 UI 展示"正在思考"），而不是让它阻塞 TTS。

### 2. 回归: MaxTurns 窗口导致当前发言不可见（本轮引入）

**现象**: 第二轮起 LLM 用英文回复，且答非所问（"It seems there is no new request here"），无视"说中文"。

**排查**: 一开始查不下去，因为存在**日志盲区**——请求日志被放在 `len(memories)==0` 的提前返回之后，记忆为空的轮次完全没有记录。补上日志后立刻定位（见改动 5）：

```
[0] user: 你好，你好。
[1] model: 你好呀！有什么我能帮上忙的吗？
[2] user: Continue processing previous requests as instructed. Exit or provide a summary if no more outputs are needed.
```

用户当前发言"你还记得我是谁吗？"根本没进 `req.Contents`，要到下一轮才从 session 历史冒出来。ADK 检测到历史以 model 轮结尾（Gemini 要求 role 交替），补了一句合成 user 消息占位（`internal/llminternal/contents_processor.go:68`）。模型看到最后一句是英文指令，就照着回英文。

**根因**: `internal/sessionstore/managed.go` 的 `sessionView` 存的是 `Get()` 时刻的事件**快照**。ADK runner 的顺序是：

```
Get() → AppendEvent(用户发言) → 读 Session().Events() 构造请求
```

AppendEvent 落到 inner session，但 view 的快照不更新，当前发言因此不可见。

这是 `MaxTurns: 3` 触发的——`MaxTurns=0` 时 `windowSession` 直接返回原 session、不套 view，所以之前一直没暴露。**与 2026-07-13 修过的 off-by-one 同源，是它的另一个入口。**

**修复**: `sessionView` 不再存快照，改存 `maxTurns`，`Events()` 每次对 inner 的实时事件做惰性窗口：

```go
func (s sessionView) Events() adksession.Events {
	return eventList(keepLastTurns(collectEvents(s.inner.Events()), s.maxTurns))
}
```

回归测试 `TestManagedServiceWindowedSessionSeesLaterAppends` 覆盖"Get() 之后 append 必须可见"，并验证过改回快照实现即 FAIL。

**副作用**: 修复前 `lastUserText()` 取到的是那句英文样板，拿它去 mem0 检索全部落空——这也是当时记忆一直搜不到的原因之一。

### 3. 记忆注入位置: contents → system instruction

**现象**: 模型主动执行记忆里的内容，反复背《出师表》。

**根因**: 记忆以 `user` role 插在 contents 最前面，模型天然当成"用户说过的话"。加一句"这不是当前请求"的免责声明压不住。

**修复** (`cmd/server/llm_gemini.go`): 改为拼进 `req.Config.SystemInstruction`，即指令层，与业界（ChatGPT memory）一致。构造新的 `Content` 对象而非 append 到原 Parts，避免污染 agent 配置里跨轮共享的 SystemInstruction。

> 注意 ADK 会在 system instruction **最前面**插入英文身份样板（`identity_request_processor.go:39`）：`You are an agent. Your internal name is "voice-agent"...`。单独测试它不足以导致英文回复，但 `llmagent.Config.Description` 建议改成中文以减少语言干扰。

### 4. mem0 提取规则的两次调整

**第一次（过滤噪声）**: 库里存了大量一次性指令当长期记忆——`用户请求背诵《出师表》`、`用户让助手停止说话`、`用户请求助理跳舞`、`用户说了"你好，你好"`。加规则排除一次性请求和寒暄。

**第二次（过滤过头，反向修正）**: 数学问题完全没被记住。先排除写入失败——3 次超时发生在 18:35:01/18:35:20/18:40:56，都不在数学轮次（18:38–18:39），说明请求送达了，是 mem0 主动不存。原因是第一次的规则命中了"用户请求解释费马定理"。

这里把两件事混为一谈了：

- **请求本身**（"用户要求背出师表"）—— 一次性，不该存 ✓
- **请求暴露的兴趣**（"用户对数学感兴趣"）—— 持久事实，该存 ✗ 被一起丢了

**最终规则** (`/data/liuxin/mem0/server/main.py`): 不再笼统禁止请求类记忆，改为明确区分——不要把请求记成待办任务，但要提取它暴露的持久兴趣。带上正反例子：从"解释费马定理"应存"用户对数学感兴趣"而非"用户要求解释费马定理"；若请求不暴露任何持久信息（"唱首歌"、"别说了"）则不存。

> 教训：过滤规则要针对"记成什么形式"，而不是"是否是请求"。请求是信息的载体，不是信息本身。

### 5. mem0 写入超时（记忆静默丢失）

**现象**: `memory write failed: context deadline exceeded`，对应轮次的记忆完全丢失。

**根因**: `MEM0_TIMEOUT_SECONDS=10` 是 `http.Client` 级别的超时，search 和 add 共用。但两者预算需求相反：

- search 在关键路径上，必须快（90ms，超时应立即降级）
- add 是后台异步的，mem0 内部要多轮 LLM 提炼 + 与已有记忆判重，**耗时随库大小增长**（1 条时 4.5s，20 条时 >30s）

**修复**: 移除 client 级超时，改由各自的 per-request context 控制

| 操作 | 超时 | 环境变量 |
|---|---|---|
| Search | 10s | `MEM0_TIMEOUT_SECONDS` |
| Add | 60s | `MEM0_WRITE_TIMEOUT_SECONDS`（新增） |

`Mem0Client.withTimeout` 只在调用方没有更紧的 deadline 时才施加，不覆盖上游取消。`writer.go` 自己那层 30s 也放宽到 2 分钟，否则会盖住新的 60s。

### 6. 请求日志覆盖所有返回路径

排查问题 2 时发现的日志盲区。`cmd/server/llm_gemini.go` 的请求日志原本在 `len(memories)==0` 提前返回之后，记忆为空时完全无记录。

改为 `defer logLLMRequest(...)`，所有返回路径都记录；新增 `llm system instruction` 日志打印 ADK 拼装后的完整系统提示（此前唯一看不到的部分）；快照文本后再异步输出，避免读到已被模型消费的请求。

### 本轮改动文件

| 文件 | 改动 |
|------|------|
| `internal/llm/doubao.go` | `thinking` 请求参数 + `DoubaoConfig.Thinking` |
| `internal/config/config.go` | `DOUBAO_LLM_THINKING`、`Mem0Config.WriteTimeout` |
| `internal/sessionstore/managed.go` | `sessionView` 快照 → 惰性窗口（回归修复） |
| `internal/sessionstore/managed_test.go` | 新增窗口可见性回归测试 |
| `internal/memory/mem0_client.go` | search/add 分离超时 |
| `internal/memory/writer.go` | 队列层超时 30s → 2min |
| `cmd/server/llm_gemini.go` | 记忆注入改到 system instruction；日志覆盖所有路径 |
| `cmd/server/main.go` | 透传 `WriteTimeout` |
| `.env` / `.env.example` | `DOUBAO_LLM_THINKING`、`MEM0_WRITE_TIMEOUT_SECONDS` |
| `/data/liuxin/mem0/server/main.py` | `custom_instructions` 两次调整 |

验证: `go build ./cmd/... ./internal/...` 通过，`go test ./internal/...` 通过。

### 当前状态

| 项 | 状态 |
|---|---|
| LLM 首 token | ✅ 0.4–0.7s（关闭 thinking） |
| 多轮上下文 | ✅ MaxTurns=3 窗口正确，当前发言可见 |
| 记忆检索 | ✅ E2E 验证（"你喜欢吃西瓜皮蘸酱油"） |
| 记忆写入 | ✅ 超时已放宽，不再静默丢失 |
| 记忆提取质量 | ⚠️ 新规则待验证（兴趣类记忆是否正确保留） |
| Barge-in | ❌ 未实现 |
| Echo/回声 | ⚠️ 需要 AEC，开发用耳机 |

### 遗留问题

1. **旧记忆数据是旧规则产物** — 新 `custom_instructions` 只对新写入生效，库里既有记忆不会追溯清理。
2. **mem0 写入耗时随库大小线性增长** — 长期使用需要关注。mem0 每次 add 都要检索相似记忆并让 LLM 逐条判定 ADD/UPDATE/DELETE，记忆上千条后可能需要分片或归档策略。
3. **`llmagent.Config.Description` 仍是英文** — 会进 system instruction，建议改中文。
4. **记忆注入无相关性阈值** — 当前固定注入 top 8，不管分数高低。噪声记忆仍可能挤掉有用记忆。
5. **`scripts/` 命令结构** — 已于 2026-09-01 拆分为独立 Go 命令目录，`go test ./...` 不再因重复 `main` 失败。

---

## Development Log: Barge-in（打断）+ 客户端播放器重写 + 记忆污染修复 (2026-08-03)

### 1. Barge-in — TTS/LLM 进行中被第二句打断

**改动前是严格半双工**，三处各自独立地阻断了打断：

- `consumeServerEndpointingTranscripts` 在单 goroutine 里**同步阻塞**调用 `runTextTurn`，turn 1 跑完前读不到第二句
- `ProcessAudio` 在 `turnActive` 为真时把 `vadOut.VADEnd` **整个吞掉**，本轮不发 SEGMENT_END，FunASR 永不解码第二句
- 客户端在播放期间**硬静音**，服务端连音频都收不到
- 且没有 per-turn 的 `context.CancelFunc`，`s.ctx` 只在整条 WS 断开时才取消

**触发时机**是句末打断：信号只能来自 FunASR 的 sentence-final，它依赖 Silero `VADEnd`（静音 200ms）+ SEGMENT_END 解码（64-90ms）。用户说话时 TTS 仍在播，停下约 300ms 后切断。说话即时压低（ducking）需要 `AudioProcessor` 暴露 speech-start 信号，未做。

**回声前提**：本轮明确不处理 TTS 回采，开发戴耳机。方案假定输入无回声，不做文本相似度或时序过滤。`BARGE_IN_ENABLED=false` 可回退到半双工。

#### 核心机制（`internal/voice/session.go`）

**turn 异步化 + 输出纪元**。sentence 循环只做调度，turn 跑在独立 goroutine 里带自己的可取消 context：

```go
turnMu     sync.Mutex          // 只保护 turnCancel / turnDone
turnCancel context.CancelFunc
turnDone   chan struct{}
turnSeq    atomic.Uint64       // 输出纪元
emitMu     sync.Mutex          // 让「校验纪元+发送」相对「递增纪元」原子
closed     chan struct{}       // 解开 teardown 时阻塞的 emit
```

全部 8 处 `s.outputs <-` 改走 `emit(seq, out)` / `emitControl(out)`。`emitMu` 不能用 atomic 替代——它保证"校验纪元后发送"与"递增纪元后发 interrupt"互斥，这是**杜绝陈旧帧排在 interrupt 之后**的依据。`select` 带 `<-s.closed` 分支顺带修掉一个既有隐患：api 侧 writer goroutine 已返回时，阻塞的 `s.outputs <-` 会永久卡住并让 `closeSession` 卡在 `<-writerDone`。

**打断序列**（`interruptTurn`）：

1. `turnMu` 下 copy-and-clear cancel/done（使并发调用幂等），**不持锁等待**（否则第二次打断死锁）
2. `emitMu` 下递增纪元 —— **在 cancel 之前**，让垂死 turn 解绕时产生的帧全部被丢弃
3. `cancel()` 停 LLM
4. `cancelActiveTTS()` 停 TTS
5. 等 `done`（`DrainTimeout` 上限）
6. 发 `interrupt` 标记 —— 在等待**之后**，所以正常路径下陈旧音频严格早于标记

**`tts_end` 必须被纪元丢弃**，这条比丢音频更要紧：客户端的 `tts_end` 分支会 `flush()`，即**播完**队列，与打断意图正好相反。

**TTS 取消必须 Cancel + Close 且 forwarder 持续排空，缺一不可**。豆包的两个硬约束：`Cancel`（`tts/doubao.go:140`）只写 CancelSession 帧，**不关闭 `Audio()`**；`readLoop`（:208）在 `s.audio <-` 上**阻塞发送**，没人再收就永久停住，`defer close(s.audio)` 永不执行——而 `Close()` 的 `conn.Close()` **解不开一个已阻塞在 channel 发送上的 goroutine**，结果 goroutine + 连接双泄漏。所以 `forwardTTSAudio` 改成一直排空到两个 channel 关闭（错误分支的 `return` 改 `continue`），取消后继续接收但不再 emit。

**结果分类先判 `ctx.Err()`**：拆连接会让本轮自己的 `Finish`/`WriteText` 写失败，那个写错误会盖住 `ctx.Err()`；不先判就会**每次打断都误报一个 `turn_error`**。这也顺带化解了 `agent/types.go:165` 的坑（`yield` 返回 false 时 `err == nil`，与正常完成无法区分）。

**`ProcessAudio` 放行 SEGMENT_END**：`turnActive` 的吞掉改为 `!bargeIn.Enabled && turnActive.Load()`；新增 `!asrWrote` 门避免强制解码空段、避免无谓重新触发服务端的 `_force_speech_on_next_audio`。`asrWrote` 仍只由 `ProcessAudio` goroutine 拥有。

**实现中发现的方案外错误**：ASR 流正常结束时原本会**取消**还在播的那一轮。输入结束不该截断正在说的回复——取消只在 `Close()` 时才对。故拆成 `awaitTurn()`（正常结束等待，不取消、不超时）和 `interruptTurn()`（拆除时取消）。`awaitTurn` 刻意不超时：提前返回会让调用方在 turn 还能 emit 时就关掉 outputs；而 `interruptTurn` 在自己的有限等待**之前**已retire 纪元，所以它放弃的 turn 已经被静音。这是既有测试 `session_tts_test.go` 抓出来的，最终该测试未经修改即通过。

**不使用 `rotateASR`**：它重新赋值 `s.asr`/`s.endpointing`，而 `s.asr` 由 `ProcessAudio` 独占，从 sentence 循环调用是数据竞争；更要紧的是循环只在 `:155-156` **捕获一次** channel，rotate 关闭旧 channel 会让循环把两者置 nil 从而**退出并关闭 `outputs`**，直接杀死会话。第二句已解码完成，直接当新一轮跑即可（额外开销 0ms）。

#### 客户端配合

`MIC_MUTE_DURING_TTS = False` 解开播放期静音（barge-in 成立的前提）；新增 `interrupt` 分支调 `player.stop()` 丢弃队列；`accepting` 标志结构性拒收有效播放窗口外的 PCM。

### 2. 客户端播放器重写 —— 修"聊十几轮后完全没反应"

**现象**：barge-in 正常工作十余轮后，客户端对服务端的回复完全无反应。

**排查**：一开始判断为"客户端没更新"（那次确实如此），但更新后复现。服务端收音频时没有任何日志，导致"没有 SEGMENT_END"分不清三种情况：客户端没发 / 发了但 VAD 没触发 / 被新加的 `asrWrote` 门挡住。加了两条探针后一次定位：

- `audio ingest alive`（每 100 帧 ≈ 20 秒一条，带累计帧数/语音帧数/`turn_active`）
- `vad end ignored, no audio since last segment end`

日志给出决定性证据——**服务端全程正常**：

```
16:20:50  ingest: chunks=700 speech=69 turn_active=False   ← 上行没断
16:20:58  SENTENCE: 你还在吗？你还在吗？                      ← 服务端确实收到并处理了
16:20:58  *** interrupted
```

用户以为没反应的那段时间里，服务端收到了"你还在吗"和"喂喂喂"并正常起了新一轮。卡住的是**客户端接收/渲染侧**。

**根因**：客户端每轮 TTS 都开关一次 PyAudio 输出流，打断时还额外多一次。反复几十次后 ALSA 被拖住，播放线程卡在 `stream.write()`，于是 `flush()` 干等 300 秒——而 `recv_messages` 正 `await` 它，WebSocket 一条消息都不再处理。麦克风是独立 task，所以照常上行。**这是解开静音后播放流开关变频繁带来的连带影响。**

**修法**：输出流整个会话只开一次并复用，播放线程常驻。

| 方法 | 之前 | 现在 |
|---|---|---|
| `start()` | 关旧流、开新流、起新线程 | 确保流存在（首次才开）、清队列 |
| `flush()` | 阻塞等播完 + 关流（最长 300s） | 只置标志，**不阻塞** |
| `stop()` | 阻塞 join 线程 + 关流 | 清队列，**不阻塞** |
| `close()`（新增） | — | 只在退出时关流和线程 |

因所有方法变为非阻塞，`recv_messages` 里几处 `run_in_executor` 一并去掉——少一层就少一个卡点。原先靠关流避免尾音截断的 300ms 静音也不再需要：流不关，写进去的音频一定播完。

**行为变化**：`[TTS end]` 现在在音频播完**之前**打印（含义变为"服务端发完了"），仅影响显示时序。

### 3. 记忆污染 —— "问我是谁答不知道"

记忆没丢，是被污染 + 被挤掉。

**直接原因**：一条自相矛盾的否认记忆，来自用户说的"我可没说过"：

```
0.858  用户否认自己曾说过自己喜欢吃西瓜皮、对故事、篮球运动员以及《出师表》感兴趣，
       也否认曾同意被称呼为'爸爸'
```

mem0 把一次对话纠正存成了**独立事实**，而不是去更新被否认的那几条。它与真实身份记忆并列注入，LLM 读到"用户否认曾同意被称呼为爸爸"就答不知道。

**放大原因**：21 条里 14 条是"用户对 X 感兴趣"（世界杯冠军、姚明身高、科比、艺术与诗歌、故事、幽默故事……）。这是上一轮修"数学问题没被记住"时**改过头**的后果——让它提取请求背后的兴趣，它就给每个提问都造一条。且所有分数挤在 0.84–0.88 极窄区间，排序几乎不区分，top_k=8 全被占满。

**`custom_instructions` 第三次调整**：

- 否认/纠正**绝不存成新事实**；矛盾是"去更新或删除被它否认的那条"的指令，不留纠正痕迹
- 单次提问不造兴趣记忆——问一次是好奇不是特质，只有明确表达喜好或反复回到某话题才存；宁可不存也别存"用户对 X 感兴趣"
- 身份与称呼提到规则首位

> 三次调整的教训串起来：第一次存了太多噪声（一次性指令、寒暄），第二次过滤过头（连兴趣一起丢），第三次噪声换了形态（每问必造兴趣 + 否认入库）。规则要针对"记成什么形式"，而不是"是否是某类输入"。

**顺带发现一个会误导排查的接口缺陷**：数据库里 21 条，但 `GET /memories?user_id=` 只返回 20 条，漏掉的正好是那条否认记忆——`search`（agent 实际走的路径）能看到它。**查看记忆应以数据库为准**：

```bash
PGPASSWORD=mem0pass psql -h 127.0.0.1 -U mem0 -d mem0 -tAc \
  "SELECT payload->>'data' FROM memories WHERE payload->>'user_id'='test-user';"
```

已清空 test-user 全部 21 条，从零用新规则重新积累（数据库计数与 `search` 双向确认为 0）。

### 本轮改动文件

| 文件 | 改动 |
|---|---|
| `internal/voice/session.go` | turn 异步化、per-turn cancel、纪元 emit、`cancelActiveTTS`/`abortSpeech`/`awaitTurn`、forwarder 排空、`ProcessAudio` 放行 SEGMENT_END、`Close()` 取消在飞 turn、两条摄入探针 |
| `internal/voice/barge_in.go`（新） | `BargeInConfig` 及零值归一化 |
| `internal/voice/barge_in_test.go`（新） | 可取消的 runner/TTS fake + 5 个测试 |
| `client/voice_client.py` | 播放器重写为常驻输出流、解静音、`interrupt` 处理、`accepting` 标志 |
| `internal/config/config.go`、`internal/api/server.go`、`cmd/server/main.go` | `BargeInConfig` 透传 + `WithBargeIn` + 启动日志 |
| `.env`、`.env.example` | `BARGE_IN_ENABLED`、`BARGE_IN_DRAIN_TIMEOUT_MS` |
| `/data/liuxin/mem0/server/main.py` | `custom_instructions` 第三次调整 |

### 验证

新增 5 个测试，并做**变异验证**确认不是空转：

| 测试 | 破坏什么会让它失败 |
|---|---|
| `TestBargeInCancelsRunningTurn` | — |
| `TestBargeInEmitsNoTTSEndForCancelledTurn` | 同时移除纪元守卫和 forwarder 的 ctx 抑制 → 抓到泄漏的 `tts_end` |
| `TestBargeInDuringLLMBeforeTTS` | 覆盖 `abortSpeech` 的 `speechDone == nil` 路径 |
| `TestSegmentEndSentDuringActiveTurn` | 两种 flag 状态都断言（锁住回退路径） |
| `TestSessionOutputsCloseWhileTurnInFlight` | `Close()` 不再取消 turn → outputs 永不关闭 |

单个守卫移除时测试仍通过——两道防线互为冗余，这是设计意图；同时移除才失败，说明测试有效。

途中 fake 暴露一个真实细节：`Cancel` 会被调用两次（打断路径 + turn 自己的收尾）。真实豆包客户端第二次只返回写错误，fake 起初未建模而 panic，已按同样契约修正。

`gofmt`/`go vet` 干净，`go test -race ./internal/...` 全绿，既有测试未经修改通过。

### 当前状态

| 项 | 状态 |
|---|---|
| Barge-in | ✅ 已实现并 E2E 验证（4 次打断全部 `turn_active=True`） |
| 客户端长时间稳定性 | ✅ 播放器重写，已消除 ALSA 拖垮导致的渲染停滞 |
| 记忆提取规则 | ⚠️ 第三版待验证（否认不入库、单次提问不造兴趣） |
| test-user 记忆 | 已清空，从零积累 |
| Echo/回声 | ⚠️ 仍需 AEC；当前靠耳机 + `BARGE_IN_ENABLED=false` 回退 |

### 遗留问题（累积）

#### 2026-08-11: 运行时 VAD 漏检调参

在长 TTS 后的真实客户端日志中，麦克风 PCM 仍持续上传，但部分用户语音窗口的 Silero 最大概率只有 `0.492` 或 `0.323`，低于原来的 `VAD_THRESHOLD=0.5`，不会形成新 ASR 段。运行默认阈值调整为 `0.4`；`VAD_MIN_SPEECH_MS=250` 与 `VAD_MIN_SILENCE_MS=350` 保持不变。该调整只降低漏检概率，外放环境的回声分离仍需要 AEC。

1. **无 AEC 则 barge-in 与外放不可兼得** — 播放期麦克风信号在物理上无法区分"用户说话"和"自己的 TTS 绕回"。外放需 AEC，或把 `BARGE_IN_ENABLED` 与客户端 `MIC_MUTE_DURING_TTS` 一起翻回半双工。
2. **打断即时性受限于句末检测** — 约 300ms（静音 200ms + 解码 ~80ms）。要做说话即时压低需给 `vad.AudioOutput` 加 `SpeechStarted`（`audio_gate.go` 已算出该状态，只是没暴露），但无 AEC 时会被回声频繁误触发。
3. **被打断的 turn 不写 mem0** — `HandleTextTurnStream` 在 `writer.Enqueue` 之前就因 error 返回，而 ADK session 里可能已存用户消息 + 部分助手回复。行为与改动前一致，未改动；**半写事件正是 `sessionstore` 两次 off-by-one 的形状，值得专门 E2E 复验**（打断后问"我刚才说了什么"）。
4. **FunASR 每句状态重置** — 已于 2026-08-12 实现：外部 VAD 的 `SEGMENT_END` 只返回本句 final，随后清空音频缓冲和句子状态，WebSocket 保持连接；Go 侧已删除历史 `seen` 去重。
5. **`GET /memories` 漏报最后一条** — 查看记忆请以数据库为准（见上）。
6. **mem0 写入耗时随库大小增长** — 1 条时 4.5s，20 条时 >30s。当前靠放宽超时绕过，不是解决。
7. **记忆注入无相关性阈值** — 固定 top 8 不看分数。分数普遍挤在 0.84–0.88，排序区分度低，噪声仍可能挤掉有用记忆。
8. **`llmagent.Config.Description` 仍是英文** — 会进 system instruction。
9. **`scripts/` 命令结构** — 已于 2026-09-01 拆分为独立 Go 命令目录，`go test ./...` 不再因重复 `main` 失败。

---

## Development Log: Silero VAD 单一断句权与 FunASR 长连接（2026-08-11）

### 目标与结论

语音输入的句子边界统一由本地 Silero VAD 决定，FunASR 仅负责在同一条 WebSocket 上持续接收已通过 Silero 门控的 PCM、返回 partial，并在收到 `SEGMENT_END` 时强制产出该句 final。整条麦克风会话结束时才发送 `STOP` 并关闭 ASR 连接。

```text
microphone PCM
  -> Silero VAD（门控、预滚、350 ms 静音切句、15 s 强切）
  -> long-lived FunASR WebSocket
  -> VADEnd / max-speech => SEGMENT_END
  -> FunASR final transcript
  -> ADK runner（memory / tools / session / LLM）
  -> LLM text stream -> TTS stream -> client audio
```

因此，一个客户端 WebSocket 对应一个 `VoiceSession` 和一条 FunASR WebSocket；一句话结束不会新建或关闭 FunASR 连接。`VAD_MAX_SPEECH_MS=15000` 也会产生 `VADEnd`，走同一条 `SEGMENT_END` 路径。

### 修复的双 VAD 问题

此前文档和 Go 调度已经按上述协议工作，但 FunASR 服务端在 `SEGMENT_END` 时仍读取内部 FSMN-VAD 的 `current_speech_start`。这意味着内部 VAD 仍是隐式的第二个断句状态来源：Silero 已经切句而 FSMN-VAD 未进入 speech 状态时，强制解码可能没有有效音频范围，表现为说话后偶发不识别。

FunASR 增加独立的 `--external-vad` 模式：

- 首个由 Silero 放行的 PCM 记录 `segment_start_sample`。
- `add_audio` 在该模式下不调用 FSMN-VAD。
- `SEGMENT_END` 解码 `[segment_start_sample, current_end]`，完成后仅重置当前外部句子状态，WebSocket 保持连接。
- `STOP` 只在整条麦克风会话结束时收尾当前未结束片段并结束任务。
- 外部模式不加载 FSMN-VAD 模型，启动日志应包含 `External VAD mode: FSMN-VAD disabled`。

外部服务代码不纳入本仓库；可复现补丁位于 `deploy/funasr/external-vad.patch`，启动器 `deploy/funasr/run-funasr-nano.sh` 会针对 Fun-ASR 基线提交 `3f95db6` 自动应用该补丁并传入 `--external-vad`。补丁已在干净 worktree 中通过 `git apply --check`、应用和 `git diff --check` 验证。

### 额外稳定性修复

ASR 连接被对端关闭时，API 路径可能显式调用 `VoiceSession.Close()`，随后 defer 再调用一次。旧实现只让 `closed` channel 的关闭幂等，但仍会重复调用 `s.vad.Close()`；Silero ONNX session 的 `Destroy` 不可重复执行，会触发 Cgo `SIGSEGV`。

`VoiceSession.Close()` 现将完整的资源清理（取消 turn、TTS、VAD、ASR）纳入同一个 `sync.Once`，并缓存第一次的错误。新增 `TestVoiceSessionCloseReleasesResourcesOnce`，覆盖两次 `Close()` 后 VAD 和 ASR 各只关闭一次。

### 验证

- FunASR 外部 VAD 状态机测试通过：连续两个 `SEGMENT_END` 都能产出 final，且不访问内部 VAD 状态。
- `python -m py_compile /data/liuxin/Fun-ASR/serve_realtime_ws.py` 通过。
- `go test ./internal/voice ./internal/tts` 通过。
- `go build -o server ./cmd/server` 通过。
- 端到端命令：

  ```bash
  go run ./scripts/vad-funasr-e2e internal/vad/testdata/test2.wav
  ```

  同一条 FunASR WebSocket 收到 3 个 Silero 边界并返回 3 句 final：

  1. `一二三四五六七，七六五四三二一。`，79 ms
  2. `你好，你好。`，57 ms
  3. `今天天气怎么样？`，102 ms

  随后的 `STOP` 返回 stopped，证明句间没有重连、会话结束才关闭。

### LLM / TTS 直连（2026-08-11）

主服务仍保留 `http_proxy` / `https_proxy` 供其他外部依赖使用，但 `NO_PROXY` 和 `no_proxy` 增加了 `ark.cn-beijing.volces.com` 与 `openspeech.bytedance.com`。因此豆包 Ark LLM 的 HTTPS 和豆包 TTS 的 WSS 均直连，不经 `127.0.0.1:8118`；本地 FunASR 已在 localhost 排除项内。此前一次 `TLS handshake timeout` 发生在 LLM 经 8118 建连时，调整后不再依赖该代理链路。

### Silero VAD 概率遥测（2026-08-11）

实时客户端曾出现“音频持续上行但没有 ASR final”。已有探针只能说明 PCM 峰值和 RMS，无法区分“Silero 概率始终低于阈值”与“VAD 状态机未起段”。`vad.AudioOutput` 现携带每次处理的推理帧数及概率区间，`VoiceSession` 每 100 个客户端块（约 20 秒）记录 `vad_frames`、`vad_probability_min`、`vad_probability_max`，不保存或输出音频内容。新增 `TestAudioGateReportsInferenceProbabilityRange` 锁定该统计。

该遥测用于依据真实概率决定是否调整 `VAD_THRESHOLD`，避免仅凭 PCM 振幅盲目降低阈值而引入回声/噪声误触发。

### 长 TTS 的打断队列修复（2026-08-11）

**根因**：WebSocket 写泵原来将 `tts_audio` 与 `interrupt` 放入同一个容量 256 的 FIFO。大段 TTS 生成速度快于本地播放时，`interrupt` 会排在数秒旧 PCM 后；即使服务端已取消 turn，客户端也只能继续接收、排队和播放旧音频。客户端播放器本身也是无界 deque，已经到达客户端的 PCM 不能由服务端撤回。

**修复**：

- `internal/api/server.go` 把输出拆分为普通消息、按序媒体、优先控制三路。`interrupt` 与 `tts_error` 走控制通道；写泵发送控制前清空尚未发送的媒体队列。`tts_end` 保持在媒体队列尾部，仍严格位于该 turn 的音频之后。
- 客户端在收到 `interrupt` 时继续清空本地待播队列。曾尝试对正常播放队列设定 800 ms 硬上限，但 TTS 服务会短时突发发送，丢弃最旧 PCM 会造成朗读缺字，已撤回；正常播放不得主动丢帧。

服务端新增 `TestDiscardQueuedVoiceMedia`；API、voice、VAD 测试和服务端构建均通过，客户端已通过 `python3 -m py_compile client/voice_client.py`。实际客户端部署在另一台机器时，需要同步客户端修改以获得 `interrupt` 的即时清队列行为。

### 当前部署状态

- FunASR：`funasr-nano-external-vad.service`，以 `--external-vad` 运行在 `127.0.0.1:10095`。
- 主服务：`xin-chat-robot-agent.service`。
- 本次主服务二进制已重建；完成服务重启后，客户端需重新建立 WebSocket 连接。

### 当日改动清单（2026-08-11）

**本仓库：**

- `internal/voice/session.go`：TTS 文本分段从 24 调整为 12 个 rune；关闭路径改为完整资源清理的幂等 `sync.Once`；每 20 秒记录 Silero 推理帧数和概率范围。
- `internal/vad/{types,audio_gate}.go`：将每个输入块的 Silero 推理帧数、最小概率、最大概率传递给会话遥测；对应单元测试已补充。
- `internal/api/server.go`：WebSocket 输出拆为普通消息、媒体、控制三条队列；`interrupt`/`tts_error` 优先发送并清除尚未写出的旧 TTS PCM。`TestDiscardQueuedVoiceMedia` 覆盖该行为。
- `internal/tts/{text_chunker,llm_tts_real_test}.go`：默认 TTS 输入块改为 12 个 rune，标点仍可提前切分。
- `.env.example`：Ark LLM 和豆包 TTS 域名加入 `NO_PROXY`/`no_proxy`；示例 Silero 阈值改为 `0.4`。当前实际 `.env` 同样为 `VAD_THRESHOLD=0.4`、`VAD_MIN_SPEECH_MS=250`、`VAD_MIN_SILENCE_MS=350`。
- `test/session_context_load.go` 与 `SESSION_CONTEXT_LOAD_REPORT.md`：新增隔离的 PostgreSQL 上下文负载验证。550 个语义 turn / 1,100 个 ADK event 下，`conversation_load_context` p95 为 1.03 ms、`adk_session_get` p95 为 4.37 ms；读取边界保持在 12 个语义 turn 和 48 个 ADK event。
- `README.md`、`deploy/funasr/README.md`：同步为“Silero 负责断句，FunASR 长连接按 `SEGMENT_END` final”的实际协议。

**仓库外 FunASR 依赖：**

- 外部工作树：`/data/liuxin/Fun-ASR/serve_realtime_ws.py`，相对上游当前为 75 行新增、16 行修改；可复现副本为本仓库的 `deploy/funasr/external-vad.patch`。
- 增加 `--external-vad`。启用时不加载、不实例化、不读取 FSMN-VAD；首个 Silero 放行帧记录 `segment_start_sample`，普通 decode 仅给当前外部段返回 partial，`SEGMENT_END` 解码该段并保留 WebSocket 供下一句复用，`STOP` 才结束整个会话。
- `deploy/funasr/run-funasr-nano.sh` 固定应用该补丁并以 `--external-vad` 启动，无法匹配 FunASR 的 `SEGMENT_END` 基线时失败退出，避免悄然退回双 VAD。
- 当前 transient service：`funasr-nano-external-vad.service`，命令参数包括 `--external-vad`、`--decode-interval 0.8`、`--gpu-memory-utilization 0.6`、监听 `127.0.0.1:10095`。外部工作树另有 `.venv/` 与 `funasr.log`，不是源代码改动。

**验证与限制：**

- 已通过：FunASR 外部 VAD 状态机、`python -m py_compile /data/liuxin/Fun-ASR/serve_realtime_ws.py`、相关 Go 单测、服务端构建和同一条 ASR WebSocket 的三句 E2E。
- 长 TTS 后的漏识别已从日志确认是部分人声 Silero 概率低于 `0.5`，阈值已降为 `0.4` 并重启主服务；需要重新连接客户端后验证实际效果。
- 外放环境缺少 AEC 时，TTS 回声与用户人声仍无法可靠分离；这不是本次阈值和队列修复可以根治的问题。


## Development Log: Per-Segment FunASR State Reset (2026-08-12)

The external-VAD protocol now treats every `SEGMENT_END` as a complete ASR
segment lifecycle while keeping the WebSocket and loaded model alive:

```text
Silero VADEnd -> SEGMENT_END -> decode current segment -> return one final
-> reset per-segment ASR state -> accept the next segment on the same socket
```

- `RealtimeASRSession.force_segment_end()` in `deploy/funasr/external-vad.patch`
  constructs its response before calling `reset()`. The response contains only
  the newly decoded sentence; `audio_buffer`, `locked_sentences`, partial state,
  and per-segment timestamps are then cleared.
- No model reload or WebSocket reconnect occurs between utterances.
- `deploy/funasr/per-segment-reset.patch` upgrades an already applied prior
  external-VAD patch; the launcher recognizes that state and requires the same
  explicit `FUNASR_APPLY_PATCH=1` opt-in before modifying it.
- `internal/voice/FunASRASR` no longer keeps a client-side `seen` map. Each
  `segment_end` response is authoritative and contains only new final output,
  so repeated utterances with the same text and relative timestamps are valid.
- `TestFunASRASRPublishesRepeatedSegmentTimes` covers that reset-safe behavior.

This keeps the FunASR service memory and response size bounded for a long-lived
microphone session. Speaker diarization remains disabled; enabling cross-segment
speaker identity in the future requires preserving that state separately.


## Development Log: Realtime Camera Understanding (2026-08-24, implementation complete; real service pending)

### Scope

First visual capability is deliberately a low-rate camera snapshot attached to
one spoken turn:

```text
camera -> latest JPEG only -> WebSocket video_frame control message
-> VoiceSession snapshot at ASR sentence end -> Doubao vision request
```

This is not video streaming. The client will send at most one compressed JPEG
per configured interval; the server retains only the most recent valid frame
for that WebSocket connection. When FunASR yields a final sentence, the frame
is snapshotted for that turn. A newer frame cannot change an in-flight request.

### Chosen wire format

The existing binary WebSocket frames remain PCM input and PCM TTS output. A
camera frame uses a text control message so the two media types cannot be
confused:

```json
{"type":"video_frame","mime_type":"image/jpeg","data":"<Base64 JPEG>"}
```

The server decodes and verifies Base64/JPEG before accepting it; client-supplied
width and height are intentionally not trusted. The planned initial limits are
512 KiB JPEG payload, 1280x720 maximum resolution, and a three-second maximum
frame age. Invalid or expired frames are ignored, so a normal voice turn still
works without camera input.

### Backend completed in this working tree

- Added `internal/vision.Frame` and `vision.Limits`. It validates Base64 JPEG,
  checks byte/dimension limits, copies image bytes defensively, and provides a
  request-context carrier. Frames are process-memory-only data.
- `internal/api/server.go` now recognizes `video_frame` when `VISION_ENABLED`
  is true. It validates the frame and calls `VoiceSession.SetLatestFrame`.
- `VoiceSession` retains only one guarded latest frame, snapshots it when a
  server-endpointing ASR sentence starts, and also supplies it to the legacy
  local-VAD finalization path. `Close()` clears the frame.
- `TextTurnInput` and `agent.TurnInput` carry the optional frame through the
  pipeline. `ADKRunner` places it in `context.Context` while still passing a
  text-only `genai.Content` to ADK. Consequently no image bytes are placed in
  ADK session events, PostgreSQL conversation records, or mem0 writes.
- `internal/llm/doubao.go` keeps using `DOUBAO_LLM_MODEL` for every turn. A
  camera frame changes only the last user message, which becomes text plus:

  ```json
  {"type":"image_url","image_url":{"url":"data:image/jpeg;base64,<...>"}}
  ```

  Therefore `DOUBAO_LLM_MODEL` must reference a multimodal Ark endpoint whenever
  `VISION_ENABLED=true`; no second model name is configured or selected.
- Added configuration types and server wiring for `VISION_ENABLED`,
  `VISION_MAX_FRAME_BYTES`, `VISION_MAX_WIDTH`, `VISION_MAX_HEIGHT`,
  and `VISION_MAX_FRAME_AGE_SECONDS`. The visual request reuses the configured
  `DOUBAO_LLM_MODEL`.
- Added unit coverage for Base64 JPEG validation/copying, Base64 data-URL
  construction and same-model selection,
  and VoiceSession sentence-time frame forwarding.

Verification already passed after these backend changes:

```bash
gofmt -w internal/vision/frame.go internal/vision/frame_test.go \
  internal/agent/types.go internal/voice/pipeline.go internal/voice/session.go \
  internal/llm/doubao.go internal/config/config.go internal/api/server.go \
  cmd/server/main.go internal/llm/doubao_test.go internal/voice/session_test.go

go test ./internal/vision ./internal/llm ./internal/agent ./internal/voice \
  ./internal/api ./internal/config ./cmd/server
```

### Client completed

- `client/voice_client.py` now uses an OpenCV capture thread that keeps only the
  newest JPEG in memory. Capture defaults to 5 Hz (`VIDEO_CAPTURE_INTERVAL_SECONDS=0.2`),
  while upload defaults to one current frame per second
  (`VIDEO_SEND_INTERVAL_SECONDS=1`).
- The camera uploader sends the exact `video_frame` Base64 JSON protocol and
  shares one `asyncio.Lock` with PCM transmission, preventing interleaved
  WebSocket writes. Shutdown cancels all tasks and releases the camera.
- `client/requirements.txt` now includes `opencv-python`; `.env.example` and
  `README.md` document the server variables and launch command.
- `python3 -m py_compile client/voice_client.py` passes. No physical camera was
  opened by that syntax-only check, and no image file is written by the client.

### Additional protocol verification

`TestVoiceWebSocketPassesCameraFrameToTurn` opens a test WebSocket, sends a
valid Base64 JPEG `video_frame`, then sends PCM. It verifies that the completed
turn receives the expected 2x1 frame. This covers decoding, per-connection
storage, and sentence-time pipeline forwarding.

### Remaining work before real-service testing

1. Set `VISION_ENABLED=true` in local `.env` after confirming the existing
   `DOUBAO_LLM_MODEL` endpoint accepts image input.
2. Run a real camera + spoken question + Doubao visual endpoint test, recording
   only timing and byte-size metadata, never the frame contents.
3. Decide whether the first production camera profile needs lower resolution or
   a shorter frame-age limit after measuring Ark visual-token cost and latency.
