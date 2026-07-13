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
- `scripts/test_segment_end.go` — 协议级验证：对半切音频 + SEGMENT_END + 继续发送 + STOP
- `scripts/test_vad_funasr_e2e.go` — 完整链路：Silero VAD 驱动 + FunASR + SEGMENT_END + 延迟测量 + 去重

**运行命令**:
```bash
# 协议验证
go run scripts/test_segment_end.go internal/vad/testdata/test2.wav

# 完整 E2E (Silero VAD + FunASR)
go run scripts/test_vad_funasr_e2e.go internal/vad/testdata/test2.wav
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

The repository-wide command currently has a pre-existing structural failure unrelated to TTS:

```bash
go test ./...
```

The `scripts` directory contains multiple standalone `package main` programs in one directory (`test_funasr_realtime.go`, `test_segment_end.go`, and `test_vad_funasr_e2e.go`), so Go reports duplicate `main` declarations when treating that directory as one package. Each script remains runnable individually with `go run <file>`.

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

1. 安装 PostgreSQL 16 与 pgvector，创建 `mem0` 数据库和受限用户。
2. 修改 `/data/liuxin/mem0/server` 以启用 `FastEmbed`，并配置本地多语言 embedding 模型。
3. 配置 mem0 的记忆提炼 LLM、认证和 PostgreSQL 连接；本地开发可用 `AUTH_DISABLED=true`，生产环境必须启用 API key/JWT。
4. 启动 mem0 API 到 `localhost:8888`，验证 `/docs`、`/search`、`/memories`。
5. 用真实多轮语音验证：写入成功、随后查询命中、`[记忆上下文]` 注入 LLM；补充 mem0 HTTP 集成测试和安全的结构化召回日志。
