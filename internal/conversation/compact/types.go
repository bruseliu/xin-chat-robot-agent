package compact

import "time"

const (
	defaultTriggerTurns = 20
	defaultKeepTurns    = 8
	defaultQueueSize    = 256
	defaultWorkers      = 2
	defaultTimeout      = 30 * time.Second
)

// Config bounds all asynchronous work performed by Worker.
type Config struct {
	TriggerTurns int
	KeepTurns    int
	QueueSize    int
	Workers      int
	Timeout      time.Duration
	RetryDelay   time.Duration
	MaxRetry     time.Duration
}

func (config Config) withDefaults() Config {
	if config.TriggerTurns <= 0 {
		config.TriggerTurns = defaultTriggerTurns
	}
	if config.KeepTurns < 0 {
		config.KeepTurns = defaultKeepTurns
	}
	if config.KeepTurns == 0 {
		config.KeepTurns = defaultKeepTurns
	}
	if config.QueueSize <= 0 {
		config.QueueSize = defaultQueueSize
	}
	if config.Workers <= 0 {
		config.Workers = defaultWorkers
	}
	if config.Timeout <= 0 {
		config.Timeout = defaultTimeout
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = time.Second
	}
	if config.MaxRetry <= 0 {
		config.MaxRetry = time.Minute
	}
	return config
}
