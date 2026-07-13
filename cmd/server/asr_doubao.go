package main

import (
	"context"
	"fmt"
	"log/slog"

	"xin-chat-robot-agent/internal/config"
	"xin-chat-robot-agent/internal/voice"
)

func newASRStream(ctx context.Context, cfg config.ASRConfig, logger *slog.Logger) (voice.StreamingASR, error) {
	switch cfg.Provider {
	case "", "none":
		return nil, nil
	case "funasr":
		return voice.NewFunASRASR(ctx, voice.FunASRASRConfig{
			URL:          cfg.FunASRURL,
			SampleRate:   cfg.FunASRSampleRate,
			DialTimeout:  cfg.FunASRTimeout,
			WriteTimeout: cfg.FunASRTimeout,
			ReadTimeout:  cfg.FunASRTimeout,
		}, logger)
	case "aliyun_nls":
		return voice.NewAliyunNLSASR(ctx, voice.AliyunNLSASRConfig{
			URL:                             cfg.AliyunNLSURL,
			Token:                           cfg.AliyunNLSToken,
			AppKey:                          cfg.AliyunNLSAppKey,
			SampleRate:                      cfg.AliyunNLSSampleRate,
			EnablePunctuation:               cfg.AliyunNLSPunctuation,
			EnableInverseTextNormalization:  cfg.AliyunNLSITN,
			EnableIntermediateResult:        cfg.AliyunNLSIntermediate,
			EnableSemanticSentenceDetection: cfg.AliyunNLSSemanticSentenceDetection,
			MaxSentenceSilenceMs:            cfg.AliyunNLSMaxSentenceSilenceMs,
			DialTimeout:                     cfg.AliyunNLSTimeout,
			WriteTimeout:                    cfg.AliyunNLSTimeout,
			ReadTimeout:                     cfg.AliyunNLSTimeout,
		})
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
