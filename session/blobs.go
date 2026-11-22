package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/NanQiaoMi/auto-harness/llm"
)

// blobsDir returns the content-addressed blob directory alongside a session file.
func blobsDir(sessionPath string) string {
	return filepath.Join(filepath.Dir(sessionPath), "blobs")
}

// persistImages writes each attached image to the session's blob store
// (0700 dir, 0600 files), deduplicated by SHA-256, and returns a copy
// of m whose Image entries reference the blob path with Data stripped so
// JSONL transcripts never bloat with base64.
func (s *Session) persistImages(m llm.Message) (llm.Message, error) {
	if len(m.Images) == 0 {
		return m, nil
	}
	dir := blobsDir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return m, fmt.Errorf("session: create blobs dir: %w", err)
	}
	persisted := make([]llm.Image, len(m.Images))
	for i, img := range m.Images {
		raw, err := img.Bytes()
		if err != nil {
			return m, fmt.Errorf("session: read image %d: %w", i, err)
		}
		if img.SHA256 == "" || img.MediaType == "" || img.ByteSize == 0 {
			norm, err := llm.ImageFromBytes(raw, img.MediaType, img.Label)
			if err != nil {
				return m, fmt.Errorf("session: validate image %d: %w", i, err)
			}
			img = norm
		}
		ext := llm.ExtForImageMIME(img.MediaType)
		if ext == "" {
			return m, fmt.Errorf("session: unsupported image type %q", img.MediaType)
		}
		blobPath := filepath.Join(dir, img.SHA256+ext)
		if _, err := os.Stat(blobPath); os.IsNotExist(err) {
			if err := writeBlobAtomic(dir, blobPath, raw); err != nil {
				return m, fmt.Errorf("session: write image blob: %w", err)
			}
		} else if err != nil {
			return m, fmt.Errorf("session: stat image blob: %w", err)
		}
		img.Path = blobPath
		img.Data = nil
		persisted[i] = img
	}
	m.Images = persisted
	return m, nil
}

func writeBlobAtomic(dir, dst string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".blob-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}
