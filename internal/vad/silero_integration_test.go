//go:build cgo

package vad

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSileroRuntimeSmoke(t *testing.T) {
	modelPath := os.Getenv("SILERO_VAD_MODEL_PATH")
	if modelPath == "" {
		t.Skip("SILERO_VAD_MODEL_PATH is not set")
	}
	detector, err := NewSileroStreamingDetector(SileroConfig{
		ModelPath:            resolveTestModelPath(t, modelPath),
		SampleRate:           16000,
		Threshold:            0.5,
		MinSilenceDurationMs: 200,
		SpeechPadMs:          30,
	})
	if err != nil {
		t.Fatalf("NewSileroStreamingDetector() error = %v", err)
	}
	defer detector.Close()

	decision, err := detector.Process(context.Background(), Chunk{
		Samples:    make([]float32, 1024),
		SampleRate: 16000,
		Final:      true,
	})
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if decision.SampleRate != 16000 {
		t.Fatalf("SampleRate = %d, want 16000", decision.SampleRate)
	}
}

func resolveTestModelPath(t *testing.T, modelPath string) string {
	t.Helper()
	if filepath.IsAbs(modelPath) {
		return modelPath
	}
	if _, err := os.Stat(modelPath); err == nil {
		return modelPath
	}
	for _, prefix := range []string{"..", "../..", "../../.."} {
		candidate := filepath.Join(prefix, modelPath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return modelPath
}

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working dir: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			t.Fatalf("repo root not found from %s", wd)
		}
	}
}
