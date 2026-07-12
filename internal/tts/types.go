package tts

import "context"

type AudioChunk struct {
	PCM   []byte
	Final bool
}

type Stream interface {
	WriteText(ctx context.Context, text string) error
	Finish(ctx context.Context) error
	Cancel(ctx context.Context) error
	Audio() <-chan AudioChunk
	Errors() <-chan error
	SampleRate() int
	Close() error
}
