package vad

import (
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
	MinSpeechDurationMs  int
	SpeechPadMs          int
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
	if cfg.MinSpeechDurationMs == 0 {
		cfg.MinSpeechDurationMs = DefaultMinSpeechDurationMs
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
	if cfg.MinSpeechDurationMs < 0 {
		return fmt.Errorf("vad: min speech duration must be non-negative")
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
