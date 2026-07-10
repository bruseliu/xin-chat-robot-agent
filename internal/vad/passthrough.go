package vad

import "context"

// Passthrough forwards every audio chunk to an upstream service without local VAD.
type Passthrough struct{}

func NewPassthrough() *Passthrough {
	return &Passthrough{}
}

func (*Passthrough) Process(_ context.Context, chunk Chunk) (AudioOutput, error) {
	return AudioOutput{Chunk: chunk}, nil
}

func (*Passthrough) Reset() error { return nil }

func (*Passthrough) Close() error { return nil }
