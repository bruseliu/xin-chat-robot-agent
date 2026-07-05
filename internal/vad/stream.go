package vad

import (
	"context"
	"encoding/binary"
	"fmt"
)

type Inferer interface {
	Infer(samples []float32) (float32, error)
	Reset() error
}

type StreamConfig struct {
	SampleRate           int
	Threshold            float32
	MinSilenceDurationMs int
	SpeechPadMs          int
}

type Stream struct {
	cfg               StreamConfig
	inferer           Inferer
	windowSize        int
	minSilenceSamples int
	speechPadSamples  int

	buffer      []float32
	currSample  int
	triggered   bool
	tempEnd     int
	segmentOpen Segment
}

func NewStream(cfg StreamConfig, inferer Inferer) (*Stream, error) {
	if inferer == nil {
		return nil, fmt.Errorf("vad: inferer is required")
	}
	cfg = normalizeStreamConfig(cfg)
	if err := validateStreamConfig(cfg); err != nil {
		return nil, err
	}
	windowSize := 512
	if cfg.SampleRate == 8000 {
		windowSize = 256
	}
	return &Stream{
		cfg:               cfg,
		inferer:           inferer,
		windowSize:        windowSize,
		minSilenceSamples: cfg.MinSilenceDurationMs * cfg.SampleRate / 1000,
		speechPadSamples:  cfg.SpeechPadMs * cfg.SampleRate / 1000,
	}, nil
}

func (s *Stream) Process(ctx context.Context, chunk Chunk) (Decision, error) {
	if s == nil {
		return Decision{}, fmt.Errorf("vad: nil stream")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	samples, sampleRate, err := chunkSamples(chunk, s.cfg.SampleRate)
	if err != nil {
		return Decision{}, err
	}
	if sampleRate != s.cfg.SampleRate {
		return Decision{}, fmt.Errorf("vad: sample rate changed from %d to %d", s.cfg.SampleRate, sampleRate)
	}

	s.buffer = append(s.buffer, samples...)
	decision := Decision{SampleRate: s.cfg.SampleRate, Final: chunk.Final}
	for len(s.buffer) >= s.windowSize {
		if err := ctx.Err(); err != nil {
			return Decision{}, err
		}
		frame := make([]float32, s.windowSize)
		copy(frame, s.buffer[:s.windowSize])
		s.buffer = s.buffer[s.windowSize:]

		probability, err := s.inferer.Infer(frame)
		if err != nil {
			return Decision{}, fmt.Errorf("vad: infer frame: %w", err)
		}
		s.currSample += s.windowSize
		decision.Processed += s.windowSize
		s.applyProbability(probability, &decision)
	}

	if chunk.Final && s.triggered {
		decision.SpeechEnded = true
		decision.addSegment(s.closeOpenSegment(s.currSample + s.speechPadSamples))
	}
	decision.SpeechActive = s.triggered
	decision.Buffered = len(s.buffer)
	return decision, nil
}

func (s *Stream) Reset() error {
	if s == nil {
		return fmt.Errorf("vad: nil stream")
	}
	s.buffer = nil
	s.currSample = 0
	s.triggered = false
	s.tempEnd = 0
	s.segmentOpen = Segment{}
	return s.inferer.Reset()
}

func (s *Stream) WindowSize() int {
	if s == nil {
		return 0
	}
	return s.windowSize
}

func (s *Stream) applyProbability(probability float32, decision *Decision) {
	if probability >= s.cfg.Threshold && s.tempEnd != 0 {
		s.tempEnd = 0
	}
	if probability >= s.cfg.Threshold && !s.triggered {
		s.triggered = true
		decision.SpeechStarted = true
		s.segmentOpen = Segment{SpeechStartAt: seconds(clampNonNegative(s.currSample-s.windowSize-s.speechPadSamples), s.cfg.SampleRate)}
		decision.Segments = append(decision.Segments, s.segmentOpen)
	}
	if probability < s.cfg.Threshold-0.15 && s.triggered {
		if s.tempEnd == 0 {
			s.tempEnd = s.currSample
		}
		if s.currSample-s.tempEnd < s.minSilenceSamples {
			return
		}
		decision.SpeechEnded = true
		decision.addSegment(s.closeOpenSegment(s.tempEnd + s.speechPadSamples))
	}
}

func (s *Stream) closeOpenSegment(endSample int) Segment {
	segment := s.segmentOpen
	segment.SpeechEndAt = seconds(endSample, s.cfg.SampleRate)
	s.tempEnd = 0
	s.triggered = false
	s.segmentOpen = Segment{}
	return segment
}

func (d *Decision) addSegment(segment Segment) {
	if len(d.Segments) > 0 {
		last := &d.Segments[len(d.Segments)-1]
		if last.SpeechEndAt == 0 && last.SpeechStartAt == segment.SpeechStartAt {
			*last = segment
			return
		}
	}
	d.Segments = append(d.Segments, segment)
}

func normalizeStreamConfig(cfg StreamConfig) StreamConfig {
	if cfg.SampleRate == 0 {
		cfg.SampleRate = DefaultSampleRate
	}
	if cfg.Threshold == 0 {
		cfg.Threshold = DefaultThreshold
	}
	if cfg.MinSilenceDurationMs == 0 {
		cfg.MinSilenceDurationMs = DefaultMinSilenceDurationMs
	}
	if cfg.SpeechPadMs == 0 {
		cfg.SpeechPadMs = DefaultSpeechPadMs
	}
	return cfg
}

func validateStreamConfig(cfg StreamConfig) error {
	if cfg.SampleRate != 8000 && cfg.SampleRate != 16000 {
		return fmt.Errorf("vad: sample rate must be 8000 or 16000")
	}
	if cfg.Threshold <= 0 || cfg.Threshold >= 1 {
		return fmt.Errorf("vad: threshold must be in range (0, 1)")
	}
	if cfg.MinSilenceDurationMs < 0 {
		return fmt.Errorf("vad: min silence duration must be non-negative")
	}
	if cfg.SpeechPadMs < 0 {
		return fmt.Errorf("vad: speech pad must be non-negative")
	}
	return nil
}

func chunkSamples(chunk Chunk, defaultSampleRate int) ([]float32, int, error) {
	sampleRate := chunk.SampleRate
	if sampleRate == 0 {
		sampleRate = defaultSampleRate
	}
	if len(chunk.Samples) > 0 {
		return append([]float32(nil), chunk.Samples...), sampleRate, nil
	}
	if len(chunk.PCM16LE)%2 != 0 {
		return nil, 0, fmt.Errorf("vad: pcm16le length must be even")
	}
	samples := make([]float32, len(chunk.PCM16LE)/2)
	for i := range samples {
		raw := int16(binary.LittleEndian.Uint16(chunk.PCM16LE[i*2:]))
		samples[i] = float32(raw) / 32768.0
	}
	return samples, sampleRate, nil
}

func clampNonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func seconds(samples, sampleRate int) float64 {
	return float64(samples) / float64(sampleRate)
}
