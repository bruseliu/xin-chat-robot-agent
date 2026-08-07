package main

import (
	"context"
	"fmt"
	"strings"

	"xin-chat-robot-agent/internal/config"
	"xin-chat-robot-agent/internal/tts"
	"xin-chat-robot-agent/internal/voice"
)

func newTTSFactory(cfg config.TTSConfig) (voice.TTSFactory, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "none":
		return nil, nil
	case "doubao":
		return func(ctx context.Context) (tts.Stream, error) {
			stream, err := tts.NewDoubaoStream(ctx, tts.DoubaoConfig{URL: cfg.DoubaoURL, AppID: cfg.DoubaoAppID, AccessToken: cfg.DoubaoAccessToken, ResourceID: cfg.DoubaoResourceID, Speaker: cfg.DoubaoSpeaker, SampleRate: cfg.SampleRate})
			if err != nil {
				return nil, err
			}
			return stream, nil
		}, nil
	default:
		return nil, fmt.Errorf("unsupported TTS_PROVIDER %q", cfg.Provider)
	}
}
