package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	wavPath := "internal/vad/testdata/speech_16k_mono.wav"
	if len(os.Args) > 1 {
		wavPath = os.Args[1]
	}

	wav, err := os.ReadFile(wavPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read wav: %v\n", err)
		os.Exit(1)
	}
	pcm := wav[44:]
	fmt.Printf("audio: %s, pcm bytes: %d, duration: %.1fs\n", wavPath, len(pcm), float64(len(pcm))/2/16000)

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
		fmt.Fprintf(os.Stderr, "read start response: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[server] %s\n", msg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn.SetReadDeadline(time.Now().Add(15 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil {
				fmt.Printf("[server] connection closed: %v\n", err)
				return
			}
			var obj any
			if json.Unmarshal(msg, &obj) == nil {
				fmt.Print("[server] ")
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				enc.Encode(obj)
			} else {
				fmt.Printf("[server] %s\n", msg)
			}
		}
	}()

	chunkSize := 6400 // 200ms at 16kHz 16bit mono
	chunkDuration := 200 * time.Millisecond
	halfPoint := len(pcm) / 2

	fmt.Printf("\n[client] Sending first half (%d bytes, %.1fs)...\n", halfPoint, float64(halfPoint)/2/16000)
	for i := 0; i < halfPoint; i += chunkSize {
		end := i + chunkSize
		if end > halfPoint {
			end = halfPoint
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			fmt.Fprintf(os.Stderr, "write audio: %v\n", err)
			os.Exit(1)
		}
		time.Sleep(chunkDuration)
	}

	fmt.Printf("[client] >>> Sending SEGMENT_END <<<\n")
	if err := conn.WriteMessage(websocket.TextMessage, []byte("SEGMENT_END")); err != nil {
		fmt.Fprintf(os.Stderr, "write SEGMENT_END: %v\n", err)
		os.Exit(1)
	}
	time.Sleep(2 * time.Second)

	fmt.Printf("[client] Sending second half (%d bytes, %.1fs)...\n", len(pcm)-halfPoint, float64(len(pcm)-halfPoint)/2/16000)
	for i := halfPoint; i < len(pcm); i += chunkSize {
		end := i + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			fmt.Fprintf(os.Stderr, "write audio: %v\n", err)
			os.Exit(1)
		}
		time.Sleep(chunkDuration)
	}

	fmt.Printf("[client] >>> Sending STOP <<<\n")
	if err := conn.WriteMessage(websocket.TextMessage, []byte("STOP")); err != nil {
		fmt.Fprintf(os.Stderr, "write STOP: %v\n", err)
		os.Exit(1)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		fmt.Println("[client] timeout waiting for final response")
	}
}
