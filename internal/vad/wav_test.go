//go:build cgo

package vad

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSileroDetectsSpeechFromRealWAV(t *testing.T) {
	modelPath := os.Getenv("SILERO_VAD_MODEL_PATH")
	if modelPath == "" {
		t.Skip("SILERO_VAD_MODEL_PATH is not set")
	}

	cases := []struct {
		name string
		path string
	}{
		{name: "librispeech_1089_134686_0001", path: "testdata/speech_16k_mono.wav"},
		{name: "librispeech_1221_135766_0001", path: "testdata/1221-135766-0001.wav"},
		{name: "librispeech_1221_135766_0002", path: "testdata/1221-135766-0002.wav"},
		{name: "test_wav", path: "testdata/test.wav"},
	}

	segmentRoot := vadSegmentTestDir(t)
	if err := os.RemoveAll(segmentRoot); err != nil {
		t.Fatalf("clean VAD segment test dir: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pcm, sampleRate := readPCM16MonoWAV(t, tc.path)
			if sampleRate != 16000 {
				t.Fatalf("sample rate = %d, want 16000", sampleRate)
			}

			detector, err := NewSileroStreamingDetector(SileroConfig{
				ModelPath:            resolveTestModelPath(t, modelPath),
				SampleRate:           sampleRate,
				Threshold:            0.5,
				MinSilenceDurationMs: 200,
				SpeechPadMs:          30,
			})
			if err != nil {
				t.Fatalf("NewSileroStreamingDetector() error = %v", err)
			}
			defer detector.Close()

			segmentDir := filepath.Join(segmentRoot, tc.name)
			writer, err := NewWAVSegmentWriter(WAVSegmentWriterConfig{
				Dir:        segmentDir,
				SampleRate: sampleRate,
				Prefix:     tc.name,
			})
			if err != nil {
				t.Fatalf("NewWAVSegmentWriter() error = %v", err)
			}

			const chunkBytes = 512 * 2
			var sawSpeech bool
			var segmentCount int
			var fileCount int
			for offset := 0; offset < len(pcm); offset += chunkBytes {
				end := offset + chunkBytes
				if end > len(pcm) {
					end = len(pcm)
				}
				chunk := Chunk{
					PCM16LE:    pcm[offset:end],
					SampleRate: sampleRate,
					Final:      end == len(pcm),
				}
				decision, err := detector.Process(t.Context(), chunk)
				if err != nil {
					t.Fatalf("Process(offset=%d) error = %v", offset, err)
				}
				if decision.SpeechStarted || decision.SpeechActive || decision.SpeechEnded {
					sawSpeech = true
				}
				segmentCount += len(decision.Segments)
				files, err := writer.Write(t.Context(), chunk, decision)
				if err != nil {
					t.Fatalf("Write(offset=%d) error = %v", offset, err)
				}
				fileCount += len(files)
			}
			if !sawSpeech {
				t.Fatalf("Silero VAD did not detect speech in %s", tc.path)
			}
			if segmentCount == 0 {
				t.Fatalf("Silero VAD produced no speech segments for %s", tc.path)
			}
			if fileCount == 0 {
				t.Fatal("Silero VAD produced segments, but WAV segment writer wrote no files")
			}
			t.Logf("wrote %d VAD segment WAV file(s) to %s", fileCount, segmentDir)
		})
	}
}

func vadSegmentTestDir(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("VAD_SEGMENT_TEST_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(repoRootFromTest(t), "tmp", "vad-test-segments")
}

func readPCM16MonoWAV(t *testing.T, path string) ([]byte, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open wav: %v", err)
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
			format, channels, rate, bits := parsePCMFormat(t, chunk)
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

func parsePCMFormat(t *testing.T, chunk []byte) (format, channels, sampleRate, bitsPerSample int) {
	t.Helper()
	if len(chunk) < 16 {
		t.Fatalf("fmt chunk too short: %d", len(chunk))
	}
	return int(binary.LittleEndian.Uint16(chunk[0:2])),
		int(binary.LittleEndian.Uint16(chunk[2:4])),
		int(binary.LittleEndian.Uint32(chunk[4:8])),
		int(binary.LittleEndian.Uint16(chunk[14:16]))
}
