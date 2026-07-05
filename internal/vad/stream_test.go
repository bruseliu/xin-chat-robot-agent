package vad

import (
	"context"
	"errors"
	"testing"
)

func TestStreamBuffersUntilWindow(t *testing.T) {
	inferer := &scriptedInferer{probs: []float32{0.8}}
	stream, err := NewStream(StreamConfig{SampleRate: 16000, Threshold: 0.5}, inferer)
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}

	decision, err := stream.Process(context.Background(), Chunk{Samples: make([]float32, stream.WindowSize()-1), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if inferer.calls != 0 {
		t.Fatalf("Infer calls = %d, want 0", inferer.calls)
	}
	if decision.Buffered != stream.WindowSize()-1 {
		t.Fatalf("Buffered = %d, want %d", decision.Buffered, stream.WindowSize()-1)
	}
}

func TestStreamDetectsSpeechStartAndEndAcrossChunks(t *testing.T) {
	inferer := &scriptedInferer{probs: []float32{0.8, 0.1, 0.1}}
	stream, err := NewStream(StreamConfig{
		SampleRate:           16000,
		Threshold:            0.5,
		MinSilenceDurationMs: 32,
		SpeechPadMs:          0,
	}, inferer)
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}

	start, err := stream.Process(context.Background(), Chunk{Samples: make([]float32, stream.WindowSize()), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process(start) error = %v", err)
	}
	if !start.SpeechStarted || !start.SpeechActive || len(start.Segments) != 1 {
		t.Fatalf("start decision = %+v, want speech start", start)
	}

	middle, err := stream.Process(context.Background(), Chunk{Samples: make([]float32, stream.WindowSize()), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process(middle) error = %v", err)
	}
	if middle.SpeechEnded || !middle.SpeechActive {
		t.Fatalf("middle decision = %+v, want active speech without end", middle)
	}

	end, err := stream.Process(context.Background(), Chunk{Samples: make([]float32, stream.WindowSize()), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process(end) error = %v", err)
	}
	if !end.SpeechEnded || end.SpeechActive || len(end.Segments) != 1 || end.Segments[0].SpeechEndAt == 0 {
		t.Fatalf("end decision = %+v, want speech end segment", end)
	}
}

func TestStreamRejectsOddPCM16(t *testing.T) {
	stream, err := NewStream(StreamConfig{}, &scriptedInferer{})
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}
	_, err = stream.Process(context.Background(), Chunk{PCM16LE: []byte{1}, SampleRate: 16000})
	if err == nil {
		t.Fatal("Process() error = nil, want odd pcm16 error")
	}
}

func TestStreamPropagatesInferError(t *testing.T) {
	wantErr := errors.New("infer failed")
	stream, err := NewStream(StreamConfig{}, &scriptedInferer{err: wantErr})
	if err != nil {
		t.Fatalf("NewStream() error = %v", err)
	}
	_, err = stream.Process(context.Background(), Chunk{Samples: make([]float32, stream.WindowSize()), SampleRate: 16000})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Process() error = %v, want %v", err, wantErr)
	}
}

type scriptedInferer struct {
	probs []float32
	err   error
	calls int
}

func (i *scriptedInferer) Infer([]float32) (float32, error) {
	i.calls++
	if i.err != nil {
		return 0, i.err
	}
	if len(i.probs) == 0 {
		return 0, nil
	}
	prob := i.probs[0]
	i.probs = i.probs[1:]
	return prob, nil
}

func (i *scriptedInferer) Reset() error {
	i.calls = 0
	return nil
}
