package sandbox

import (
	"context"
	"testing"
	"time"
)

// TestExecDoesNotHangOnAnOrphanedPipe reproduces the CI hang: a shell
// that leaves a grandchild holding the output pipe.
func TestExecDoesNotHangOnAnOrphanedPipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	// The subshell inherits stdout and sleeps well past the deadline.
	_, _, _ = Exec(ctx, "sleep 30 & echo started", DefaultProfile(t.TempDir()), t.TempDir())
	elapsed := time.Since(start)
	if elapsed > 14*time.Second {
		t.Fatalf("Exec waited for the orphaned grandchild: %s", elapsed)
	}
	t.Logf("returned after %s", elapsed)
}
