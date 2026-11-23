//go:build linux

package sandbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxBackendIsLandlock(t *testing.T) {
	if BackendName() != "landlock" {
		t.Fatalf("BackendName() = %q, want landlock", BackendName())
	}
}

func TestLinuxSandboxReadOnlyWritePathsEmpty(t *testing.T) {
	if !Available() {
		t.Skip("landlock unavailable on this kernel")
	}
	workdir := t.TempDir()
	target := filepath.Join(workdir, "readonly.txt")
	out, sandboxed, err := Exec(context.Background(), "echo x > "+target, Profile{
		WritePaths:   nil,
		AllowNetwork: true,
	}, workdir)
	if !sandboxed {
		t.Fatal("expected sandboxed = true")
	}
	if err == nil && !strings.Contains(strings.ToLower(out), "permission denied") {
		t.Fatalf("expected write to fail under empty WritePaths, got %q", out)
	}
}
