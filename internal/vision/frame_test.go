package vision

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"
)

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	imageData := image.NewRGBA(image.Rect(0, 0, width, height))
	imageData.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	if err := jpeg.Encode(&out, imageData, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestDecodeBase64JPEG(t *testing.T) {
	data := testJPEG(t, 16, 8)
	frame, err := DecodeBase64JPEG(base64.StdEncoding.EncodeToString(data), MIMETypeJPEG, Limits{MaxFrameBytes: 1024, MaxWidth: 32, MaxHeight: 32})
	if err != nil {
		t.Fatalf("DecodeBase64JPEG() error = %v", err)
	}
	if frame.Width != 16 || frame.Height != 8 || len(frame.Data) != len(data) {
		t.Fatalf("frame = %+v, want 16x8 JPEG", frame)
	}
}

func TestDecodeBase64JPEGRejectsInvalidInput(t *testing.T) {
	if _, err := DecodeBase64JPEG("not-base64", MIMETypeJPEG, Limits{}); err == nil {
		t.Fatal("DecodeBase64JPEG() error = nil, want invalid Base64 error")
	}
	if _, err := DecodeBase64JPEG(base64.StdEncoding.EncodeToString(testJPEG(t, 40, 8)), MIMETypeJPEG, Limits{MaxWidth: 32, MaxHeight: 32}); err == nil {
		t.Fatal("DecodeBase64JPEG() error = nil, want dimensions error")
	}
}

func TestFrameContextCopiesData(t *testing.T) {
	frame := Frame{MIMEType: MIMETypeJPEG, Data: []byte{1, 2}, CapturedAt: time.Now()}
	ctx := WithFrame(t.Context(), frame)
	frame.Data[0] = 9
	got, ok := FrameFromContext(ctx)
	if !ok || got.Data[0] != 1 {
		t.Fatalf("FrameFromContext() = %+v, %v", got, ok)
	}
}
