//go:build !cgo

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
		return nil, fmt.Errorf("silero VAD unavailable: build with CGO_ENABLED=1")
	default:
		return nil, fmt.Errorf("unsupported VAD_PROVIDER %q", cfg.Provider)
	}
}
