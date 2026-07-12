// End-to-end test: Silero VAD detects speech boundaries, FunASR transcribes
// via a single long-lived WebSocket connection using SEGMENT_END.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"xin-chat-robot-agent/internal/vad"
)

func main() {
	wavPath := "internal/vad/testdata/test2.wav"
	if len(os.Args) > 1 {
		wavPath = os.Args[1]
	}

	wav, err := os.ReadFile(wavPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read wav: %v\n", err)
		os.Exit(1)
	}
	pcm := wav[44:]
	duration := float64(len(pcm)) / 2 / 16000
	fmt.Printf("audio: %s, pcm bytes: %d, duration: %.1fs\n", wavPath, len(pcm), duration)

	// Initialize Silero VAD
	detector, err := vad.NewSileroStreamingDetector(vad.SileroConfig{
		ModelPath:            "models/silero-vad/silero_vad.onnx",
		SampleRate:           16000,
		Threshold:            0.5,
		MinSilenceDurationMs: 300,
		MinSpeechDurationMs:  250,
		SpeechPadMs:          30,
		PreRollMs:            300,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "init silero vad: %v\n", err)
		os.Exit(1)
	}
	defer detector.Close()

	// Connect to FunASR
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, _, err := dialer.DialContext(context.Background(), "ws://127.0.0.1:10095", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("START")); err != nil {
		fmt.Fprintf(os.Stderr, "write START: %v\n", err)
		os.Exit(1)
	}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		fmt.Fprintf(os.Stderr, "read start: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[server] %s\n\n", msg)

	// Read responses in background
	done := make(chan struct{})
	var segEndSentAt time.Time
	var segEndMu sync.Mutex
	seen := make(map[string]struct{})
	go func() {
		defer close(done)
		for {
			conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			recvAt := time.Now()
			var obj map[string]any
			if json.Unmarshal(msg, &obj) == nil {
				event, _ := obj["event"].(string)
				if event == "segment_end" {
					segEndMu.Lock()
					latency := recvAt.Sub(segEndSentAt)
					segEndMu.Unlock()
					sentences, _ := obj["sentences"].([]any)
					for _, s := range sentences {
						sm, _ := s.(map[string]any)
						text, _ := sm["text"].(string)
						if _, dup := seen[text]; dup {
							continue
						}
						seen[text] = struct{}{}
						fmt.Printf("  ✓ SENTENCE [%dms]: %s\n", latency.Milliseconds(), text)
					}
				} else if event == "stopped" {
					fmt.Printf("[server] stopped\n")
					return
				} else {
					partial, _ := obj["partial"].(string)
					if partial != "" {
						fmt.Printf("  … partial: %s\n", partial)
					}
				}
			}
		}
	}()

	// Stream audio through Silero VAD → FunASR
	chunkSize := 6400 // 200ms at 16kHz 16bit mono (3200 samples * 2 bytes)
	chunkDuration := 200 * time.Millisecond
	segmentCount := 0
	ctx := context.Background()

	for i := 0; i < len(pcm); i += chunkSize {
		end := i + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		chunk := pcm[i:end]

		// Feed to Silero VAD
		samples := make([]float32, len(chunk)/2)
		for j := 0; j < len(chunk)-1; j += 2 {
			sample := int16(chunk[j]) | int16(chunk[j+1])<<8
			samples[j/2] = float32(sample) / 32768.0
		}

		out, err := detector.Process(ctx, vad.Chunk{Samples: samples, SampleRate: 16000})
		if err != nil {
			fmt.Fprintf(os.Stderr, "vad process: %v\n", err)
			os.Exit(1)
		}

		// Send speech audio to FunASR
		if len(out.Chunk.Samples) > 0 {
			// Convert float32 back to int16 PCM for sending
			pcmOut := make([]byte, len(out.Chunk.Samples)*2)
			for j, s := range out.Chunk.Samples {
				v := int16(s * 32768.0)
				pcmOut[j*2] = byte(v)
				pcmOut[j*2+1] = byte(v >> 8)
			}
			if err := conn.WriteMessage(websocket.BinaryMessage, pcmOut); err != nil {
				fmt.Fprintf(os.Stderr, "write audio: %v\n", err)
				os.Exit(1)
			}
		}

		// VAD detected end of speech → send SEGMENT_END
		if out.VADEnd {
			segmentCount++
			ts := float64(i) / 2 / 16000
			fmt.Printf("\n[vad] speech end detected at %.2fs → sending SEGMENT_END (#%d)\n", ts, segmentCount)
			segEndMu.Lock()
			segEndSentAt = time.Now()
			segEndMu.Unlock()
			if err := conn.WriteMessage(websocket.TextMessage, []byte("SEGMENT_END")); err != nil {
				fmt.Fprintf(os.Stderr, "write SEGMENT_END: %v\n", err)
				os.Exit(1)
			}
			time.Sleep(500 * time.Millisecond)
		}

		time.Sleep(chunkDuration)
	}

	fmt.Printf("\n[client] Audio done. Segments detected by Silero: %d. Sending STOP...\n", segmentCount)
	if err := conn.WriteMessage(websocket.TextMessage, []byte("STOP")); err != nil {
		fmt.Fprintf(os.Stderr, "write STOP: %v\n", err)
		os.Exit(1)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Println("[client] timeout")
	}
}
