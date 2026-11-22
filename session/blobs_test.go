package session

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NanQiaoMi/auto-harness/llm"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	im.Set(0, 0, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSessionImageBlobPersistenceAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, "claude-sonnet-5-5")
	if err != nil {
		t.Fatal(err)
	}
	id := s.Meta.ID

	raw := testPNG(t, 8, 8)
	img, err := llm.ImageFromBytes(raw, "image/png", "ui.png")
	if err != nil {
		t.Fatal(err)
	}

	// Append the same image twice to verify SHA-256 deduplication.
	if err := s.Append(
		llm.Message{Role: llm.RoleUser, Content: "first", Images: []llm.Image{img}},
		llm.Message{Role: llm.RoleUser, Content: "second", Images: []llm.Image{img}},
	); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Verify JSONL contains no base64 image data.
	jsonlBytes, err := os.ReadFile(filepath.Join(dir, id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	if strings.Contains(string(jsonlBytes), b64) {
		t.Fatal("session JSONL must not embed raw base64 image data")
	}

	// Verify blob directory permissions (0700) and file permissions (0600), single file.
	bdir := filepath.Join(dir, "blobs")
	dirInfo, err := os.Stat(bdir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("blobs dir perm = %o, want 0700", perm)
	}
	entries, err := os.ReadDir(bdir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("blob count = %d, want 1 (deduplicated)", len(entries))
	}
	fileInfo, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("blob file perm = %o, want 0600", perm)
	}

	// Reload and confirm bytes round-trip cleanly from blob path.
	reloaded, err := Load(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()

	if len(reloaded.Messages) != 2 || len(reloaded.Messages[0].Images) != 1 {
		t.Fatalf("reloaded messages = %+v", reloaded.Messages)
	}
	gotImg := reloaded.Messages[0].Images[0]
	if len(gotImg.Data) != 0 {
		t.Errorf("reloaded image should not preload Data in JSONL")
	}
	gotBytes, err := gotImg.Bytes()
	if err != nil || !bytes.Equal(gotBytes, raw) {
		t.Fatalf("reloaded Bytes() mismatch: err=%v len=%d", err, len(gotBytes))
	}

	// Forking into a separate directory copies blobs so the fork is self-contained.
	forkDir := t.TempDir()
	forked, err := Fork(reloaded, ForkOptions{Dir: forkDir})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	defer forked.Close()
	if len(forked.Messages) != 2 || len(forked.Messages[0].Images) != 1 {
		t.Fatalf("forked messages = %+v", forked.Messages)
	}
	forkBlob := forked.Messages[0].Images[0].Path
	if !strings.HasPrefix(forkBlob, forkDir) {
		t.Errorf("fork blob path = %q, want under %q", forkBlob, forkDir)
	}
	forkBytes, err := forked.Messages[0].Images[0].Bytes()
	if err != nil || !bytes.Equal(forkBytes, raw) {
		t.Errorf("forked Bytes() mismatch: err=%v", err)
	}
}
