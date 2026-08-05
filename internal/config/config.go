package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr    string
	Env     string
	AgentID string
	Mem0    Mem0Config
	Session SessionConfig
	LLM     LLMConfig
	VAD     VADConfig
	ASR     ASRConfig
	TTS     TTSConfig
	BargeIn BargeInConfig
}

// BargeInConfig controls whether a new utterance interrupts the reply in
// progress. Requires echo-free input: with speakers and no AEC the microphone
// hears the assistant's own TTS, which would interrupt itself.
type BargeInConfig struct {
	Enabled      bool
	DrainTimeout time.Duration
}

type SessionConfig struct {
	PostgresDSN     string
	AutoMigrate     bool
	MaxTurns        int
	TTL             time.Duration
	CleanupInterval time.Duration
}

type Mem0Config struct {
	Mode         string
	BaseURL      string
	APIKey       string
	TopK         int
	Timeout      time.Duration
	WriteTimeout time.Duration
}

type LLMConfig struct {
	Provider       string
	GeminiAPIKey   string
	GeminiModel    string
	DoubaoBaseURL  string
	DoubaoAPIKey   string
	DoubaoModel    string
	DoubaoTimeout  time.Duration
	DoubaoThinking string
	Instruction    string
}

type TTSConfig struct {
	Provider          string
	DoubaoURL         string
	DoubaoAppID       string
	DoubaoAccessToken string
	DoubaoResourceID  string
	DoubaoSpeaker     string
	SampleRate        int
}

type ASRConfig struct {
	Provider                           string
	FunASRURL                          string
	FunASRSampleRate                   int
	FunASRTimeout                      time.Duration
	AliyunNLSURL                       string
	AliyunNLSToken                     string
	AliyunNLSAppKey                    string
	AliyunNLSSampleRate                int
	AliyunNLSPunctuation               bool
	AliyunNLSITN                       bool
	AliyunNLSIntermediate              bool
	AliyunNLSSemanticSentenceDetection bool
	AliyunNLSMaxSentenceSilenceMs      int
	AliyunNLSTimeout                   time.Duration
	DoubaoURL                          string
	DoubaoAppKey                       string
	DoubaoAccessKey                    string
	DoubaoResourceID                   string
	DoubaoModelName                    string
	DoubaoLanguage                     string
	DoubaoSampleRate                   int
	DoubaoEnablePunc                   bool
	DoubaoCompressAudio                bool
	DoubaoTimeout                      time.Duration
}

type VADConfig struct {
	Provider             string
	SileroModelPath      string
	SampleRate           int
	Threshold            float32
	MinSilenceDurationMs int
	MinSpeechDurationMs  int
	SpeechPadMs          int
	PreRollMs            int
	AudioBufferSeconds   int
	MaxSpeechMs          int
}

func Load() Config {
	return Config{
		Addr:    envString("APP_ADDR", ":8080"),
		Env:     envString("APP_ENV", "local"),
		AgentID: envString("AGENT_ID", "voice-agent"),
		Session: SessionConfig{
			PostgresDSN:     envString("ADK_SESSION_POSTGRES_DSN", ""),
			AutoMigrate:     envBool("ADK_SESSION_AUTO_MIGRATE", true),
			MaxTurns:        envInt("ADK_SESSION_MAX_TURNS", 20),
			TTL:             time.Duration(envInt("ADK_SESSION_TTL_HOURS", 168)) * time.Hour,
			CleanupInterval: time.Duration(envInt("ADK_SESSION_CLEANUP_INTERVAL_MINUTES", 60)) * time.Minute,
		},
		Mem0: Mem0Config{
			Mode:    envString("MEM0_MODE", "self_hosted"),
			BaseURL: envString("MEM0_BASE_URL", "http://localhost:8888"),
			APIKey:  envString("MEM0_API_KEY", ""),
			TopK:    envInt("MEM0_TOP_K", 8),
			Timeout: time.Duration(envInt("MEM0_TIMEOUT_SECONDS", 10)) * time.Second,
			// Writes are asynchronous; mem0 spends seconds of LLM time per write
			// reconciling facts against the existing store, so allow much longer.
			WriteTimeout: time.Duration(envInt("MEM0_WRITE_TIMEOUT_SECONDS", 60)) * time.Second,
		},
		LLM: LLMConfig{
			Provider:      envString("LLM_PROVIDER", "none"),
			GeminiAPIKey:  envString("GEMINI_API_KEY", envString("GOOGLE_API_KEY", "")),
			GeminiModel:   envString("GEMINI_MODEL", "gemini-flash-latest"),
			DoubaoBaseURL: envString("DOUBAO_LLM_BASE_URL", "https://ark.cn-beijing.volces.com/api/v3"),
			DoubaoAPIKey:  envString("DOUBAO_LLM_API_KEY", envString("ARK_API_KEY", "")),
			DoubaoModel:   envString("DOUBAO_LLM_MODEL", ""),
			DoubaoTimeout: time.Duration(envInt("DOUBAO_LLM_TIMEOUT_SECONDS", 60)) * time.Second,
			// Reasoning models emit thinking tokens before any answer content,
			// costing seconds of first-token latency. Voice needs it off.
			DoubaoThinking: envString("DOUBAO_LLM_THINKING", "disabled"),
			Instruction:    envString("AGENT_INSTRUCTION", "你是一个实时语音助手。请用自然、简洁的中文回答，适合语音朗读，不要使用 Markdown 格式。"),
		},
		VAD: VADConfig{
			Provider:             envString("VAD_PROVIDER", "none"),
			SileroModelPath:      envString("SILERO_VAD_MODEL_PATH", ""),
			SampleRate:           envInt("VAD_SAMPLE_RATE", 16000),
			Threshold:            envFloat32("VAD_THRESHOLD", 0.5),
			MinSilenceDurationMs: envInt("VAD_MIN_SILENCE_MS", 200),
			MinSpeechDurationMs:  envInt("VAD_MIN_SPEECH_MS", 250),
			SpeechPadMs:          envInt("VAD_SPEECH_PAD_MS", 30),
			PreRollMs:            envInt("VAD_PRE_ROLL_MS", 800),
			AudioBufferSeconds:   envInt("VAD_AUDIO_BUFFER_SECONDS", 5),
			MaxSpeechMs:          envInt("VAD_MAX_SPEECH_MS", 15000),
		},
		TTS: TTSConfig{Provider: envString("TTS_PROVIDER", "none"), DoubaoURL: envString("DOUBAO_TTS_URL", "wss://openspeech.bytedance.com/api/v3/tts/bidirection"), DoubaoAppID: envString("DOUBAO_TTS_APP_ID", ""), DoubaoAccessToken: envString("DOUBAO_TTS_ACCESS_TOKEN", ""), DoubaoResourceID: envString("DOUBAO_TTS_RESOURCE_ID", "volc.service_type.10029"), DoubaoSpeaker: envString("DOUBAO_TTS_SPEAKER", ""), SampleRate: envInt("DOUBAO_TTS_SAMPLE_RATE", 24000)},
		BargeIn: BargeInConfig{
			Enabled:      envBool("BARGE_IN_ENABLED", true),
			DrainTimeout: time.Duration(envInt("BARGE_IN_DRAIN_TIMEOUT_MS", 500)) * time.Millisecond,
		},
		ASR: ASRConfig{
			Provider:                           envString("ASR_PROVIDER", "none"),
			FunASRURL:                          envString("FUNASR_URL", "ws://127.0.0.1:10095"),
			FunASRSampleRate:                   envInt("FUNASR_SAMPLE_RATE", 16000),
			FunASRTimeout:                      time.Duration(envInt("FUNASR_TIMEOUT_SECONDS", 30)) * time.Second,
			AliyunNLSURL:                       envString("ALIYUN_NLS_URL", "wss://nls-gateway.cn-shanghai.aliyuncs.com/ws/v1"),
			AliyunNLSToken:                     envString("ALIYUN_NLS_TOKEN", ""),
			AliyunNLSAppKey:                    envString("ALIYUN_NLS_APP_KEY", ""),
			AliyunNLSSampleRate:                envInt("ALIYUN_NLS_SAMPLE_RATE", 16000),
			AliyunNLSPunctuation:               envBool("ALIYUN_NLS_ENABLE_PUNCTUATION", true),
			AliyunNLSITN:                       envBool("ALIYUN_NLS_ENABLE_ITN", true),
			AliyunNLSIntermediate:              envBool("ALIYUN_NLS_ENABLE_INTERMEDIATE", true),
			AliyunNLSSemanticSentenceDetection: envBool("ALIYUN_NLS_ENABLE_SEMANTIC_SENTENCE_DETECTION", true),
			AliyunNLSMaxSentenceSilenceMs:      envInt("ALIYUN_NLS_MAX_SENTENCE_SILENCE_MS", 800),
			AliyunNLSTimeout:                   time.Duration(envInt("ALIYUN_NLS_TIMEOUT_SECONDS", 30)) * time.Second,
			DoubaoURL:                          envString("DOUBAO_ASR_URL", "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel"),
			DoubaoAppKey:                       envString("DOUBAO_ASR_APP_KEY", ""),
			DoubaoAccessKey:                    envString("DOUBAO_ASR_ACCESS_KEY", ""),
			DoubaoResourceID:                   envString("DOUBAO_ASR_RESOURCE_ID", "volc.bigasr.sauc.duration"),
			DoubaoModelName:                    envString("DOUBAO_ASR_MODEL_NAME", "bigmodel"),
			DoubaoLanguage:                     envString("DOUBAO_ASR_LANGUAGE", "zh-CN"),
			DoubaoSampleRate:                   envInt("DOUBAO_ASR_SAMPLE_RATE", 16000),
			DoubaoEnablePunc:                   envBool("DOUBAO_ASR_ENABLE_PUNC", true),
			DoubaoCompressAudio:                envBool("DOUBAO_ASR_COMPRESS_AUDIO", true),
			DoubaoTimeout:                      time.Duration(envInt("DOUBAO_ASR_TIMEOUT_SECONDS", 30)) * time.Second,
		},
	}
}

func envString(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envFloat32(key string, fallback float32) float32 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 32)
	if err != nil {
		return fallback
	}
	return float32(value)
}
