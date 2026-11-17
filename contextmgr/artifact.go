package contextmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ArtifactStore persists content that is too large to keep in the model
// context. The returned string is a stable location the model can read
// back with the regular read tool.
type ArtifactStore interface {
	Put(ctx context.Context, name, content string) (string, error)
}

// FileArtifactStore writes artifacts into a directory.
//
// Writes are content-addressed, so the same tool output is stored once
// even when a request is rebuilt repeatedly.
type FileArtifactStore struct {
	Root string
}

// NewFileArtifactStore creates a store rooted at dir.
func NewFileArtifactStore(dir string) *FileArtifactStore {
	return &FileArtifactStore{Root: dir}
}

// Directory reports the store's root, for diagnostics.
func (s *FileArtifactStore) Directory() string {
	if s == nil {
		return ""
	}
	return s.Root
}

// Put writes content to the store and returns the absolute path.
func (s *FileArtifactStore) Put(ctx context.Context, name, content string) (string, error) {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return "", fmt.Errorf("contextmgr: artifact store has no root")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return "", fmt.Errorf("contextmgr: create artifact dir: %w", err)
	}
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])[:20]
	base := sanitizeArtifactName(name)
	path := filepath.Join(s.Root, hash+"-"+base+".txt")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	tmp, err := os.CreateTemp(s.Root, ".artifact-*")
	if err != nil {
		return "", fmt.Errorf("contextmgr: create artifact: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return "", fmt.Errorf("contextmgr: write artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("contextmgr: close artifact: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", fmt.Errorf("contextmgr: chmod artifact: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("contextmgr: publish artifact: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil
	}
	return abs, nil
}

func sanitizeArtifactName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "artifact"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= 48 {
			break
		}
	}
	out := strings.Trim(b.String(), "-.")
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	if out == "" {
		return "artifact"
	}
	return out
}
