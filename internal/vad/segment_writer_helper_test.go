package vad

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

type testSegmentFile struct {
	Path       string
	Segment    Segment
	SampleRate int
	Bytes      int
}

type WAVSegmentWriterConfig struct {
	Dir        string
	SampleRate int
	Prefix     string
}

type WAVSegmentWriter struct {
	dir        string
	sampleRate int
	prefix     string
	history    []byte
	nextIndex  int
	written    map[string]bool
}

func NewWAVSegmentWriter(cfg WAVSegmentWriterConfig) (*WAVSegmentWriter, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("vad: segment dir is required")
	}
	if cfg.SampleRate == 0 {
		cfg.SampleRate = DefaultSampleRate
	}
	if cfg.SampleRate != 8000 && cfg.SampleRate != 16000 {
		return nil, fmt.Errorf("vad: segment writer sample rate must be 8000 or 16000")
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "segment"
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("vad: create segment dir: %w", err)
	}
	return &WAVSegmentWriter{
		dir:        cfg.Dir,
		sampleRate: cfg.SampleRate,
		prefix:     cfg.Prefix,
		written:    map[string]bool{},
	}, nil
}

func (w *WAVSegmentWriter) Write(ctx context.Context, chunk Chunk, decision Decision) ([]testSegmentFile, error) {
	if w == nil {
		return nil, fmt.Errorf("vad: nil segment writer")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pcm, sampleRate, err := chunkPCM16LE(chunk, w.sampleRate)
	if err != nil {
		return nil, err
	}
	if sampleRate != w.sampleRate {
		return nil, fmt.Errorf("vad: segment writer sample rate changed from %d to %d", w.sampleRate, sampleRate)
	}
	w.history = append(w.history, pcm...)

	var files []testSegmentFile
	for _, segment := range decision.Segments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if segment.SpeechEndAt <= segment.SpeechStartAt {
			continue
		}
		startSample := sampleIndex(segment.SpeechStartAt, w.sampleRate)
		endSample := sampleIndex(segment.SpeechEndAt, w.sampleRate)
		key := fmt.Sprintf("%d:%d", startSample, endSample)
		if w.written[key] {
			continue
		}
		startByte := clampByteOffset(startSample * 2)
		endByte := clampByteOffset(endSample * 2)
		if startByte >= len(w.history) {
			continue
		}
		if endByte > len(w.history) {
			endByte = len(w.history)
		}
		if endByte <= startByte {
			continue
		}
		w.nextIndex++
		path := filepath.Join(w.dir, fmt.Sprintf("%s_%06d_%08d_%08d.wav", w.prefix, w.nextIndex, startSample, endSample))
		if err := writePCM16WAV(path, w.history[startByte:endByte], w.sampleRate); err != nil {
			return nil, err
		}
		w.written[key] = true
		files = append(files, testSegmentFile{
			Path:       path,
			Segment:    segment,
			SampleRate: w.sampleRate,
			Bytes:      endByte - startByte,
		})
	}
	return files, nil
}

func (w *WAVSegmentWriter) Reset() error {
	if w == nil {
		return nil
	}
	w.history = nil
	w.nextIndex = 0
	w.written = map[string]bool{}
	return nil
}

func (w *WAVSegmentWriter) Close() error {
	return nil
}

func chunkPCM16LE(chunk Chunk, defaultSampleRate int) ([]byte, int, error) {
	sampleRate := chunk.SampleRate
	if sampleRate == 0 {
		sampleRate = defaultSampleRate
	}
	if len(chunk.PCM16LE) > 0 {
		if len(chunk.PCM16LE)%2 != 0 {
			return nil, 0, fmt.Errorf("vad: pcm16le length must be even")
		}
		return append([]byte(nil), chunk.PCM16LE...), sampleRate, nil
	}
	pcm := make([]byte, len(chunk.Samples)*2)
	for i, sample := range chunk.Samples {
		if sample > 1 {
			sample = 1
		}
		if sample < -1 {
			sample = -1
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(math.Round(float64(sample*32767)))))
	}
	return pcm, sampleRate, nil
}

func writePCM16WAV(path string, pcm []byte, sampleRate int) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("vad: create wav segment: %w", err)
	}
	defer f.Close()

	dataSize := uint32(len(pcm))
	byteRate := uint32(sampleRate * 2)
	blockAlign := uint16(2)
	bitsPerSample := uint16(16)
	if _, err := f.Write([]byte("RIFF")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(36)+dataSize); err != nil {
		return err
	}
	if _, err := f.Write([]byte("WAVEfmt ")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, byteRate); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, blockAlign); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, bitsPerSample); err != nil {
		return err
	}
	if _, err := f.Write([]byte("data")); err != nil {
		return err
	}
	if err := binary.Write(f, binary.LittleEndian, dataSize); err != nil {
		return err
	}
	_, err = f.Write(pcm)
	return err
}

func sampleIndex(seconds float64, sampleRate int) int {
	if seconds <= 0 {
		return 0
	}
	return int(math.Round(seconds * float64(sampleRate)))
}

func clampByteOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}
