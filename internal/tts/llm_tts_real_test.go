package tts

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	llmprovider "xin-chat-robot-agent/internal/llm"
)

func TestDoubaoLLMToTTSRealStream(t *testing.T) {
	if os.Getenv("DOUBAO_LLM_TTS_REAL_TEST") != "1" {
		t.Skip("set DOUBAO_LLM_TTS_REAL_TEST=1 to run real Doubao LLM-to-TTS integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(envIntForTest("DOUBAO_LLM_TTS_TIMEOUT_SECONDS", 180))*time.Second)
	defer cancel()

	llmStream, err := llmprovider.NewDoubaoLLM(ctx, llmprovider.DoubaoConfig{
		BaseURL: os.Getenv("DOUBAO_LLM_BASE_URL"),
		APIKey:  os.Getenv("DOUBAO_LLM_API_KEY"),
		Model:   os.Getenv("DOUBAO_LLM_MODEL"),
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDoubaoLLM: %v", err)
	}
	sampleRate := envIntForTest("DOUBAO_TTS_SAMPLE_RATE", 24000)
	var speech *DoubaoStream
	defer func() {
		if speech != nil {
			_ = speech.Close()
		}
	}()

	started := time.Now()
	audioResult := make(chan []byte, 1)
	audioErr := make(chan error, 1)
	startSpeech := func() error {
		if speech != nil {
			return nil
		}
		var startErr error
		speech, startErr = NewDoubaoStream(ctx, DoubaoConfig{
			URL: os.Getenv("DOUBAO_TTS_URL"), AppID: os.Getenv("DOUBAO_TTS_APP_ID"),
			AccessToken: os.Getenv("DOUBAO_TTS_ACCESS_TOKEN"), ResourceID: os.Getenv("DOUBAO_TTS_RESOURCE_ID"),
			Speaker: os.Getenv("DOUBAO_TTS_SPEAKER"), SampleRate: sampleRate, Timeout: 30 * time.Second,
		})
		if startErr != nil {
			return startErr
		}
		var player io.WriteCloser
		var waitPlayer func() error
		if os.Getenv("DOUBAO_LLM_TTS_PLAY") == "1" {
			player, waitPlayer, startErr = startPCMPlayer(ctx, sampleRate)
			if startErr != nil {
				return startErr
			}
			t.Log("streaming PCM to aplay")
		}
		go collectRealTTSAudio(ctx, speech, audioResult, audioErr, started, t, player, waitPlayer)
		return nil
	}

	prompt := strings.TrimSpace(os.Getenv("DOUBAO_LLM_TTS_PROMPT"))
	if prompt == "" {
		prompt = "请用一句简短的中文介绍你自己。"
	}
	t.Logf("prompt=%q", prompt)
	request := &model.LLMRequest{
		Model: os.Getenv("DOUBAO_LLM_MODEL"),
		Config: &genai.GenerateContentConfig{
			MaxOutputTokens:   int32(envIntForTest("DOUBAO_LLM_TTS_MAX_TOKENS", 512)),
			SystemInstruction: genai.NewContentFromText("请使用自然、适合语音播报的中文完整回答，不使用Markdown格式，控制在300字以内，不能在半句话中结束。", genai.RoleUser),
		},
		Contents: []*genai.Content{
			genai.NewContentFromText(prompt, genai.RoleUser),
		},
	}
	var answer strings.Builder
	textChunker := NewTextChunker(24)
	ttsChunkCount := 0
	deltaCount := 0
	for response, streamErr := range llmStream.GenerateContent(ctx, request, true) {
		if streamErr != nil {
			if speech != nil {
				_ = speech.Cancel(ctx)
			}
			t.Fatalf("Doubao LLM stream: %v", streamErr)
		}
		if response == nil || !response.Partial {
			continue
		}
		delta := llmResponseText(response.Content)
		if delta == "" {
			continue
		}
		deltaCount++
		t.Logf("LLM delta #%d after %s: %q", deltaCount, time.Since(started).Round(time.Millisecond), delta)
		answer.WriteString(delta)
		for _, chunk := range textChunker.Push(delta) {
			if err := startSpeech(); err != nil {
				t.Fatalf("start TTS: %v", err)
			}
			ttsChunkCount++
			t.Logf("TTS text chunk #%d: %q", ttsChunkCount, chunk)
			if err := speech.WriteText(ctx, chunk); err != nil {
				t.Fatalf("TTS WriteText: %v", err)
			}
		}
	}
	if deltaCount == 0 {
		t.Fatal("Doubao LLM returned no streaming deltas")
	}
	t.Logf("LLM stream completed with %d deltas after %s", deltaCount, time.Since(started).Round(time.Millisecond))
	if chunk := textChunker.Flush(); chunk != "" {
		if err := startSpeech(); err != nil {
			t.Fatalf("start TTS: %v", err)
		}
		ttsChunkCount++
		t.Logf("TTS text chunk #%d: %q", ttsChunkCount, chunk)
		if err := speech.WriteText(ctx, chunk); err != nil {
			t.Fatalf("TTS WriteText: %v", err)
		}
	}
	t.Logf("sent %d aggregated text chunks to TTS", ttsChunkCount)
	if err := speech.Finish(ctx); err != nil {
		t.Fatalf("TTS Finish: %v", err)
	}

	select {
	case pcm := <-audioResult:
		if len(pcm) == 0 {
			t.Fatal("empty TTS audio")
		}
		output := "playback only"
		if os.Getenv("DOUBAO_LLM_TTS_PLAY") != "1" {
			output = filepath.Join("..", "..", "tmp", "doubao_llm_tts_stream.wav")
			if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
				t.Fatalf("create output directory: %v", err)
			}
			if err := writePCM16WAV(output, pcm, sampleRate); err != nil {
				t.Fatalf("write WAV: %v", err)
			}
		}
		t.Logf("answer=%q", answer.String())
		t.Logf("received %d PCM bytes in %s; WAV: %s", len(pcm), time.Since(started).Round(time.Millisecond), output)
	case err := <-audioErr:
		t.Fatalf("TTS audio stream: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func collectRealTTSAudio(ctx context.Context, speech Stream, result chan<- []byte, errs chan<- error, started time.Time, t *testing.T, player io.WriteCloser, waitPlayer func() error) {
	var pcm []byte
	firstAudio := true
	for {
		select {
		case chunk, ok := <-speech.Audio():
			if !ok || chunk.Final {
				if player != nil {
					_ = player.Close()
					if err := waitPlayer(); err != nil {
						errs <- fmt.Errorf("aplay: %w", err)
						return
					}
				}
				result <- pcm
				return
			}
			if len(chunk.PCM) > 0 && firstAudio {
				firstAudio = false
				t.Logf("first TTS audio after %s", time.Since(started).Round(time.Millisecond))
			}
			if player != nil {
				if _, err := player.Write(chunk.PCM); err != nil {
					errs <- fmt.Errorf("write PCM to aplay: %w", err)
					return
				}
			}
			pcm = append(pcm, chunk.PCM...)
		case err, ok := <-speech.Errors():
			if ok && err != nil {
				errs <- err
				return
			}
		case <-ctx.Done():
			errs <- ctx.Err()
			return
		}
	}
}

func llmResponseText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var text strings.Builder
	for _, part := range content.Parts {
		if part != nil {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func startPCMPlayer(ctx context.Context, sampleRate int) (io.WriteCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, "aplay", "-q", "-t", "raw", "-f", "S16_LE", "-r", strconv.Itoa(sampleRate), "-c", "1")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	return stdin, cmd.Wait, nil
}
