//go:build cgo

package voice

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xin-chat-robot-agent/internal/vad"
)

func TestSileroVADToDoubaoASRRealService(t *testing.T) {
	if os.Getenv("VAD_ASR_REAL_TEST") != "1" {
		t.Skip("set VAD_ASR_REAL_TEST=1 to run real Silero VAD + Doubao ASR integration test")
	}
	modelPath := os.Getenv("SILERO_VAD_MODEL_PATH")
	if modelPath == "" {
		t.Skip("SILERO_VAD_MODEL_PATH is required")
	}
	appKey := os.Getenv("DOUBAO_ASR_APP_KEY")
	accessKey := os.Getenv("DOUBAO_ASR_ACCESS_KEY")
	if appKey == "" || accessKey == "" {
		t.Skip("DOUBAO_ASR_APP_KEY and DOUBAO_ASR_ACCESS_KEY are required")
	}

	wavPath := os.Getenv("VAD_ASR_TEST_WAV")
	if wavPath == "" {
		wavPath = filepath.FromSlash("../vad/testdata/test2.wav")
	}
	pcm, sampleRate := readPCM16MonoWAVForASR(t, wavPath)

	ctx, cancel := context.WithTimeout(context.Background(), envDuration("VAD_ASR_REAL_TIMEOUT", 60*time.Second))
	defer cancel()

	detector, err := vad.NewSileroStreamingDetector(vad.SileroConfig{
		ModelPath:            resolveRepoRelativePathForVoiceTest(t, modelPath),
		SampleRate:           sampleRate,
		Threshold:            0.5,
		MinSilenceDurationMs: 200,
		MinSpeechDurationMs:  250,
		SpeechPadMs:          30,
		PreRollMs:            800,
		AudioBufferSeconds:   5,
		MaxSpeechMs:          15000,
	})
	if err != nil {
		t.Fatalf("NewSileroStreamingDetector() error = %v", err)
	}
	defer detector.Close()

	stream, err := NewDoubaoASR(ctx, DoubaoASRConfig{
		URL:           os.Getenv("DOUBAO_ASR_URL"),
		AppKey:        appKey,
		AccessKey:     accessKey,
		ResourceID:    os.Getenv("DOUBAO_ASR_RESOURCE_ID"),
		ModelName:     os.Getenv("DOUBAO_ASR_MODEL_NAME"),
		Language:      os.Getenv("DOUBAO_ASR_LANGUAGE"),
		SampleRate:    sampleRate,
		EnablePunc:    envBool("DOUBAO_ASR_ENABLE_PUNC", true),
		CompressAudio: envBool("DOUBAO_ASR_COMPRESS_AUDIO", true),
		DialTimeout:   10 * time.Second,
		WriteTimeout:  10 * time.Second,
		ReadTimeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDoubaoASR() error = %v", err)
	}
	defer stream.Close()
	go logStreamingTranscripts(t, stream.Transcripts())

	chunkDuration := envDuration("VAD_ASR_REAL_CHUNK_DURATION", 200*time.Millisecond)
	chunkBytes := chunkBytesForDuration(chunkDuration, sampleRate)
	if chunkBytes <= 0 {
		chunkBytes = sampleRate / 5 * 2
	}
	paceAudio := envBool("VAD_ASR_REAL_PACE", true)

	var vadOutputSamples int
	var asrWrites int
	var vadEnds int
	for offset := 0; offset < len(pcm); offset += chunkBytes {
		end := offset + chunkBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		vadOut, err := detector.Process(ctx, vad.Chunk{
			PCM16LE:    pcm[offset:end],
			SampleRate: sampleRate,
			Final:      end == len(pcm),
		})
		if err != nil {
			t.Fatalf("VAD Process(offset=%d) error = %v", offset, err)
		}
		if len(vadOut.Chunk.Samples) > 0 || len(vadOut.Chunk.PCM16LE) > 0 {
			vadOutputSamples += len(vadOut.Chunk.Samples) + len(vadOut.Chunk.PCM16LE)/2
			asrWrites++
			if err := stream.Write(ctx, vadOut.Chunk); err != nil {
				t.Fatalf("ASR Write(offset=%d) error = %v", offset, err)
			}
		}
		if vadOut.VADEnd {
			vadEnds++
		}
		if paceAudio && end < len(pcm) {
			select {
			case <-ctx.Done():
				t.Fatalf("context done while pacing audio: %v", ctx.Err())
			case <-time.After(chunkDuration):
			}
		}
	}
	if vadEnds == 0 {
		t.Fatalf("VAD never emitted VADEnd; vad_output_samples=%d asr_writes=%d", vadOutputSamples, asrWrites)
	}
	if asrWrites == 0 {
		t.Fatalf("VAD produced no ASR audio; vad_ends=%d", vadEnds)
	}
	transcript, err := stream.Finalize(ctx)
	if err != nil {
		t.Fatalf("ASR Finalize() after whole audio error = %v", err)
	}
	if transcript.Text == "" {
		t.Fatalf("empty transcript from VAD-gated real Doubao ASR; vad_output_samples=%d asr_writes=%d vad_ends=%d", vadOutputSamples, asrWrites, vadEnds)
	}
	t.Logf("VAD+ASR transcript: %q; vad_output_samples=%d asr_writes=%d vad_ends=%d", transcript.Text, vadOutputSamples, asrWrites, vadEnds)
}

func resolveRepoRelativePathForVoiceTest(t *testing.T, path string) string {
	t.Helper()
	if filepath.IsAbs(path) {
		return path
	}
	if _, err := os.Stat(path); err == nil {
		return path
	}
	for _, prefix := range []string{"..", "../..", "../../.."} {
		candidate := filepath.Join(prefix, path)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return path
}
