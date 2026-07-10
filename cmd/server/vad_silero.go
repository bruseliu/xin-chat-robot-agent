//go:build cgo

package main

import (
	"fmt"

	"xin-chat-robot-agent/internal/config"
	"xin-chat-robot-agent/internal/vad"
)

func newVADDetector(cfg config.VADConfig) (vad.AudioProcessor, error) {
	switch cfg.Provider {
	case "", "none":
		return nil, nil
	case "passthrough":
		return vad.NewPassthrough(), nil
	case "silero":
		return vad.NewSileroStreamingDetector(vad.SileroConfig{
			ModelPath:            cfg.SileroModelPath,
			SampleRate:           cfg.SampleRate,
			Threshold:            cfg.Threshold,
			MinSilenceDurationMs: cfg.MinSilenceDurationMs,
			MinSpeechDurationMs:  cfg.MinSpeechDurationMs,
			SpeechPadMs:          cfg.SpeechPadMs,
			PreRollMs:            cfg.PreRollMs,
			AudioBufferSeconds:   cfg.AudioBufferSeconds,
			MaxSpeechMs:          cfg.MaxSpeechMs,
		})
	default:
		return nil, fmt.Errorf("unsupported VAD_PROVIDER %q", cfg.Provider)
	}
}
