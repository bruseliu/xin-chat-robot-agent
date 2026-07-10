package voice

import (
	"context"
	"fmt"
	"strings"

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

type VoiceSessionConfig struct {
	UserID    string
	SessionID string
	Metadata  map[string]string
}

type VoiceTurnOutput struct {
	Type       string         `json:"type"`
	Transcript Transcript     `json:"transcript"`
	Response   TextTurnOutput `json:"response,omitempty"`
	Final      bool           `json:"final"`
	Error      string         `json:"error,omitempty"`
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
	response, err := s.pipeline.HandleTextTurn(ctx, TextTurnInput{
		UserID:    s.cfg.UserID,
		SessionID: s.cfg.SessionID,
		Text:      strings.TrimSpace(transcript.Text),
		Metadata:  metadata,
	})
	if err != nil {
		return VoiceTurnOutput{}, err
	}
	return VoiceTurnOutput{Transcript: transcript, Response: response, Final: true}, nil
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
	if err := s.vad.Close(); err != nil {
		first = err
	}
	if err := s.asr.Close(); err != nil && first == nil {
		first = err
	}
	return first
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
