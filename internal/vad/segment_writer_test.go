package vad

import (
	"context"
	"os"
	"testing"
)

func TestWAVSegmentWriterWritesCompletedSegment(t *testing.T) {
	writer, err := NewWAVSegmentWriter(WAVSegmentWriterConfig{
		Dir:        t.TempDir(),
		SampleRate: 16000,
		Prefix:     "test_segment",
	})
	if err != nil {
		t.Fatalf("NewWAVSegmentWriter() error = %v", err)
	}

	pcm := make([]byte, 1600*2)
	files, err := writer.Write(context.Background(), Chunk{PCM16LE: pcm, SampleRate: 16000}, Decision{
		SampleRate: 16000,
		Segments: []Segment{
			{SpeechStartAt: 0, SpeechEndAt: 0.05},
		},
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files len = %d, want 1", len(files))
	}
	info, err := os.Stat(files[0].Path)
	if err != nil {
		t.Fatalf("stat segment file: %v", err)
	}
	wantSize := int64(44 + 800*2)
	if info.Size() != wantSize {
		t.Fatalf("segment file size = %d, want %d", info.Size(), wantSize)
	}
}

func TestWAVSegmentWriterSkipsOpenSegment(t *testing.T) {
	writer, err := NewWAVSegmentWriter(WAVSegmentWriterConfig{Dir: t.TempDir(), SampleRate: 16000})
	if err != nil {
		t.Fatalf("NewWAVSegmentWriter() error = %v", err)
	}
	files, err := writer.Write(context.Background(), Chunk{PCM16LE: make([]byte, 1600), SampleRate: 16000}, Decision{
		SampleRate: 16000,
		Segments:   []Segment{{SpeechStartAt: 0.1}},
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("files len = %d, want 0", len(files))
	}
}
