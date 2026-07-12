package voice

import (
	"context"
	"fmt"
	"strings"
	"sync"

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

// ServerEndpointingASR emits a final transcript for each provider-detected sentence.
type ServerEndpointingASR interface {
	StreamingASR
	SentenceTranscripts() <-chan Transcript
	PartialTranscripts() <-chan Transcript
}

// SegmentEndNotifier is optionally implemented by ServerEndpointingASR providers
// that support a "segment end" signal from the client's local VAD. The provider
// force-decodes the current audio as a sentence without closing the connection.
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
}

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
	session := &VoiceSession{
		pipeline:   pipeline,
		vad:        processor,
		asr:        asr,
		asrFactory: factory,
		cfg:        cfg,
		ctx:        ctx,
	}
	if session.ctx == nil {
		session.ctx = context.Background()
	}
	if endpointing, ok := asr.(ServerEndpointingASR); ok {
		session.endpointing = endpointing
		session.outputs = make(chan VoiceTurnOutput, 16)
		go session.consumeServerEndpointingTranscripts()
	}
	return session, nil
}

// Outputs returns provider-driven sentence results for server-endpointing ASR.
func (s *VoiceSession) Outputs() <-chan VoiceTurnOutput {
	if s == nil {
		return nil
	}
	return s.outputs
}

func (s *VoiceSession) consumeServerEndpointingTranscripts() {
	defer close(s.outputs)
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
			output, err := s.runTextTurn(s.ctx, transcript)
			if err != nil {
				output = VoiceTurnOutput{Type: "sentence", Transcript: transcript, Final: true, Error: err.Error()}
			} else {
				output.Type = "sentence"
			}
			s.outputs <- output
		case partial, ok := <-partials:
			if !ok {
				partials = nil
				continue
			}
			if strings.TrimSpace(partial.Text) == "" {
				continue
			}
			s.outputs <- VoiceTurnOutput{Type: "partial", Transcript: partial}
		}
	}
}

func (s *VoiceSession) runTextTurn(ctx context.Context, transcript Transcript) (VoiceTurnOutput, error) {
	transcript.IsFinal = true
	metadata := cloneMetadata(s.cfg.Metadata)
	metadata["source"] = "voice_session"

	var speech tts.Stream
	var speechDone chan struct{}
	var speechWriteErr error
	wroteDelta := false
	textChunker := tts.NewTextChunker(24)
	startSpeech := func() error {
		if speech != nil || s.cfg.TTSFactory == nil || s.outputs == nil {
			return nil
		}
		var err error
		speech, err = s.cfg.TTSFactory(ctx)
		if err != nil {
			return err
		}
		speechDone = make(chan struct{})
		s.setActiveTTS(speech)
		s.outputs <- VoiceTurnOutput{Type: "tts_start", SampleRate: speech.SampleRate()}
		go s.forwardTTSAudio(speech, speechDone)
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
		if event.Type != agent.StreamEventLLMDelta || event.TextDelta == "" {
			return true
		}
		if startErr := startSpeech(); startErr != nil {
			speechWriteErr = startErr
			return false
		}
		s.outputs <- VoiceTurnOutput{Type: "llm_delta", Response: TextTurnOutput{Text: event.TextDelta}}
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
		if speech != nil {
			_ = speech.Cancel(ctx)
			_ = speech.Close()
		}
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
					_ = speech.Cancel(ctx)
					_ = speech.Close()
					return VoiceTurnOutput{}, err
				}
			}
		}
		if !wroteDelta && strings.TrimSpace(response.Text) != "" {
			if err := speech.WriteText(ctx, response.Text); err != nil {
				_ = speech.Cancel(ctx)
				_ = speech.Close()
				return VoiceTurnOutput{}, err
			}
		}
		if err := speech.Finish(ctx); err != nil {
			_ = speech.Close()
			return VoiceTurnOutput{}, err
		}
		select {
		case <-speechDone:
		case <-ctx.Done():
			_ = speech.Cancel(context.Background())
			_ = speech.Close()
			return VoiceTurnOutput{}, ctx.Err()
		}
		_ = speech.Close()
	}
	return VoiceTurnOutput{Transcript: transcript, Response: response, Final: true}, nil
}

func (s *VoiceSession) forwardTTSAudio(speech tts.Stream, done chan<- struct{}) {
	defer close(done)
	audio, errs := speech.Audio(), speech.Errors()
	for audio != nil || errs != nil {
		select {
		case chunk, ok := <-audio:
			if !ok {
				audio = nil
				continue
			}
			if len(chunk.PCM) > 0 {
				s.outputs <- VoiceTurnOutput{Type: "tts_audio", AudioPCM: chunk.PCM}
			}
			if chunk.Final {
				s.outputs <- VoiceTurnOutput{Type: "tts_end", Final: true}
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				s.outputs <- VoiceTurnOutput{Type: "tts_error", Error: err.Error()}
				return
			}
		}
	}
}

func (s *VoiceSession) ProcessAudio(ctx context.Context, chunk AudioChunk) (VoiceTurnOutput, error) {
	if s == nil {
		return VoiceTurnOutput{}, fmt.Errorf("nil voice session")
	}
	vadOut, err := s.vad.Process(ctx, vad.Chunk{
		PCM16LE:    chunk.PCM16LE,
		Samples:    chunk.Samples,
		SampleRate: chunk.SampleRate,
		Final:      chunk.Final,
	})
	if err != nil {
		return VoiceTurnOutput{}, err
	}
	if hasAudio(vadOut.Chunk) {
		if err := s.asr.Write(ctx, vadOut.Chunk); err != nil {
			return VoiceTurnOutput{}, err
		}
		s.asrWrote = true
	}
	if s.endpointing != nil {
		if chunk.Final {
			if _, err := s.asr.Finalize(ctx); err != nil {
				return VoiceTurnOutput{}, err
			}
			return VoiceTurnOutput{Final: true}, nil
		}
		if vadOut.VADEnd {
			if notifier, ok := s.endpointing.(SegmentEndNotifier); ok {
				if err := notifier.NotifySegmentEnd(ctx); err != nil {
					return VoiceTurnOutput{}, err
				}
			}
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

func cloneMetadata(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
