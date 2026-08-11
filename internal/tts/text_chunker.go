package tts

import "strings"

// TextChunker combines token-sized LLM deltas into TTS-friendly phrase chunks.
type TextChunker struct {
	buffer   []rune
	maxRunes int
}

func NewTextChunker(maxRunes int) *TextChunker {
	if maxRunes <= 0 {
		maxRunes = 12
	}
	return &TextChunker{maxRunes: maxRunes}
}

func (c *TextChunker) Push(text string) []string {
	c.buffer = append(c.buffer, []rune(text)...)
	var chunks []string
	for len(c.buffer) > 0 {
		end := c.boundary()
		if end == 0 {
			break
		}
		chunk := strings.TrimSpace(string(c.buffer[:end]))
		c.buffer = c.buffer[end:]
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

func (c *TextChunker) Flush() string {
	chunk := strings.TrimSpace(string(c.buffer))
	c.buffer = nil
	return chunk
}

func (c *TextChunker) boundary() int {
	limit := len(c.buffer)
	if limit > c.maxRunes {
		limit = c.maxRunes
	}
	for i := 0; i < limit; i++ {
		if strings.ContainsRune("，。！？；：,.!?;:\n", c.buffer[i]) {
			return i + 1
		}
	}
	if len(c.buffer) >= c.maxRunes {
		return c.maxRunes
	}
	return 0
}
