package voice

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xin-chat-robot-agent/internal/agent"
	"xin-chat-robot-agent/internal/tts"
	"xin-chat-robot-agent/internal/vad"
)

type AudioChunk struct {
	PCM16LE    []byte
	Samples    []float32
	SampleRate int
	Final      bool
}

type Transcript struct {
	Text       string  `json:"text"`
	IsFinal    bool    `json:"is_final,omitempty"`
	IsPartial  bool    `json:"is_partial,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// StreamingASR is a connection-scoped ASR stream. Legacy providers can use VAD
// segments, while server-endpointing providers keep one stream for the full input.
// Finalize ends the provider task when the client finishes its audio stream.
type StreamingASR interface {
	Write(ctx context.Context, chunk vad.Chunk) error
	Finalize(ctx context.Context) (Transcript, error)
	Reset() error
	Close() error
}

// ServerEndpointingASR emits a final transcript for each provider-confirmed
// sentence while retaining the connection for the full microphone session.
type ServerEndpointingASR interface {
	StreamingASR
	SentenceTranscripts() <-chan Transcript
	PartialTranscripts() <-chan Transcript
}

// SegmentEndNotifier is optionally implemented by ServerEndpointingASR providers
// that accept local VAD sentence boundaries. SEGMENT_END force-decodes the
// current audio as a sentence without closing the microphone connection.
type SegmentEndNotifier interface {
	NotifySegmentEnd(ctx context.Context) error
}

type ASRFactory func(ctx context.Context) (StreamingASR, error)
type TTSFactory func(ctx context.Context) (tts.Stream, error)

type VoiceSessionConfig struct {
	UserID     string
	SessionID  string
	Metadata   map[string]string
	TTSFactory TTSFactory
	BargeIn    BargeInConfig
}

type VoiceTurnOutput struct {
	Type       string         `json:"type"`
	Transcript Transcript     `json:"transcript"`
	Response   TextTurnOutput `json:"response,omitempty"`
	Final      bool           `json:"final"`
	Error      string         `json:"error,omitempty"`
	AudioPCM   []byte         `json:"-"`
	SampleRate int            `json:"sample_rate,omitempty"`
}

// VoiceSession is the per-client-connection runtime. Legacy ASR providers finalize
// a VAD segment, while server-endpointing providers emit text turns from their
// own sentence-final events and stay connected until client stream shutdown.
type VoiceSession struct {
	pipeline    *Pipeline
	vad         vad.AudioProcessor
	asr         StreamingASR
	asrFactory  ASRFactory
	cfg         VoiceSessionConfig
	asrWrote    bool
	endpointing ServerEndpointingASR
	outputs     chan VoiceTurnOutput
	ctx         context.Context
	ttsMu       sync.Mutex
	activeTTS   tts.Stream
	logger      *slog.Logger
	turnActive  atomic.Bool

	// turnMu guards turnCancel/turnDone, which describe the turn currently in
	// flight. Both are nil while idle.
	turnMu     sync.Mutex
	turnCancel context.CancelFunc
	turnDone   chan struct{}

	// turnSeq is the output epoch. A frame is only forwarded to the client while
	// its originating turn is still current, so a cancelled turn cannot emit as
	// it unwinds. emitMu makes "check epoch, then send" atomic with respect to
	// "bump epoch, then send the interrupt marker" — that pairing is what keeps
	// stale audio from landing after the marker.
	turnSeq atomic.Uint64
	emitMu  sync.Mutex

	// closed aborts a blocked emit during teardown, so Close() cannot deadlock
	// against a transport writer that has already gone away.
	closeOnce sync.Once
	closeErr  error
	closed    chan struct{}

	// Ingest counters, owned by the ProcessAudio goroutine.
	chunksIn          int64
	speechChunksIn    int64
	inputPeak         float64
	inputEnergy       float64
	inputSamples      int64
	vadFrames         int64
	vadProbabilityMin float64
	vadProbabilityMax float64
}

// audioProbeEveryChunks samples the ingest log. At the client's 200 ms frames
// this is roughly one line every 20 seconds.
const audioProbeEveryChunks = 100

func NewVoiceSession(pipeline *Pipeline, processor vad.AudioProcessor, asr StreamingASR, cfg VoiceSessionConfig) (*VoiceSession, error) {
	return newVoiceSession(context.Background(), pipeline, processor, asr, nil, cfg)
}

func NewVoiceSessionWithASRFactory(ctx context.Context, pipeline *Pipeline, processor vad.AudioProcessor, factory ASRFactory, cfg VoiceSessionConfig) (*VoiceSession, error) {
	if factory == nil {
		return nil, fmt.Errorf("voice session requires ASR factory")
	}
	asr, err := factory(ctx)
	if err != nil {
		return nil, err
	}
	return newVoiceSession(ctx, pipeline, processor, asr, factory, cfg)
}

func newVoiceSession(ctx context.Context, pipeline *Pipeline, processor vad.AudioProcessor, asr StreamingASR, factory ASRFactory, cfg VoiceSessionConfig) (*VoiceSession, error) {
	if pipeline == nil {
		return nil, fmt.Errorf("voice session requires pipeline")
	}
	if processor == nil {
		return nil, fmt.Errorf("voice session requires VAD processor")
	}
	if asr == nil {
		return nil, fmt.Errorf("voice session requires ASR stream")
	}
	if strings.TrimSpace(cfg.UserID) == "" {
		return nil, fmt.Errorf("voice session requires user id")
	}
	logger := pipeline.logger.With("user_id", cfg.UserID, "session_id", cfg.SessionID)
	cfg.BargeIn = cfg.BargeIn.normalize()
	session := &VoiceSession{
		pipeline:   pipeline,
		vad:        processor,
		asr:        asr,
		asrFactory: factory,
		cfg:        cfg,
		ctx:        ctx,
		logger:     logger,
		closed:     make(chan struct{}),
	}
	if session.ctx == nil {
		session.ctx = context.Background()
	}
	if endpointing, ok := asr.(ServerEndpointingASR); ok {
		session.endpointing = endpointing
		session.outputs = make(chan VoiceTurnOutput, 16)
		logger.Info("voice session started", "mode", "server_endpointing")
		go session.consumeServerEndpointingTranscripts()
	} else {
		logger.Info("voice session started", "mode", "local_vad")
	}
	return session, nil
}

// emit forwards a frame produced by turn seq, dropping it if that turn has since
// been superseded. Used for everything a turn produces, so a cancelled turn goes
// quiet the moment it is interrupted rather than as it finishes unwinding.
func (s *VoiceSession) emit(seq uint64, out VoiceTurnOutput) {
	if s.outputs == nil {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if seq != s.turnSeq.Load() {
		return
	}
	select {
	case s.outputs <- out:
	case <-s.closed:
	}
}

// emitControl forwards a frame that does not belong to any turn, so it is never
// epoch-filtered: ASR partials and the interrupt marker itself.
func (s *VoiceSession) emitControl(out VoiceTurnOutput) {
	if s.outputs == nil {
		return
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	select {
	case s.outputs <- out:
	case <-s.closed:
	}
}

// Outputs returns provider-driven sentence results for server-endpointing ASR.
func (s *VoiceSession) Outputs() <-chan VoiceTurnOutput {
	if s == nil {
		return nil
	}
	return s.outputs
}

// consumeServerEndpointingTranscripts dispatches turns; it must not block on one,
// otherwise a second utterance cannot be observed while the first is still
// speaking and barge-in is impossible.
func (s *VoiceSession) consumeServerEndpointingTranscripts() {
	defer func() {
		// Let an in-flight turn finish before closing outputs: the input stream
		// ending must not truncate a reply that is still being spoken, and its
		// goroutine would panic sending on a closed channel. Teardown is Close's
		// job — it cancels the turn, so this wait cannot outlive the session.
		s.awaitTurn()
		close(s.outputs)
	}()
	sentences := s.endpointing.SentenceTranscripts()
	partials := s.endpointing.PartialTranscripts()

	for sentences != nil || partials != nil {
		select {
		case transcript, ok := <-sentences:
			if !ok {
				sentences = nil
				continue
			}
			if !transcript.IsFinal || strings.TrimSpace(transcript.Text) == "" {
				continue
			}
			s.logger.Info("asr sentence", "text", transcript.Text)
			if s.turnActive.Load() {
				if !s.cfg.BargeIn.Enabled {
					// Half-duplex: let the current reply finish and drop this.
					s.logger.Info("sentence dropped, turn in progress", "text", transcript.Text)
					continue
				}
				s.interruptTurn("barge_in", true)
			}
			s.startTurn(transcript)
		case partial, ok := <-partials:
			if !ok {
				partials = nil
				continue
			}
			if strings.TrimSpace(partial.Text) == "" {
				continue
			}
			s.emitControl(VoiceTurnOutput{Type: "partial", Transcript: partial})
		}
	}
}

// startTurn runs one turn in its own goroutine and returns its output epoch.
func (s *VoiceSession) startTurn(transcript Transcript) uint64 {
	ctx, cancel := context.WithCancel(s.ctx)
	done := make(chan struct{})

	s.emitMu.Lock()
	seq := s.turnSeq.Add(1)
	s.emitMu.Unlock()

	s.turnMu.Lock()
	s.turnCancel, s.turnDone = cancel, done
	s.turnMu.Unlock()

	// Set on this goroutine, before the turn exists, so ProcessAudio never
	// observes an idle session while a turn is being set up.
	s.turnActive.Store(true)
	s.emit(seq, VoiceTurnOutput{Type: "sentence", Transcript: transcript})

	go func() {
		// Defers run LIFO: turnActive clears before done closes, so a caller that
		// has waited on done is guaranteed to see an idle session.
		defer close(done)
		defer cancel()
		defer s.turnActive.Store(false)

		output, err := s.runTextTurn(ctx, seq, transcript)
		switch {
		case ctx.Err() != nil:
			// Checked first: tearing down the TTS connection makes the turn's own
			// Finish/WriteText fail, and that write error would otherwise surface
			// as a spurious turn_error on every interrupt.
			s.logger.Info("turn interrupted", "text", transcript.Text)
		case err != nil:
			s.logger.Error("text turn failed", "error", err, "text", transcript.Text)
			s.emit(seq, VoiceTurnOutput{Type: "turn_error", Transcript: transcript, Final: true, Error: err.Error()})
		default:
			output.Type = "sentence"
			s.emit(seq, output)
		}
	}()
	return seq
}

// awaitTurn blocks until the in-flight turn finishes on its own. It deliberately
// does not cancel, and does not time out: returning early would let the caller
// close outputs while the turn can still emit. interruptTurn retires the epoch
// before its own bounded wait, so a turn it gave up on is already muted.
func (s *VoiceSession) awaitTurn() {
	s.turnMu.Lock()
	done := s.turnDone
	s.turnMu.Unlock()
	if done == nil {
		return
	}
	<-done
}

// interruptTurn cancels the in-flight turn and waits for it to unwind. Safe to
// call concurrently and when idle.
func (s *VoiceSession) interruptTurn(reason string, notify bool) {
	s.turnMu.Lock()
	cancel, done := s.turnCancel, s.turnDone
	s.turnCancel, s.turnDone = nil, nil
	s.turnMu.Unlock()
	if cancel == nil {
		return
	}

	// Retire the epoch before cancelling so nothing the dying turn produces on
	// the way out can reach the client.
	s.emitMu.Lock()
	s.turnSeq.Add(1)
	s.emitMu.Unlock()

	cancel()
	s.cancelActiveTTS()

	// Not holding turnMu here: a second interrupt would otherwise deadlock.
	select {
	case <-done:
	case <-time.After(s.cfg.BargeIn.DrainTimeout):
		s.logger.Warn("turn interrupt drain timed out", "reason", reason)
	}

	if notify {
		// After the wait, so in the normal path any stale audio strictly
		// precedes the marker and the client's drop is unambiguous.
		s.emitControl(VoiceTurnOutput{Type: "interrupt", Error: reason})
	}
}

func (s *VoiceSession) runTextTurn(ctx context.Context, seq uint64, transcript Transcript) (VoiceTurnOutput, error) {
	transcript.IsFinal = true
	metadata := cloneMetadata(s.cfg.Metadata)
	metadata["source"] = "voice_session"
	turnStartedAt := time.Now()

	var speech tts.Stream
	var speechDone chan struct{}
	var speechWriteErr error
	wroteDelta := false
	firstLLMDelta := false
	textChunker := tts.NewTextChunker(12)
	startSpeech := func() error {
		if speech != nil || s.cfg.TTSFactory == nil || s.outputs == nil {
			return nil
		}
		nextSpeech, err := s.cfg.TTSFactory(ctx)
		if err != nil {
			s.logger.Error("tts create failed", "error", err)
			return err
		}
		if nextSpeech == nil {
			return fmt.Errorf("tts factory returned nil stream")
		}
		speech = nextSpeech
		ttsStartedAt := time.Now()
		s.logger.Info("tts started", "sample_rate", speech.SampleRate(), "asr_final_to_tts_start", time.Since(turnStartedAt))
		speechDone = make(chan struct{})
		s.setActiveTTS(speech)
		s.emit(seq, VoiceTurnOutput{Type: "tts_start", SampleRate: speech.SampleRate()})
		go s.forwardTTSAudio(ctx, seq, speech, speechDone, turnStartedAt, ttsStartedAt)
		return nil
	}
	defer func() {
		if speech != nil {
			s.clearActiveTTS(speech)
		}
	}()

	response, err := s.pipeline.HandleTextTurnStream(ctx, TextTurnInput{
		UserID: s.cfg.UserID, SessionID: s.cfg.SessionID,
		Text: strings.TrimSpace(transcript.Text), Metadata: metadata,
	}, func(event agent.StreamEvent) bool {
		// The ADK runner does not check ctx between events, so stop the iteration
		// here as soon as the turn is cancelled.
		if ctx.Err() != nil {
			return false
		}
		if event.Type != agent.StreamEventLLMDelta || event.TextDelta == "" {
			return true
		}
		if !firstLLMDelta {
			firstLLMDelta = true
			s.logger.Info("llm first delta", "asr_final_to_llm_first", time.Since(turnStartedAt))
		}
		if startErr := startSpeech(); startErr != nil {
			speechWriteErr = startErr
			return false
		}
		s.emit(seq, VoiceTurnOutput{Type: "llm_delta", Response: TextTurnOutput{Text: event.TextDelta}})
		if speech != nil {
			wroteDelta = true
			for _, chunk := range textChunker.Push(event.TextDelta) {
				if speechWriteErr = speech.WriteText(ctx, chunk); speechWriteErr != nil {
					return false
				}
			}
		}
		return true
	})
	if err == nil && speechWriteErr != nil {
		err = speechWriteErr
	}
	if err != nil {
		s.abortSpeech(speech, speechDone)
		return VoiceTurnOutput{}, err
	}
	if speech == nil && s.cfg.TTSFactory != nil && strings.TrimSpace(response.Text) != "" {
		if err := startSpeech(); err != nil {
			return VoiceTurnOutput{}, err
		}
	}
	if speech != nil {
		if wroteDelta {
			if chunk := textChunker.Flush(); chunk != "" {
				if err := speech.WriteText(ctx, chunk); err != nil {
					s.abortSpeech(speech, speechDone)
					return VoiceTurnOutput{}, err
				}
			}
		}
		if !wroteDelta && strings.TrimSpace(response.Text) != "" {
			if err := speech.WriteText(ctx, response.Text); err != nil {
				s.abortSpeech(speech, speechDone)
				return VoiceTurnOutput{}, err
			}
		}
		if err := speech.Finish(ctx); err != nil {
			s.abortSpeech(speech, speechDone)
			return VoiceTurnOutput{}, err
		}
		select {
		case <-speechDone:
		case <-ctx.Done():
			s.abortSpeech(speech, speechDone)
			return VoiceTurnOutput{}, ctx.Err()
		}
		_ = speech.Close()
	}
	return VoiceTurnOutput{Transcript: transcript, Response: response, Final: true}, nil
}

// forwardTTSAudio pumps provider audio to the client until both provider channels
// close.
//
// It must keep receiving even once the turn is cancelled and even after an error:
// the Doubao read loop sends on its audio channel blocking, and closing the
// connection does not release a goroutine already parked on a channel send. If
// this loop stopped receiving, that goroutine would never reach its deferred
// close and both it and the connection would leak. So on cancellation we drain
// and discard rather than return.
func (s *VoiceSession) forwardTTSAudio(ctx context.Context, seq uint64, speech tts.Stream, done chan<- struct{}, turnStartedAt, ttsStartedAt time.Time) {
	defer close(done)
	audio, errs := speech.Audio(), speech.Errors()
	reportedErr := false
	firstAudio := true
	for audio != nil || errs != nil {
		select {
		case chunk, ok := <-audio:
			if !ok {
				audio = nil
				continue
			}
			if ctx.Err() != nil {
				continue
			}
			if len(chunk.PCM) > 0 {
				if firstAudio {
					firstAudio = false
					s.logger.Info("tts first audio", "asr_final_to_tts_first_audio", time.Since(turnStartedAt), "tts_start_to_first_audio", time.Since(ttsStartedAt))
				}
				s.emit(seq, VoiceTurnOutput{Type: "tts_audio", AudioPCM: chunk.PCM})
			}
			if chunk.Final {
				// Withheld for a cancelled turn: the client treats tts_end as
				// "play out what is queued", the opposite of an interrupt.
				s.emit(seq, VoiceTurnOutput{Type: "tts_end", Final: true})
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil && !reportedErr && ctx.Err() == nil {
				reportedErr = true
				s.emit(seq, VoiceTurnOutput{Type: "tts_error", Error: err.Error()})
			}
		}
	}
}

func (s *VoiceSession) ProcessAudio(ctx context.Context, chunk AudioChunk) (VoiceTurnOutput, error) {
	if s == nil {
		return VoiceTurnOutput{}, fmt.Errorf("nil voice session")
	}
	// Liveness probe: without it, "the assistant stopped responding" cannot be
	// told apart from "the client stopped sending". Sampled, not per-chunk.
	s.chunksIn++
	peak, energy, samples := audioLevel(chunk)
	if peak > s.inputPeak {
		s.inputPeak = peak
	}
	s.inputEnergy += energy
	s.inputSamples += samples
	vadOut, err := s.vad.Process(ctx, vad.Chunk{
		PCM16LE:    chunk.PCM16LE,
		Samples:    chunk.Samples,
		SampleRate: chunk.SampleRate,
		Final:      chunk.Final,
	})
	if err != nil {
		return VoiceTurnOutput{}, err
	}
	if vadOut.InferenceFrames > 0 {
		if s.vadFrames == 0 || float64(vadOut.ProbabilityMin) < s.vadProbabilityMin {
			s.vadProbabilityMin = float64(vadOut.ProbabilityMin)
		}
		if s.vadFrames == 0 || float64(vadOut.ProbabilityMax) > s.vadProbabilityMax {
			s.vadProbabilityMax = float64(vadOut.ProbabilityMax)
		}
		s.vadFrames += int64(vadOut.InferenceFrames)
	}
	if s.chunksIn%audioProbeEveryChunks == 0 {
		inputRMS := 0.0
		if s.inputSamples > 0 {
			inputRMS = math.Sqrt(s.inputEnergy / float64(s.inputSamples))
		}
		s.logger.Info("audio ingest alive", "chunks", s.chunksIn, "speech_chunks", s.speechChunksIn, "turn_active", s.turnActive.Load(), "input_peak", s.inputPeak, "input_rms", inputRMS, "vad_frames", s.vadFrames, "vad_probability_min", s.vadProbabilityMin, "vad_probability_max", s.vadProbabilityMax)
		s.inputPeak, s.inputEnergy, s.inputSamples = 0, 0, 0
		s.vadFrames, s.vadProbabilityMin, s.vadProbabilityMax = 0, 0, 0
	}
	if vadOut.VADStart && s.cfg.BargeIn.Enabled && s.turnActive.Load() {
		s.logger.Info("vad speech start detected, interrupting active turn")
		s.interruptTurn("barge_in", true)
	}
	if hasAudio(vadOut.Chunk) {
		if err := s.asr.Write(ctx, vadOut.Chunk); err != nil {
			return VoiceTurnOutput{}, err
		}
		s.asrWrote = true
		s.speechChunksIn++
	}
	if s.endpointing != nil {
		if chunk.Final {
			if _, err := s.asr.Finalize(ctx); err != nil {
				return VoiceTurnOutput{}, err
			}
			return VoiceTurnOutput{Final: true}, nil
		}
		if !s.cfg.BargeIn.Enabled && s.turnActive.Load() {
			// Half-duplex: withholding SEGMENT_END means the provider never
			// decodes anything said over the current reply, so it cannot be
			// interrupted — including by the assistant's own echo.
			return VoiceTurnOutput{Final: false}, nil
		}
		if vadOut.VADEnd {
			if !s.asrWrote {
				// Nothing new since the last segment end; force-decoding an empty
				// segment would only re-arm the provider's speech-start latch.
				s.logger.Info("vad end ignored, no audio since last segment end")
				return VoiceTurnOutput{Final: false}, nil
			}
			s.logger.Info("vad end detected, sending SEGMENT_END", "turn_active", s.turnActive.Load())
			if notifier, ok := s.endpointing.(SegmentEndNotifier); ok {
				if err := notifier.NotifySegmentEnd(ctx); err != nil {
					s.logger.Error("segment_end failed", "error", err)
					return VoiceTurnOutput{}, err
				}
			}
			s.asrWrote = false
		}
		return VoiceTurnOutput{Final: false}, nil
	}
	if vadOut.VADEnd {
		return s.finalizeASRTurn(ctx, chunk.Final)
	}
	if chunk.Final {
		if !s.asrWrote {
			return VoiceTurnOutput{Final: true}, nil
		}
		return s.finalizeASRTurn(ctx, true)
	}
	return VoiceTurnOutput{Final: false}, nil
}

func (s *VoiceSession) finalizeASRTurn(ctx context.Context, streamFinal bool) (VoiceTurnOutput, error) {
	if !s.asrWrote {
		return VoiceTurnOutput{Final: streamFinal}, nil
	}
	transcript, err := s.asr.Finalize(ctx)
	if err != nil {
		return VoiceTurnOutput{}, err
	}
	transcript.IsFinal = true
	s.asrWrote = false
	if streamFinal {
		_ = s.asr.Close()
	} else if err := s.rotateASR(ctx); err != nil {
		return VoiceTurnOutput{}, err
	}

	text := strings.TrimSpace(transcript.Text)
	if text == "" {
		return VoiceTurnOutput{Transcript: transcript, Final: true}, nil
	}

	metadata := cloneMetadata(s.cfg.Metadata)
	metadata["source"] = "voice_session"
	response, err := s.pipeline.HandleTextTurn(ctx, TextTurnInput{
		UserID:    s.cfg.UserID,
		SessionID: s.cfg.SessionID,
		Text:      text,
		Metadata:  metadata,
	})
	if err != nil {
		return VoiceTurnOutput{}, err
	}
	return VoiceTurnOutput{
		Transcript: transcript,
		Response:   response,
		Final:      true,
	}, nil
}

func (s *VoiceSession) rotateASR(ctx context.Context) error {
	if s.asrFactory == nil {
		return s.asr.Reset()
	}
	if s.asr != nil {
		_ = s.asr.Close()
	}
	next, err := s.asrFactory(ctx)
	if err != nil {
		return err
	}
	s.asr = next
	return nil
}

func (s *VoiceSession) Reset() error {
	if s == nil {
		return nil
	}
	if err := s.vad.Reset(); err != nil {
		return err
	}
	s.asrWrote = false
	return s.asr.Reset()
}

func (s *VoiceSession) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closeErr = s.closeResources()
	})
	return s.closeErr
}

func (s *VoiceSession) closeResources() error {
	// Release any emit blocked on a transport writer that has already gone away,
	// then cancel the in-flight turn so Close does not wait out a whole LLM+TTS
	// round trip.
	if s.closed != nil {
		close(s.closed)
	}
	s.interruptTurn("session_close", false)

	var first error
	s.ttsMu.Lock()
	activeTTS := s.activeTTS
	s.activeTTS = nil
	s.ttsMu.Unlock()
	if activeTTS != nil {
		if err := activeTTS.Cancel(context.Background()); err != nil {
			first = err
		}
		if err := activeTTS.Close(); err != nil && first == nil {
			first = err
		}
	}
	if err := s.vad.Close(); err != nil {
		first = err
	}
	if err := s.asr.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

func (s *VoiceSession) setActiveTTS(stream tts.Stream) {
	s.ttsMu.Lock()
	s.activeTTS = stream
	s.ttsMu.Unlock()
}

// cancelActiveTTS hard-stops whatever stream the running turn registered. Safe
// alongside that turn's own teardown: Close is idempotent, and clearActiveTTS
// only clears a matching stream so the turn's defer becomes a no-op.
func (s *VoiceSession) cancelActiveTTS() {
	s.ttsMu.Lock()
	stream := s.activeTTS
	s.activeTTS = nil
	s.ttsMu.Unlock()
	if stream == nil {
		return
	}
	// Cancel then Close, always in that order: Cancel writes a frame and fails
	// once the connection is gone.
	_ = stream.Cancel(context.Background())
	_ = stream.Close()
}

// abortSpeech is the turn-local teardown: stop the provider, then wait for the
// forwarder so no frame outlives the turn. Close is what actually ends the
// forwarder — Cancel alone leaves the provider's audio channel open.
func (s *VoiceSession) abortSpeech(speech tts.Stream, speechDone <-chan struct{}) {
	if speech == nil {
		return
	}
	_ = speech.Cancel(context.Background())
	_ = speech.Close()
	if speechDone == nil {
		return
	}
	select {
	case <-speechDone:
	case <-time.After(s.cfg.BargeIn.DrainTimeout):
		s.logger.Warn("tts forwarder drain timed out")
	}
}

func (s *VoiceSession) clearActiveTTS(stream tts.Stream) {
	s.ttsMu.Lock()
	if s.activeTTS == stream {
		s.activeTTS = nil
	}
	s.ttsMu.Unlock()
}

func hasAudio(chunk vad.Chunk) bool {
	return len(chunk.Samples) > 0 || len(chunk.PCM16LE) > 0
}

// audioLevel returns normalized peak and accumulated squared amplitude for a
// bounded diagnostic log. It never retains client audio.
func audioLevel(chunk AudioChunk) (peak, energy float64, samples int64) {
	consume := func(value float64) {
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
		energy += value * value
		samples++
	}
	if len(chunk.PCM16LE) > 1 {
		for i := 0; i+1 < len(chunk.PCM16LE); i += 2 {
			value := int16(uint16(chunk.PCM16LE[i]) | uint16(chunk.PCM16LE[i+1])<<8)
			consume(float64(value) / 32768)
		}
		return peak, energy, samples
	}
	for _, value := range chunk.Samples {
		consume(float64(value))
	}
	return peak, energy, samples
}

func cloneMetadata(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
