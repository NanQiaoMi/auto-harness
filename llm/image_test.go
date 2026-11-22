package llm

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{color.Black, color.White}
	img := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeWebPVP8L(w, h int) []byte {
	buf := make([]byte, 30)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 22)
	copy(buf[8:12], "WEBP")
	copy(buf[12:16], "VP8L")
	binary.LittleEndian.PutUint32(buf[16:20], 10)
	buf[20] = 0x2f
	bits := uint32((w-1)&0x3fff) | (uint32((h-1)&0x3fff) << 14)
	binary.LittleEndian.PutUint32(buf[21:25], bits)
	return buf
}

func makeWebPVP8X(w, h int) []byte {
	buf := make([]byte, 30)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 22)
	copy(buf[8:12], "WEBP")
	copy(buf[12:16], "VP8X")
	binary.LittleEndian.PutUint32(buf[16:20], 10)
	w0 := uint32(w - 1)
	h0 := uint32(h - 1)
	buf[24] = byte(w0)
	buf[25] = byte(w0 >> 8)
	buf[26] = byte(w0 >> 16)
	buf[27] = byte(h0)
	buf[28] = byte(h0 >> 8)
	buf[29] = byte(h0 >> 16)
	return buf
}

func makeWebPVP8(w, h int) []byte {
	buf := make([]byte, 30)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], 22)
	copy(buf[8:12], "WEBP")
	copy(buf[12:16], "VP8 ")
	binary.LittleEndian.PutUint32(buf[16:20], 10)
	buf[23] = 0x9d
	buf[24] = 0x01
	buf[25] = 0x2a
	binary.LittleEndian.PutUint16(buf[26:28], uint16(w))
	binary.LittleEndian.PutUint16(buf[28:30], uint16(h))
	return buf
}

func TestImageFromBytesFormats(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
		w, h int
	}{
		{"png", makePNG(t, 12, 8), "image/png", 12, 8},
		{"jpeg", makeJPEG(t, 16, 10), "image/jpeg", 16, 10},
		{"gif", makeGIF(t, 5, 7), "image/gif", 5, 7},
		{"webp-vp8l", makeWebPVP8L(320, 240), "image/webp", 320, 240},
		{"webp-vp8x", makeWebPVP8X(1920, 1080), "image/webp", 1920, 1080},
		{"webp-vp8", makeWebPVP8(640, 480), "image/webp", 640, 480},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img, err := ImageFromBytes(tc.data, tc.mime, "sample")
			if err != nil {
				t.Fatalf("ImageFromBytes: %v", err)
			}
			if img.MediaType != tc.mime {
				t.Errorf("MediaType = %q, want %q", img.MediaType, tc.mime)
			}
			if img.Width != tc.w || img.Height != tc.h {
				t.Errorf("dims = %dx%d, want %dx%d", img.Width, img.Height, tc.w, tc.h)
			}
			if img.ByteSize != len(tc.data) {
				t.Errorf("ByteSize = %d, want %d", img.ByteSize, len(tc.data))
			}
			if len(img.SHA256) != 64 {
				t.Errorf("SHA256 = %q", img.SHA256)
			}
			got, err := img.Bytes()
			if err != nil || !bytes.Equal(got, tc.data) {
				t.Errorf("Bytes() mismatch (err=%v)", err)
			}
			ph := img.Placeholder()
			if !strings.Contains(ph, tc.mime) || !strings.Contains(ph, "sha256:") {
				t.Errorf("Placeholder = %q", ph)
			}
		})
	}
}

func TestImageFromBytesValidation(t *testing.T) {
	if _, err := ImageFromBytes(nil, "", ""); err == nil {
		t.Error("want error for empty data")
	}
	if _, err := ImageFromBytes([]byte("plain text"), "", ""); err == nil {
		t.Error("want error for non-image text")
	}
	pngBytes := makePNG(t, 4, 4)
	if _, err := ImageFromBytes(pngBytes, "image/jpeg", ""); err == nil {
		t.Error("want error when declared mime mismatches content")
	}
	if _, err := ImageFromBytes(pngBytes, "image/bmp", ""); err == nil {
		t.Error("want error for unsupported mime")
	}
	oversized := make([]byte, MaxImageBytes+1)
	copy(oversized, pngBytes)
	if _, err := ImageFromBytes(oversized, "", ""); err == nil {
		t.Error("want error for oversized image")
	}
	// Truncated PNG header after magic bytes should fail decoding.
	if _, err := ImageFromBytes([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0}, "", ""); err == nil {
		t.Error("want error for corrupt PNG")
	}
}

func TestLoadImageFileAndBytesFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shot.png")
	raw := makePNG(t, 9, 6)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	img, err := LoadImageFile(path)
	if err != nil {
		t.Fatalf("LoadImageFile: %v", err)
	}
	if img.Path != path || img.Label != "shot.png" || img.Width != 9 || img.Height != 6 {
		t.Errorf("unexpected image metadata: %+v", img)
	}
	// Stripping in-memory Data should still resolve via Path.
	img.Data = nil
	loaded, err := img.Bytes()
	if err != nil || !bytes.Equal(loaded, raw) {
		t.Errorf("Bytes via path failed: err=%v len=%d", err, len(loaded))
	}

	// Empty file fails.
	empty := filepath.Join(dir, "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageFile(empty); err == nil {
		t.Error("want error for empty file")
	}
	if _, err := LoadImageFile(dir); err == nil {
		t.Error("want error for directory")
	}
}

func TestModelSupportsImage(t *testing.T) {
	claude, ok := Lookup("claude-sonnet-5-5")
	if !ok || !claude.SupportsImage() {
		t.Errorf("claude-sonnet-5-5 should support images")
	}
	ds, ok := Lookup("deepseek-chat")
	if !ok || ds.SupportsImage() {
		t.Errorf("deepseek-chat should be text-only")
	}
	custom := Model{ID: "custom-gateway-model"}
	if !custom.SupportsImage() {
		t.Errorf("custom model without declared input should default to supporting images")
	}
}
