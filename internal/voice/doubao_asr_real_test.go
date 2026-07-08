package voice

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"xin-chat-robot-agent/internal/vad"
)

func TestDoubaoASRRealService(t *testing.T) {
	if os.Getenv("DOUBAO_ASR_REAL_TEST") != "1" {
		t.Skip("set DOUBAO_ASR_REAL_TEST=1 to run real Doubao ASR integration test")
	}
	appKey := os.Getenv("DOUBAO_ASR_APP_KEY")
	accessKey := os.Getenv("DOUBAO_ASR_ACCESS_KEY")
	if appKey == "" || accessKey == "" {
		t.Skip("DOUBAO_ASR_APP_KEY and DOUBAO_ASR_ACCESS_KEY are required")
	}

	wavPath := os.Getenv("DOUBAO_ASR_TEST_WAV")
	if wavPath == "" {
		wavPath = filepath.FromSlash("../vad/testdata/test.wav")
	}
	pcm, sampleRate := readPCM16MonoWAVForASR(t, wavPath)

	ctx, cancel := context.WithTimeout(context.Background(), envDuration("DOUBAO_ASR_REAL_TIMEOUT", 45*time.Second))
	defer cancel()

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

	chunkDuration := envDuration("DOUBAO_ASR_REAL_CHUNK_DURATION", 200*time.Millisecond)
	chunkBytes := chunkBytesForDuration(chunkDuration, sampleRate)
	if chunkBytes <= 0 {
		chunkBytes = sampleRate / 5 * 2
	}
	paceAudio := envBool("DOUBAO_ASR_REAL_PACE", true)
	for offset := 0; offset < len(pcm); offset += chunkBytes {
		end := offset + chunkBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := stream.Write(ctx, vad.Chunk{PCM16LE: pcm[offset:end], SampleRate: sampleRate}); err != nil {
			t.Fatalf("Write(offset=%d) error = %v", offset, err)
		}
		if paceAudio && end < len(pcm) {
			select {
			case <-ctx.Done():
				t.Fatalf("context done while pacing audio: %v", ctx.Err())
			case <-time.After(chunkDuration):
			}
		}
	}

	transcript, err := stream.Finalize(ctx)
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if transcript.Text == "" {
		t.Fatalf("empty transcript from real Doubao ASR")
	}
	t.Logf("real Doubao ASR transcript: %q", transcript.Text)
}

func readPCM16MonoWAVForASR(t *testing.T, path string) ([]byte, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open wav %s: %v", path, err)
	}
	defer f.Close()

	header := make([]byte, 12)
	if _, err := io.ReadFull(f, header); err != nil {
		t.Fatalf("read wav header: %v", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		t.Fatalf("%s is not a RIFF/WAVE file", path)
	}

	var sampleRate int
	var pcm []byte
	for {
		chunkHeader := make([]byte, 8)
		_, err := io.ReadFull(f, chunkHeader)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			t.Fatalf("read chunk header: %v", err)
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := int(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		chunk := make([]byte, chunkSize)
		if _, err := io.ReadFull(f, chunk); err != nil {
			t.Fatalf("read %s chunk: %v", chunkID, err)
		}
		if chunkSize%2 == 1 {
			if _, err := f.Seek(1, io.SeekCurrent); err != nil {
				t.Fatalf("skip chunk padding: %v", err)
			}
		}
		switch chunkID {
		case "fmt ":
			format := int(binary.LittleEndian.Uint16(chunk[0:2]))
			channels := int(binary.LittleEndian.Uint16(chunk[2:4]))
			rate := int(binary.LittleEndian.Uint32(chunk[4:8]))
			bits := int(binary.LittleEndian.Uint16(chunk[14:16]))
			if format != 1 || channels != 1 || bits != 16 {
				t.Fatalf("unsupported WAV format: format=%d channels=%d bits=%d", format, channels, bits)
			}
			sampleRate = rate
		case "data":
			pcm = chunk
		}
	}
	if sampleRate == 0 {
		t.Fatal("missing fmt chunk")
	}
	if len(pcm) == 0 {
		t.Fatal("missing data chunk")
	}
	return pcm, sampleRate
}

func envBool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}

func chunkBytesForDuration(duration time.Duration, sampleRate int) int {
	return int(float64(sampleRate) * duration.Seconds() * 2)
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return d
}

func logStreamingTranscripts(t *testing.T, transcripts <-chan Transcript) {
	for transcript := range transcripts {
		t.Logf("streaming ASR transcript: %q final=%v confidence=%.3f", transcript.Text, transcript.IsFinal, transcript.Confidence)
	}
}
