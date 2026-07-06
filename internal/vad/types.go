package vad

import "context"

const (
	DefaultSampleRate           = 16000
	DefaultThreshold            = 0.5
	DefaultMinSilenceDurationMs = 200
	DefaultMinSpeechDurationMs  = 250
	DefaultSpeechPadMs          = 30
	DefaultPreRollMs            = 800
	DefaultAudioBufferSeconds   = 5
)

type Chunk struct {
	PCM16LE    []byte
	Samples    []float32
	SampleRate int
	Final      bool
}

type Segment struct {
	SpeechStartAt float64 `json:"speech_start_at"`
	SpeechEndAt   float64 `json:"speech_end_at,omitempty"`
}

type Decision struct {
	SpeechActive  bool      `json:"speech_active"`
	SpeechStarted bool      `json:"speech_started"`
	SpeechEnded   bool      `json:"speech_ended"`
	Segments      []Segment `json:"segments,omitempty"`
	SampleRate    int       `json:"sample_rate"`
	Processed     int       `json:"processed_samples"`
	Buffered      int       `json:"buffered_samples"`
	Final         bool      `json:"final"`
}

type AudioOutput struct {
	Chunk  Chunk `json:"chunk"`
	VADEnd bool  `json:"vad_end"`
}

type AudioProcessor interface {
	Process(ctx context.Context, chunk Chunk) (AudioOutput, error)
	Reset() error
	Close() error
}
