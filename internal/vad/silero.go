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
	SpeechPadMs          int
	LogLevel             int
}

type SileroStreamingDetector struct {
	detector *speech.Detector
	stream   *Stream
}

func NewSileroStreamingDetector(cfg SileroConfig) (*SileroStreamingDetector, error) {
	streamCfg := normalizeStreamConfig(StreamConfig{
		SampleRate:           cfg.SampleRate,
		Threshold:            cfg.Threshold,
		MinSilenceDurationMs: cfg.MinSilenceDurationMs,
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
	stream, err := NewStream(streamCfg, sileroInferer{detector: detector})
	if err != nil {
		_ = detector.Destroy()
		return nil, err
	}
	return &SileroStreamingDetector{
		detector: detector,
		stream:   stream,
	}, nil
}

func (d *SileroStreamingDetector) Process(ctx context.Context, chunk Chunk) (Decision, error) {
	if d == nil || d.stream == nil {
		return Decision{}, fmt.Errorf("vad: nil silero detector")
	}
	return d.stream.Process(ctx, chunk)
}

func (d *SileroStreamingDetector) Reset() error {
	if d == nil || d.stream == nil {
		return fmt.Errorf("vad: nil silero detector")
	}
	return d.stream.Reset()
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
