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
	Text       string
	IsFinal    bool
	Confidence float64
}

// StreamingASR is a connection-scoped ASR stream. Audio is written as VAD-gated
// chunks from one client audio stream. Finalize is called only when the
// client audio stream ends, so multiple VAD segments can share one ASR connection.
type StreamingASR interface {
	Write(ctx context.Context, chunk vad.Chunk) error
	Finalize(ctx context.Context) (Transcript, error)
	Reset() error
	Close() error
}

type VoiceSessionConfig struct {
	UserID    string
	SessionID string
	Metadata  map[string]string
}

type VoiceTurnOutput struct {
	Transcript Transcript     `json:"transcript"`
	Response   TextTurnOutput `json:"response,omitempty"`
	Final      bool           `json:"final"`
}

// VoiceSession is the per-client-connection runtime. It owns VAD and ASR stream
// state for the connection, then starts an ADK text turn only after ASR returns
// a final transcript.
type VoiceSession struct {
	pipeline *Pipeline
	vad      vad.AudioProcessor
	asr      StreamingASR
	cfg      VoiceSessionConfig
	asrWrote bool
}

func NewVoiceSession(pipeline *Pipeline, processor vad.AudioProcessor, asr StreamingASR, cfg VoiceSessionConfig) (*VoiceSession, error) {
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
	return &VoiceSession{
		pipeline: pipeline,
		vad:      processor,
		asr:      asr,
		cfg:      cfg,
	}, nil
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
	if !chunk.Final {
		return VoiceTurnOutput{Final: false}, nil
	}
	if !s.asrWrote {
		return VoiceTurnOutput{Final: true}, nil
	}

	transcript, err := s.asr.Finalize(ctx)
	if err != nil {
		return VoiceTurnOutput{}, err
	}
	transcript.IsFinal = true
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
