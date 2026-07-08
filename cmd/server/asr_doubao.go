package main

import (
	"context"
	"fmt"

	"xin-chat-robot-agent/internal/config"
	"xin-chat-robot-agent/internal/voice"
)

func newASRStream(ctx context.Context, cfg config.ASRConfig) (voice.StreamingASR, error) {
	switch cfg.Provider {
	case "", "none":
		return nil, nil
	case "doubao":
		return voice.NewDoubaoASR(ctx, voice.DoubaoASRConfig{
			URL:           cfg.DoubaoURL,
			AppKey:        cfg.DoubaoAppKey,
			AccessKey:     cfg.DoubaoAccessKey,
			ResourceID:    cfg.DoubaoResourceID,
			ModelName:     cfg.DoubaoModelName,
			Language:      cfg.DoubaoLanguage,
			SampleRate:    cfg.DoubaoSampleRate,
			EnablePunc:    cfg.DoubaoEnablePunc,
			CompressAudio: cfg.DoubaoCompressAudio,
			DialTimeout:   cfg.DoubaoTimeout,
			WriteTimeout:  cfg.DoubaoTimeout,
			ReadTimeout:   cfg.DoubaoTimeout,
		})
	default:
		return nil, fmt.Errorf("unsupported ASR_PROVIDER %q", cfg.Provider)
	}
}
