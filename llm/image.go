package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxImageBytes caps a single image payload at 10 MiB to keep memory,
// disk blobs and provider requests bounded.
const MaxImageBytes = 10 * 1024 * 1024

// Image is an image attachment carried on a neutral Message.
//
// In memory, Data holds the raw bytes (for tests or freshly loaded inputs).
// When persisted to a session, Data is moved into the session blob store
// and Path points at that file so JSONL transcripts stay compact.
type Image struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data,omitempty"`
	Path      string `json:"path,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	ByteSize  int    `json:"byte_size,omitempty"`
	Label     string `json:"label,omitempty"`
}

// IsSupportedImageMIME reports whether mime is one of the supported
// static image formats (PNG, JPEG, WebP, GIF).
func IsSupportedImageMIME(mime string) bool {
	switch normalizeImageMIME(mime) {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return true
	default:
		return false
	}
}

// ExtForImageMIME returns the canonical filename extension (including
// the leading dot) for a supported image MIME type.
func ExtForImageMIME(mime string) string {
	switch normalizeImageMIME(mime) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}

// ImageMIMEFromExt maps a file extension to a supported image MIME type,
// returning "" when the extension is not a supported image format.
func ImageMIMEFromExt(ext string) string {
	switch strings.ToLower(strings.TrimSpace(ext)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return ""
	}
}

func normalizeImageMIME(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	if semi := strings.IndexByte(m, ';'); semi >= 0 {
		m = strings.TrimSpace(m[:semi])
	}
	if m == "image/jpg" {
		return "image/jpeg"
	}
	return m
}

// DetectImageMIME inspects magic bytes to identify PNG, JPEG, WebP or GIF.
// It returns "" if data does not match any supported format.
func DetectImageMIME(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg"
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	default:
		return ""
	}
}

// ImageFromBytes validates raw image bytes against size and magic-byte
// rules, extracts dimensions, and computes the SHA-256 digest.
func ImageFromBytes(data []byte, mediaType, label string) (Image, error) {
	if len(data) == 0 {
		return Image{}, fmt.Errorf("llm: image is empty")
	}
	if len(data) > MaxImageBytes {
		return Image{}, fmt.Errorf("llm: image exceeds %d bytes (%d bytes)", MaxImageBytes, len(data))
	}
	detected := DetectImageMIME(data)
	if detected == "" {
		return Image{}, fmt.Errorf("llm: unsupported or invalid image format")
	}
	if norm := normalizeImageMIME(mediaType); norm != "" {
		if !IsSupportedImageMIME(norm) {
			return Image{}, fmt.Errorf("llm: unsupported image media type %q", mediaType)
		}
		if norm != detected {
			return Image{}, fmt.Errorf("llm: image content is %s, declared %s", detected, norm)
		}
	}

	w, h, err := decodeImageDimensions(data, detected)
	if err != nil {
		return Image{}, fmt.Errorf("llm: decode %s header: %w", detected, err)
	}

	sum := sha256.Sum256(data)
	copied := make([]byte, len(data))
	copy(copied, data)
	return Image{
		MediaType: detected,
		Data:      copied,
		SHA256:    hex.EncodeToString(sum[:]),
		Width:     w,
		Height:    h,
		ByteSize:  len(data),
		Label:     label,
	}, nil
}

func decodeImageDimensions(data []byte, mime string) (int, int, error) {
	if mime == "image/webp" {
		return decodeWebPDimensions(data)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, fmt.Errorf("invalid dimensions %dx%d", cfg.Width, cfg.Height)
	}
	return cfg.Width, cfg.Height, nil
}

func decodeWebPDimensions(data []byte) (int, int, error) {
	if len(data) < 30 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return 0, 0, fmt.Errorf("truncated WebP header")
	}
	chunk := string(data[12:16])
	switch chunk {
	case "VP8 ":
		if data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, fmt.Errorf("invalid VP8 keyframe signature")
		}
		w := int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		h := int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
		if w <= 0 || h <= 0 {
			return 0, 0, fmt.Errorf("invalid VP8 dimensions %dx%d", w, h)
		}
		return w, h, nil
	case "VP8L":
		if data[20] != 0x2f {
			return 0, 0, fmt.Errorf("invalid VP8L signature")
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		w := int(bits&0x3fff) + 1
		h := int((bits>>14)&0x3fff) + 1
		return w, h, nil
	case "VP8X":
		w := int(uint32(data[24])|uint32(data[25])<<8|uint32(data[26])<<16) + 1
		h := int(uint32(data[27])|uint32(data[28])<<8|uint32(data[29])<<16) + 1
		return w, h, nil
	default:
		return 0, 0, fmt.Errorf("unsupported WebP chunk %q", chunk)
	}
}

// LoadImageFile reads and validates an image file from disk.
func LoadImageFile(path string) (Image, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Image{}, fmt.Errorf("llm: load image %s: %w", path, err)
	}
	if info.IsDir() {
		return Image{}, fmt.Errorf("llm: %s is a directory", path)
	}
	if info.Size() == 0 {
		return Image{}, fmt.Errorf("llm: image %s is empty", path)
	}
	if info.Size() > MaxImageBytes {
		return Image{}, fmt.Errorf("llm: image %s exceeds %d bytes (%d bytes)", path, MaxImageBytes, info.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return Image{}, fmt.Errorf("llm: load image %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
	if err != nil {
		return Image{}, fmt.Errorf("llm: read image %s: %w", path, err)
	}
	img, err := ImageFromBytes(data, "", filepath.Base(path))
	if err != nil {
		return Image{}, err
	}
	img.Path = path
	return img, nil
}

// Bytes returns the image payload from memory or, if stripped, from Path.
func (img Image) Bytes() ([]byte, error) {
	if len(img.Data) > 0 {
		if len(img.Data) > MaxImageBytes {
			return nil, fmt.Errorf("llm: image exceeds %d bytes (%d bytes)", MaxImageBytes, len(img.Data))
		}
		return img.Data, nil
	}
	if img.Path == "" {
		return nil, fmt.Errorf("llm: image has neither data nor path")
	}
	info, err := os.Stat(img.Path)
	if err != nil {
		return nil, fmt.Errorf("llm: read image %s: %w", img.Path, err)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("llm: image %s is empty", img.Path)
	}
	if info.Size() > MaxImageBytes {
		return nil, fmt.Errorf("llm: image %s exceeds %d bytes (%d bytes)", img.Path, MaxImageBytes, info.Size())
	}
	f, err := os.Open(img.Path)
	if err != nil {
		return nil, fmt.Errorf("llm: read image %s: %w", img.Path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("llm: read image %s: %w", img.Path, err)
	}
	if len(data) > MaxImageBytes {
		return nil, fmt.Errorf("llm: image %s exceeds %d bytes", img.Path, MaxImageBytes)
	}
	return data, nil
}

// Placeholder formats a compact text representation of the image for
// summaries, logs and text-only fallbacks.
func (img Image) Placeholder() string {
	var parts []string
	media := img.MediaType
	if media == "" {
		media = "image"
	}
	parts = append(parts, media)
	if img.Width > 0 && img.Height > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", img.Width, img.Height))
	}
	if img.SHA256 != "" {
		short := img.SHA256
		if len(short) > 8 {
			short = short[:8]
		}
		parts = append(parts, "sha256:"+short)
	}
	return "[image: " + strings.Join(parts, " ") + "]"
}
