package voice

import "time"

const defaultBargeInDrainTimeout = 500 * time.Millisecond

// BargeInConfig controls whether a new utterance may interrupt the turn that is
// already running.
//
// When disabled the session is half-duplex: a sentence arriving mid-turn is
// dropped and the current reply plays to completion. This is the echo-safe mode
// — with speakers and no acoustic echo cancellation the microphone picks up the
// assistant's own TTS, and an enabled barge-in would let the assistant interrupt
// itself. Enable it only when the input is echo-free (headphones, or AEC applied
// upstream).
type BargeInConfig struct {
	Enabled bool
	// DrainTimeout bounds how long an interrupt waits for the cancelled turn and
	// its TTS forwarder to unwind before giving up and relying on the output
	// epoch to discard whatever they still produce.
	DrainTimeout time.Duration
}

func (c BargeInConfig) normalize() BargeInConfig {
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = defaultBargeInDrainTimeout
	}
	return c
}
