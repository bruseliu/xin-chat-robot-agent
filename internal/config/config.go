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
}

type SessionConfig struct {
	PostgresDSN string
	AutoMigrate bool
}

type Mem0Config struct {
	Mode    string
	BaseURL string
	APIKey  string
	TopK    int
	Timeout time.Duration
}

func Load() Config {
	return Config{
		Addr:    envString("APP_ADDR", ":8080"),
		Env:     envString("APP_ENV", "local"),
		AgentID: envString("AGENT_ID", "voice-agent"),
		Session: SessionConfig{
			PostgresDSN: envString("ADK_SESSION_POSTGRES_DSN", ""),
			AutoMigrate: envBool("ADK_SESSION_AUTO_MIGRATE", true),
		},
		Mem0: Mem0Config{
			Mode:    envString("MEM0_MODE", "self_hosted"),
			BaseURL: envString("MEM0_BASE_URL", "http://localhost:8888"),
			APIKey:  envString("MEM0_API_KEY", ""),
			TopK:    envInt("MEM0_TOP_K", 8),
			Timeout: time.Duration(envInt("MEM0_TIMEOUT_SECONDS", 10)) * time.Second,
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
