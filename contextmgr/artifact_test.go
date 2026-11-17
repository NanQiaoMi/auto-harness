package contextmgr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileArtifactStoreWritesContentAndDeduplicates(t *testing.T) {
	dir := t.TempDir()
	store := NewFileArtifactStore(dir)

	first, err := store.Put(context.Background(), "tool-../../escape", "large output")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(first) {
		t.Fatalf("artifact path %q is not absolute", first)
	}
	if !strings.HasPrefix(first, dir+string(os.PathSeparator)) {
		t.Fatalf("artifact path %q escaped the store root %q", first, dir)
	}
	if strings.Contains(filepath.Base(first), "..") {
		t.Fatalf("artifact name %q was not sanitized", filepath.Base(first))
	}
	data, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "large output" {
		t.Fatalf("artifact content = %q", data)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode = %o, want 600", info.Mode().Perm())
	}

	second, err := store.Put(context.Background(), "tool-../../escape", "large output")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("identical writes used different paths: %q and %q", first, second)
	}
}

func TestFileArtifactStoreRequiresRootAndHonorsCancellation(t *testing.T) {
	if _, err := (&FileArtifactStore{}).Put(context.Background(), "x", "y"); err == nil {
		t.Fatal("a store without a root must fail")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewFileArtifactStore(t.TempDir()).Put(ctx, "x", "y"); err == nil {
		t.Fatal("a cancelled context must not write an artifact")
	}
}
