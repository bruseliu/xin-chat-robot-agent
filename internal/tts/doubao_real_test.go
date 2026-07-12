package tts

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestDoubaoStreamRealService(t *testing.T) {
	if os.Getenv("DOUBAO_TTS_REAL_TEST") != "1" {
		t.Skip("set DOUBAO_TTS_REAL_TEST=1 to run real Doubao TTS integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sampleRate := envIntForTest("DOUBAO_TTS_SAMPLE_RATE", 24000)
	stream, err := NewDoubaoStream(ctx, DoubaoConfig{
		URL: os.Getenv("DOUBAO_TTS_URL"), AppID: os.Getenv("DOUBAO_TTS_APP_ID"),
		AccessToken: os.Getenv("DOUBAO_TTS_ACCESS_TOKEN"), ResourceID: os.Getenv("DOUBAO_TTS_RESOURCE_ID"),
		Speaker: os.Getenv("DOUBAO_TTS_SPEAKER"), SampleRate: sampleRate, Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDoubaoStream: %v", err)
	}
	defer stream.Close()

	// Consume audio before writing text to exercise full-duplex behavior.
	audioResult := make(chan []byte, 1)
	audioErr := make(chan error, 1)
	go func() {
		var pcm []byte
		for {
			select {
			case chunk, ok := <-stream.Audio():
				if !ok || chunk.Final {
					audioResult <- pcm
					return
				}
				pcm = append(pcm, chunk.PCM...)
			case err, ok := <-stream.Errors():
				if ok && err != nil {
					audioErr <- err
					return
				}
			case <-ctx.Done():
				audioErr <- ctx.Err()
				return
			}
		}
	}()

	for _, text := range []string{"你好，", "这是豆包双向流式", "语音合成测试。"} {
		if err := stream.WriteText(ctx, text); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if err := stream.Finish(ctx); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	select {
	case pcm := <-audioResult:
		if len(pcm) == 0 {
			t.Fatal("empty TTS audio")
		}
		output := os.Getenv("DOUBAO_TTS_TEST_OUTPUT")
		if output == "" {
			output = filepath.Join("..", "..", "tmp", "doubao_tts_real_test.wav")
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			t.Fatalf("create output directory: %v", err)
		}
		if err := writePCM16WAV(output, pcm, sampleRate); err != nil {
			t.Fatalf("write WAV: %v", err)
		}
		t.Logf("received %d PCM bytes; WAV: %s", len(pcm), output)
	case err := <-audioErr:
		t.Fatalf("TTS stream: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func writePCM16WAV(path string, pcm []byte, sampleRate int) error {
	var header bytes.Buffer
	dataSize := uint32(len(pcm))
	for _, value := range []any{
		[]byte("RIFF"), uint32(36) + dataSize, []byte("WAVE"), []byte("fmt "),
		uint32(16), uint16(1), uint16(1), uint32(sampleRate), uint32(sampleRate * 2),
		uint16(2), uint16(16), []byte("data"), dataSize,
	} {
		if err := binary.Write(&header, binary.LittleEndian, value); err != nil {
			return fmt.Errorf("encode WAV header: %w", err)
		}
	}
	return os.WriteFile(path, append(header.Bytes(), pcm...), 0o600)
}

func envIntForTest(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
