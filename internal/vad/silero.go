//go:build cgo

package vad

import (
	"context"
	"fmt"

	"github.com/streamer45/silero-vad-go/speech"
)

type SileroConfig struct {
	ModelPath            string
	SampleRate           int
	Threshold            float32
	MinSilenceDurationMs int
	MinSpeechDurationMs  int
	SpeechPadMs          int
	LogLevel             int
	PreRollMs            int
	AudioBufferSeconds   int
	MaxSpeechMs          int
}

type SileroStreamingDetector struct {
	detector *speech.Detector
	gate     *AudioGate
}

func NewSileroStreamingDetector(cfg SileroConfig) (*SileroStreamingDetector, error) {
	streamCfg := normalizeStreamConfig(StreamConfig{
		SampleRate:           cfg.SampleRate,
		Threshold:            cfg.Threshold,
		MinSilenceDurationMs: cfg.MinSilenceDurationMs,
		MinSpeechDurationMs:  cfg.MinSpeechDurationMs,
		SpeechPadMs:          cfg.SpeechPadMs,
	})
	if err := validateStreamConfig(streamCfg); err != nil {
		return nil, err
	}
	detector, err := speech.NewDetector(speech.DetectorConfig{
		ModelPath:            cfg.ModelPath,
		SampleRate:           streamCfg.SampleRate,
		Threshold:            streamCfg.Threshold,
		MinSilenceDurationMs: streamCfg.MinSilenceDurationMs,
		SpeechPadMs:          streamCfg.SpeechPadMs,
		LogLevel:             speech.LogLevel(cfg.LogLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("create silero vad detector: %w", err)
	}
	gate, err := NewAudioGate(sileroInferer{detector: detector}, streamCfg, AudioGateConfig{
		PreRollMs:          cfg.PreRollMs,
		AudioBufferSeconds: cfg.AudioBufferSeconds,
		MaxSpeechMs:        cfg.MaxSpeechMs,
	})
	if err != nil {
		_ = detector.Destroy()
		return nil, err
	}
	return &SileroStreamingDetector{
		detector: detector,
		gate:     gate,
	}, nil
}

func (d *SileroStreamingDetector) Process(ctx context.Context, chunk Chunk) (AudioOutput, error) {
	if d == nil || d.gate == nil {
		return AudioOutput{}, fmt.Errorf("vad: nil silero detector")
	}
	return d.gate.Process(ctx, chunk)
}

func (d *SileroStreamingDetector) Reset() error {
	if d == nil || d.gate == nil {
		return fmt.Errorf("vad: nil silero detector")
	}
	return d.gate.Reset()
}

func (d *SileroStreamingDetector) Close() error {
	if d == nil || d.detector == nil {
		return nil
	}
	return d.detector.Destroy()
}

type sileroInferer struct {
	detector *speech.Detector
}

func (i sileroInferer) Infer(samples []float32) (float32, error) {
	if i.detector == nil {
		return 0, fmt.Errorf("vad: nil silero inferer")
	}
	return i.detector.Infer(samples)
}

func (i sileroInferer) Reset() error {
	if i.detector == nil {
		return fmt.Errorf("vad: nil silero inferer")
	}
	return i.detector.Reset()
}
