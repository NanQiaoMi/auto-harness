//go:build darwin

package sandbox

import (
	"context"
	"strings"
	"testing"
)

// TestPolicyOrderIsCorrect verifies the generated policy denies before
// it allows, since Seatbelt uses the last matching rule.
func TestPolicyOrderIsCorrect(t *testing.T) {
	policy, err := policyFor(Profile{
		WritePaths:    []string{"/repo"},
		DenyReadPaths: []string{"/repo/.ssh"},
	})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	denyWrite := strings.Index(policy, "(deny file-write*)")
	allowWrite := strings.Index(policy, "(allow file-write*")
	if denyWrite < 0 || allowWrite < 0 {
		t.Fatalf("policy is missing rules:\n%s", policy)
	}
	if denyWrite > allowWrite {
		t.Errorf("the deny rule must come before the allow rule:\n%s", policy)
	}
	if !strings.HasSuffix(strings.TrimSpace(policy), "(allow default)") {
		t.Errorf("the blanket allow must come last:\n%s", policy)
	}
	// The read denial must come before the blanket allow too.
	denyRead := strings.Index(policy, "(deny file-read*")
	if denyRead < 0 || denyRead > strings.Index(policy, "(allow default)") {
		t.Errorf("the read denial must come before the blanket allow:\n%s", policy)
	}
}

func TestDarwinCommandUsesSeatbelt(t *testing.T) {
	if !Available() {
		t.Skip("sandbox-exec unavailable")
	}
	dir := t.TempDir()
	profile := Profile{WritePaths: []string{dir}, AllowNetwork: true}
	cmd, sandboxed, err := Command(context.Background(), "/bin/echo", []string{"hi"}, &profile, dir)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if !sandboxed {
		t.Fatal("expected sandboxed = true on darwin")
	}
	if !strings.HasSuffix(cmd.Path, "sandbox-exec") {
		t.Errorf("path = %q, want sandbox-exec", cmd.Path)
	}
}
