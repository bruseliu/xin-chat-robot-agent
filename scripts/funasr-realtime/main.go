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

	// Read responses in background
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

	// Send 200ms chunks (16kHz 16bit mono = 6400 bytes per 200ms)
	chunkSize := 6400
	chunkDuration := 200 * time.Millisecond
	sent := 0
	start := time.Now()

	for i := 0; i < len(pcm); i += chunkSize {
		end := i + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			fmt.Fprintf(os.Stderr, "write audio: %v\n", err)
			os.Exit(1)
		}
		sent++
		time.Sleep(chunkDuration)
	}

	elapsed := time.Since(start)
	fmt.Printf("\n[client] sent %d chunks in %.1fs, sending STOP...\n", sent, elapsed.Seconds())

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
