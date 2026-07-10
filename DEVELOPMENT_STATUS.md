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
