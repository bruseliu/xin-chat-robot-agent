package vad

import (
	"context"
	"errors"
	"testing"
)

func TestAudioGateBuffersUntilWindow(t *testing.T) {
	inferer := &scriptedInferer{probs: []float32{0.8}}
	gate, err := NewAudioGate(inferer, StreamConfig{SampleRate: 16000, Threshold: 0.5}, AudioGateConfig{})
	if err != nil {
		t.Fatalf("NewAudioGate() error = %v", err)
	}
	out, err := gate.Process(context.Background(), Chunk{Samples: make([]float32, 511), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if inferer.calls != 0 {
		t.Fatalf("Infer calls = %d, want 0", inferer.calls)
	}
	if len(out.Chunk.Samples) != 0 || out.VADEnd {
		t.Fatalf("output = %+v, want no audio and no end", out)
	}
}

func TestAudioGateReturnsPreRollOnSpeechStart(t *testing.T) {
	inferer := &scriptedInferer{probs: scriptedProbs(27, 0, 0.8, 0.8)}
	gate, err := NewAudioGate(inferer, StreamConfig{
		SampleRate:          16000,
		Threshold:           0.5,
		MinSpeechDurationMs: 32,
		SpeechPadMs:         0,
	}, AudioGateConfig{})
	if err != nil {
		t.Fatalf("NewAudioGate() error = %v", err)
	}

	silence := Chunk{Samples: makeSequence(27*512, 0), SampleRate: 16000}
	out, err := gate.Process(context.Background(), silence)
	if err != nil {
		t.Fatalf("Process(silence) error = %v", err)
	}
	if len(out.Chunk.Samples) != 0 || out.VADEnd {
		t.Fatalf("silence output = %+v, want no audio and no end", out)
	}

	speech := Chunk{Samples: makeSequence(1024, 20000), SampleRate: 16000}
	out, err = gate.Process(context.Background(), speech)
	if err != nil {
		t.Fatalf("Process(speech) error = %v", err)
	}
	if got, want := len(out.Chunk.Samples), 12800; got != want {
		t.Fatalf("output samples = %d, want %d", got, want)
	}
	if out.Chunk.Samples[0] != 2048 {
		t.Fatalf("first pre-roll sample = %v, want 2048", out.Chunk.Samples[0])
	}
	if out.Chunk.Samples[11776] != 20000 {
		t.Fatalf("first speech sample = %v, want 20000", out.Chunk.Samples[11776])
	}
	if out.VADEnd {
		t.Fatal("VADEnd = true, want false on speech start")
	}
}

func TestAudioGateMarksEndAndForwardsCurrentWindow(t *testing.T) {
	inferer := &scriptedInferer{probs: []float32{0.8, 0.8, 0.1, 0.1}}
	gate, err := NewAudioGate(inferer, StreamConfig{
		SampleRate:           16000,
		Threshold:            0.5,
		MinSpeechDurationMs:  32,
		MinSilenceDurationMs: 32,
		SpeechPadMs:          0,
	}, AudioGateConfig{})
	if err != nil {
		t.Fatalf("NewAudioGate() error = %v", err)
	}

	if _, err := gate.Process(context.Background(), Chunk{Samples: makeSequence(1024, 1000), SampleRate: 16000}); err != nil {
		t.Fatalf("Process(start) error = %v", err)
	}
	out, err := gate.Process(context.Background(), Chunk{Samples: makeSequence(1024, 5000), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process(end) error = %v", err)
	}
	if !out.VADEnd {
		t.Fatal("VADEnd = false, want true")
	}
	if got, want := len(out.Chunk.Samples), 1024; got != want {
		t.Fatalf("output samples = %d, want %d", got, want)
	}
}

func TestAudioGateForceCutsContinuousSpeech(t *testing.T) {
	inferer := &scriptedInferer{probs: scriptedProbs(20, 0.8)}
	gate, err := NewAudioGate(inferer, StreamConfig{
		SampleRate:          16000,
		Threshold:           0.5,
		MinSpeechDurationMs: 32,
	}, AudioGateConfig{
		PreRollMs:          0,
		AudioBufferSeconds: 1,
		MaxSpeechMs:        96,
	})
	if err != nil {
		t.Fatalf("NewAudioGate() error = %v", err)
	}

	out, err := gate.Process(context.Background(), Chunk{Samples: makeSequence(4*512, 1000), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if !out.VADEnd {
		t.Fatal("VADEnd = false, want true after max speech duration")
	}
	if len(out.Chunk.Samples) == 0 {
		t.Fatal("output samples = 0, want forced-cut speech audio")
	}
}

func TestAudioGateForceCutBoundsNextPreRoll(t *testing.T) {
	inferer := &scriptedInferer{probs: scriptedProbs(20, 0.8)}
	gate, err := NewAudioGate(inferer, StreamConfig{
		SampleRate:          16000,
		Threshold:           0.5,
		MinSpeechDurationMs: 32,
	}, AudioGateConfig{
		PreRollMs:          64,
		AudioBufferSeconds: 1,
		MaxSpeechMs:        96,
	})
	if err != nil {
		t.Fatalf("NewAudioGate() error = %v", err)
	}

	out, err := gate.Process(context.Background(), Chunk{Samples: makeSequence(4*512, 1000), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process(first segment) error = %v", err)
	}
	if !out.VADEnd {
		t.Fatal("first segment VADEnd = false, want forced cut")
	}

	out, err = gate.Process(context.Background(), Chunk{Samples: makeSequence(2*512, 4000), SampleRate: 16000})
	if err != nil {
		t.Fatalf("Process(second segment) error = %v", err)
	}
	if out.VADEnd {
		t.Fatal("second segment VADEnd = true, want active segment without forced cut yet")
	}
	if got, want := len(out.Chunk.Samples), 1024; got != want {
		t.Fatalf("second segment samples = %d, want %d", got, want)
	}
	if out.Chunk.Samples[0] != 4000 {
		t.Fatalf("second segment first sample = %v, want 4000 (no pre-cut pre-roll duplicate)", out.Chunk.Samples[0])
	}
}

func TestAudioGatePropagatesInferError(t *testing.T) {
	wantErr := errors.New("infer failed")
	gate, err := NewAudioGate(&scriptedInferer{err: wantErr}, StreamConfig{}, AudioGateConfig{})
	if err != nil {
		t.Fatalf("NewAudioGate() error = %v", err)
	}
	_, err = gate.Process(context.Background(), Chunk{Samples: make([]float32, 512), SampleRate: 16000})
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

func scriptedProbs(n int, prob float32, tail ...float32) []float32 {
	probs := make([]float32, 0, n+len(tail))
	for i := 0; i < n; i++ {
		probs = append(probs, prob)
	}
	return append(probs, tail...)
}

func makeSequence(n int, offset float32) []float32 {
	samples := make([]float32, n)
	for i := range samples {
		samples[i] = offset + float32(i)
	}
	return samples
}
