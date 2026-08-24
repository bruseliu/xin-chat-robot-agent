// Package vision contains ephemeral image data used by a single model request.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg"
	"strings"
	"time"
)

const MIMETypeJPEG = "image/jpeg"

// Limits constrain client-provided camera frames before they enter a session.
// A frame is never written to a conversation or memory store.
type Limits struct {
	Enabled       bool
	MaxFrameBytes int
	MaxWidth      int
	MaxHeight     int
	MaxAge        time.Duration
}

func (l Limits) Normalize() Limits {
	if l.MaxFrameBytes <= 0 {
		l.MaxFrameBytes = 512 * 1024
	}
	if l.MaxWidth <= 0 {
		l.MaxWidth = 1280
	}
	if l.MaxHeight <= 0 {
		l.MaxHeight = 720
	}
	if l.MaxAge <= 0 {
		l.MaxAge = 3 * time.Second
	}
	return l
}

// Frame is a verified JPEG held only in process memory for one connection.
type Frame struct {
	MIMEType   string
	Data       []byte
	Width      int
	Height     int
	CapturedAt time.Time
}

func (f Frame) Clone() Frame {
	copy := f
	copy.Data = append([]byte(nil), f.Data...)
	return copy
}

func (f Frame) Expired(now time.Time, maxAge time.Duration) bool {
	return f.CapturedAt.IsZero() || maxAge > 0 && now.Sub(f.CapturedAt) > maxAge
}

// DecodeBase64JPEG validates a client camera frame and stamps it with server
// receipt time. Only JPEG is accepted in the first realtime camera protocol.
func DecodeBase64JPEG(encoded, mimeType string, limits Limits) (Frame, error) {
	limits = limits.Normalize()
	if strings.EqualFold(strings.TrimSpace(mimeType), MIMETypeJPEG) == false {
		return Frame{}, fmt.Errorf("unsupported image MIME type %q", mimeType)
	}
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return Frame{}, fmt.Errorf("image data is empty")
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > limits.MaxFrameBytes {
		return Frame{}, fmt.Errorf("image exceeds %d byte limit", limits.MaxFrameBytes)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Frame{}, fmt.Errorf("decode image Base64: %w", err)
	}
	if len(data) == 0 || len(data) > limits.MaxFrameBytes {
		return Frame{}, fmt.Errorf("image exceeds %d byte limit", limits.MaxFrameBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		return Frame{}, fmt.Errorf("image is not a valid JPEG")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > limits.MaxWidth || config.Height > limits.MaxHeight {
		return Frame{}, fmt.Errorf("image dimensions %dx%d exceed %dx%d limit", config.Width, config.Height, limits.MaxWidth, limits.MaxHeight)
	}
	return Frame{MIMEType: MIMETypeJPEG, Data: data, Width: config.Width, Height: config.Height, CapturedAt: time.Now()}, nil
}

type frameContextKey struct{}

// WithFrame attaches a defensive copy to one request context. The ADK session
// still receives a text-only user content, so frames are not persisted there.
func WithFrame(ctx context.Context, frame Frame) context.Context {
	return context.WithValue(ctx, frameContextKey{}, frame.Clone())
}

func FrameFromContext(ctx context.Context) (Frame, bool) {
	frame, ok := ctx.Value(frameContextKey{}).(Frame)
	if !ok || len(frame.Data) == 0 {
		return Frame{}, false
	}
	return frame.Clone(), true
}
