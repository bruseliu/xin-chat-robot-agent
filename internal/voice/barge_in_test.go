package voice

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/tts"
	"xin-chat-robot-agent/internal/vad"
)

var errStreamClosed = errors.New("stream closed")

// cancellableRunner yields one delta, signals that the turn is under way, then
// blocks until the turn's context is cancelled or the test releases it.
type cancellableRunner struct {
	mu      sync.Mutex
	calls   int
	cancels int

	started chan struct{} // buffered; one token per turn that reached the block
	release chan struct{}
}

func newCancellableRunner() *cancellableRunner {
	return &cancellableRunner{started: make(chan struct{}, 8), release: make(chan struct{})}
}

func (r *cancellableRunner) RunTurnStream(ctx context.Context, _ agent.TurnInput, yield func(agent.StreamEvent) bool) (agent.TurnOutput, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	if yield != nil {
		yield(agent.StreamEvent{Type: agent.StreamEventLLMDelta, TextDelta: "回", Partial: true})
	}
	r.started <- struct{}{}
	select {
	case <-ctx.Done():
		r.mu.Lock()
		r.cancels++
		r.mu.Unlock()
		return agent.TurnOutput{}, ctx.Err()
	case <-r.release:
		return agent.TurnOutput{Text: "回答"}, nil
	}
}

func (r *cancellableRunner) RunTurn(ctx context.Context, in agent.TurnInput) (agent.TurnOutput, error) {
	return r.RunTurnStream(ctx, in, nil)
}

func (r *cancellableRunner) counts() (calls, cancels int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.cancels
}

func (r *cancellableRunner) awaitStart(t *testing.T) {
	t.Helper()
	select {
	case <-r.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn to start")
	}
}

// cancellableTTS mirrors the Doubao stream's teardown contract: Cancel only
// records intent, and Close is the only thing that closes the audio/error
// channels — i.e. the only thing that can end the session's forwarder.
type cancellableTTS struct {
	audio     chan tts.AudioChunk
	errs      chan error
	closeOnce sync.Once
	cancelled atomic.Bool
	closed    atomic.Bool

	// emitOnCancel makes Cancel deliver a PCM chunk and a Final chunk, modelling
	// a provider that keeps sending briefly after CancelSession. These land after
	// the turn has been superseded and are exactly the frames that must not reach
	// the client.
	emitOnCancel bool

	mu    sync.Mutex
	texts []string
}

func newCancellableTTS() *cancellableTTS {
	return &cancellableTTS{audio: make(chan tts.AudioChunk, 8), errs: make(chan error, 1)}
}

func (f *cancellableTTS) WriteText(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return nil
}

func (f *cancellableTTS) Finish(context.Context) error {
	f.audio <- tts.AudioChunk{Final: true}
	return nil
}

func (f *cancellableTTS) Cancel(context.Context) error {
	f.cancelled.Store(true)
	// Cancel is legitimately called more than once for one stream (the
	// interrupting path and the turn's own teardown both do it). Mirror the real
	// client, where a Cancel after Close is just a failed write.
	if f.closed.Load() {
		return errStreamClosed
	}
	if f.emitOnCancel {
		f.audio <- tts.AudioChunk{PCM: []byte{9, 9}}
		f.audio <- tts.AudioChunk{Final: true}
	}
	return nil
}

func (f *cancellableTTS) Close() error {
	f.closeOnce.Do(func() {
		f.closed.Store(true)
		close(f.audio)
		close(f.errs)
	})
	return nil
}

func (f *cancellableTTS) Audio() <-chan tts.AudioChunk { return f.audio }
func (f *cancellableTTS) Errors() <-chan error         { return f.errs }
func (f *cancellableTTS) SampleRate() int              { return 16000 }

func (f *cancellableTTS) push(pcm []byte) { f.audio <- tts.AudioChunk{PCM: pcm} }

// segmentEndASR is a server-endpointing fake that also records SEGMENT_END
// notifications, which fakeServerEndpointingASR deliberately does not implement.
type segmentEndASR struct {
	sentences chan Transcript
	partials  chan Transcript
	notifies  atomic.Int64
}

func newSegmentEndASR() *segmentEndASR {
	return &segmentEndASR{sentences: make(chan Transcript, 4), partials: make(chan Transcript, 4)}
}

func (f *segmentEndASR) Write(context.Context, vad.Chunk) error       { return nil }
func (f *segmentEndASR) Finalize(context.Context) (Transcript, error) { return Transcript{}, nil }
func (f *segmentEndASR) Reset() error                                 { return nil }
func (f *segmentEndASR) Close() error                                 { return nil }
func (f *segmentEndASR) SentenceTranscripts() <-chan Transcript       { return f.sentences }
func (f *segmentEndASR) PartialTranscripts() <-chan Transcript        { return f.partials }
func (f *segmentEndASR) NotifySegmentEnd(context.Context) error {
	f.notifies.Add(1)
	return nil
}

// collectTypes drains outputs until stop returns true or the deadline passes.
func collectTypes(t *testing.T, out <-chan VoiceTurnOutput, stop func(types []string) bool) []string {
	t.Helper()
	var types []string
	deadline := time.After(3 * time.Second)
	for {
		select {
		case o, ok := <-out:
			if !ok {
				return types
			}
			types = append(types, o.Type)
			if stop(types) {
				return types
			}
		case <-deadline:
			t.Fatalf("timed out collecting outputs, got %v", types)
			return types
		}
	}
}

func contains(types []string, want string) bool {
	for _, v := range types {
		if v == want {
			return true
		}
	}
	return false
}

func TestBargeInCancelsRunningTurn(t *testing.T) {
	runner := newCancellableRunner()
	pipeline := NewPipeline(runner, nil, slog.Default())
	asr := newSegmentEndASR()
	first := newCancellableTTS()
	second := newCancellableTTS()
	var handed atomic.Int64
	session, err := NewVoiceSession(pipeline, &fakeVoiceVAD{}, asr, VoiceSessionConfig{
		UserID:  "alice",
		BargeIn: BargeInConfig{Enabled: true},
		TTSFactory: func(context.Context) (tts.Stream, error) {
			if handed.Add(1) == 1 {
				return first, nil
			}
			return second, nil
		},
	})
	if err != nil {
		t.Fatalf("NewVoiceSession: %v", err)
	}
	defer session.Close()

	asr.sentences <- Transcript{Text: "第一句", IsFinal: true}
	runner.awaitStart(t)
	first.push([]byte{1, 2})

	// Second utterance arrives while the first turn is still speaking.
	asr.sentences <- Transcript{Text: "第二句", IsFinal: true}
	runner.awaitStart(t)

	types := collectTypes(t, session.Outputs(), func(ty []string) bool {
		// Stop once the new turn has begun speaking.
		return contains(ty, "interrupt") && contains(ty[indexOf(ty, "interrupt"):], "sentence")
	})

	if !contains(types, "interrupt") {
		t.Fatalf("no interrupt emitted, types = %v", types)
	}
	if !first.cancelled.Load() || !first.closed.Load() {
		t.Fatalf("first TTS not torn down: cancelled=%v closed=%v", first.cancelled.Load(), first.closed.Load())
	}
	calls, cancels := runner.counts()
	if calls != 2 {
		t.Fatalf("runner calls = %d, want 2", calls)
	}
	if cancels != 1 {
		t.Fatalf("runner cancels = %d, want 1", cancels)
	}
}

func indexOf(types []string, want string) int {
	for i, v := range types {
		if v == want {
			return i
		}
	}
	return len(types)
}

// A cancelled turn must never emit tts_end. The client treats tts_end as "play
// out what is queued", so leaking one makes the user hear the rest of the reply
// they just interrupted — worse than leaking raw PCM, which the client discards.
func TestBargeInEmitsNoTTSEndForCancelledTurn(t *testing.T) {
	runner := newCancellableRunner()
	pipeline := NewPipeline(runner, nil, slog.Default())
	asr := newSegmentEndASR()
	first := newCancellableTTS()
	first.emitOnCancel = true
	second := newCancellableTTS()
	var handed atomic.Int64
	session, err := NewVoiceSession(pipeline, &fakeVoiceVAD{}, asr, VoiceSessionConfig{
		UserID:  "alice",
		BargeIn: BargeInConfig{Enabled: true},
		TTSFactory: func(context.Context) (tts.Stream, error) {
			if handed.Add(1) == 1 {
				return first, nil
			}
			return second, nil
		},
	})
	if err != nil {
		t.Fatalf("NewVoiceSession: %v", err)
	}
	defer session.Close()

	asr.sentences <- Transcript{Text: "第一句", IsFinal: true}
	runner.awaitStart(t)
	first.push([]byte{1, 2})

	asr.sentences <- Transcript{Text: "第二句", IsFinal: true}
	runner.awaitStart(t)

	// Turn 2 is left blocked in the LLM, so every frame up to the marker belongs
	// to the cancelled turn 1.
	types := collectTypes(t, session.Outputs(), func(ty []string) bool {
		return contains(ty, "interrupt")
	})

	i := indexOf(types, "interrupt")
	if i == len(types) {
		t.Fatalf("no interrupt emitted, types = %v", types)
	}
	if contains(types[:i], "tts_end") {
		t.Fatalf("cancelled turn announced tts_end, types = %v", types)
	}
	if contains(types[i+1:], "tts_end") {
		t.Fatalf("tts_end leaked after interrupt, types = %v", types)
	}
}

func TestBargeInDuringLLMBeforeTTS(t *testing.T) {
	runner := newCancellableRunner()
	pipeline := NewPipeline(runner, nil, slog.Default())
	asr := newSegmentEndASR()
	// No TTS factory: the interrupt lands while the turn is still in the LLM,
	// exercising abortSpeech's nil-stream path.
	session, err := NewVoiceSession(pipeline, &fakeVoiceVAD{}, asr, VoiceSessionConfig{
		UserID:  "alice",
		BargeIn: BargeInConfig{Enabled: true},
	})
	if err != nil {
		t.Fatalf("NewVoiceSession: %v", err)
	}
	defer session.Close()

	asr.sentences <- Transcript{Text: "第一句", IsFinal: true}
	runner.awaitStart(t)
	asr.sentences <- Transcript{Text: "第二句", IsFinal: true}
	runner.awaitStart(t)

	types := collectTypes(t, session.Outputs(), func(ty []string) bool {
		return contains(ty, "interrupt")
	})
	if !contains(types, "interrupt") {
		t.Fatalf("no interrupt emitted, types = %v", types)
	}
	if contains(types, "turn_error") {
		t.Fatalf("interrupt reported as turn_error, types = %v", types)
	}
	calls, cancels := runner.counts()
	if calls != 2 || cancels != 1 {
		t.Fatalf("runner calls/cancels = %d/%d, want 2/1", calls, cancels)
	}
}

func TestSegmentEndSentDuringActiveTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    int64
	}{
		{"barge_in_enabled", true, 1},
		{"barge_in_disabled", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newCancellableRunner()
			pipeline := NewPipeline(runner, nil, slog.Default())
			asr := newSegmentEndASR()
			processor := &fakeVoiceVAD{out: vad.AudioOutput{
				Chunk:  vad.Chunk{Samples: []float32{0.1}, SampleRate: 16000},
				VADEnd: true,
			}}
			session, err := NewVoiceSession(pipeline, processor, asr, VoiceSessionConfig{
				UserID:  "alice",
				BargeIn: BargeInConfig{Enabled: tc.enabled},
			})
			if err != nil {
				t.Fatalf("NewVoiceSession: %v", err)
			}
			defer session.Close()

			asr.sentences <- Transcript{Text: "第一句", IsFinal: true}
			runner.awaitStart(t)

			if _, err := session.ProcessAudio(context.Background(), AudioChunk{
				Samples: []float32{0.1}, SampleRate: 16000,
			}); err != nil {
				t.Fatalf("ProcessAudio: %v", err)
			}
			if got := asr.notifies.Load(); got != tc.want {
				t.Fatalf("SEGMENT_END notifies = %d, want %d", got, tc.want)
			}
		})
	}
}

// Tearing down while a turn is in flight must not leave that turn able to send on
// a closed outputs channel. A panic in the turn goroutine takes down the whole
// test binary, so reaching the end of this test is the assertion.
func TestSessionOutputsCloseWhileTurnInFlight(t *testing.T) {
	runner := newCancellableRunner()
	pipeline := NewPipeline(runner, nil, slog.Default())
	asr := newSegmentEndASR()
	session, err := NewVoiceSession(pipeline, &fakeVoiceVAD{}, asr, VoiceSessionConfig{
		UserID:  "alice",
		BargeIn: BargeInConfig{Enabled: true},
	})
	if err != nil {
		t.Fatalf("NewVoiceSession: %v", err)
	}

	asr.sentences <- Transcript{Text: "第一句", IsFinal: true}
	runner.awaitStart(t)

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(asr.sentences)
	close(asr.partials)

	deadline := time.After(3 * time.Second)
	closed := false
	for !closed {
		select {
		case _, ok := <-session.Outputs():
			if !ok {
				closed = true
			}
		case <-deadline:
			t.Fatal("outputs did not close after Close with a turn in flight")
		}
	}

	// Outputs is closed. Release the runner so any turn Close failed to reap now
	// finishes and tries to emit its result into that closed channel.
	close(runner.release)
	time.Sleep(100 * time.Millisecond)
}
