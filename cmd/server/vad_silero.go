//go:build cgo

package main

import (
	"fmt"

	"xin-chat-robot-agent/internal/config"
	"xin-chat-robot-agent/internal/vad"
)

func newVADDetector(cfg config.VADConfig) (vad.StreamingDetector, error) {
	switch cfg.Provider {
	case "", "none":
		return nil, nil
	case "silero":
		return vad.NewSileroStreamingDetector(vad.SileroConfig{
			ModelPath:            cfg.SileroModelPath,
			SampleRate:           cfg.SampleRate,
			Threshold:            cfg.Threshold,
			MinSilenceDurationMs: cfg.MinSilenceDurationMs,
			SpeechPadMs:          cfg.SpeechPadMs,
		})
	default:
		return nil, fmt.Errorf("unsupported VAD_PROVIDER %q", cfg.Provider)
	}
}
