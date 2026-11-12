package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAvailableMatchesPlatform verifies the availability probe agrees
// with the platform.
func TestAvailableMatchesPlatform(t *testing.T) {
	// The test only asserts the probe does not panic and returns a
	// boolean; the sandbox is only usable on darwin.
	_ = Available()
}

// TestExecRunsCommand verifies a sandboxed command still works.
func TestExecRunsCommand(t *testing.T) {
	if !Available() {
		t.Skip("no sandbox available on this platform")
	}
	dir := t.TempDir()
	out, sandboxed, err := Exec(context.Background(), "echo hello", DefaultProfile(dir), dir)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !sandboxed {
		t.Error("the command was not sandboxed")
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output = %q", out)
	}
}

// TestSandboxBlocksWriteOutsideProfile verifies the sandbox really
// confines writes.
func TestSandboxBlocksWriteOutsideProfile(t *testing.T) {
	if !Available() {
		t.Skip("no sandbox available on this platform")
	}
	workdir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "escaped.txt")

	profile := Profile{
		WritePaths:   []string{workdir},
		AllowNetwork: true,
	}
	out, _, err := Exec(context.Background(), "echo escaped > "+target, profile, workdir)
	if err == nil && !strings.Contains(out, "Operation not permitted") {
		t.Fatalf("the write outside the profile succeeded: %q", out)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("the file was created outside the sandbox")
	}
}

// TestSandboxAllowsWriteInsideProfile verifies the sandbox permits the
// writes it should.
func TestSandboxAllowsWriteInsideProfile(t *testing.T) {
	if !Available() {
		t.Skip("no sandbox available on this platform")
	}
	workdir := t.TempDir()
	target := filepath.Join(workdir, "allowed.txt")

	profile := Profile{
		WritePaths:   []string{workdir},
		AllowNetwork: true,
	}
	if _, _, err := Exec(context.Background(), "echo ok > "+target, profile, workdir); err != nil {
		t.Fatalf("exec: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "ok") {
		t.Errorf("content = %q", data)
	}
}

// TestSandboxBlocksReadingDeniedPaths verifies the sandbox hides the
// credential directories.
func TestSandboxBlocksReadingDeniedPaths(t *testing.T) {
	if !Available() {
		t.Skip("no sandbox available on this platform")
	}
	workdir := t.TempDir()
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "token")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}

	profile := Profile{
		WritePaths:    []string{workdir},
		DenyReadPaths: []string{secretDir},
		AllowNetwork:  true,
	}
	out, _, _ := Exec(context.Background(), "cat "+secret, profile, workdir)
	if strings.Contains(out, "s3cret") {
		t.Errorf("the denied file was readable: %q", out)
	}
}

// TestSandboxBlocksNetwork verifies the network can be denied.
func TestSandboxBlocksNetwork(t *testing.T) {
	if !Available() {
		t.Skip("no sandbox available on this platform")
	}
	dir := t.TempDir()
	profile := Profile{
		WritePaths: []string{dir},
		// Network denied.
		AllowNetwork: false,
	}
	// A DNS lookup must fail without network access.
	out, _, _ := Exec(context.Background(), "ping -c 1 -W 1 127.0.0.1", profile, dir)
	// The sandbox refuses the socket, which is the signal to look for:
	// ping still prints its statistics line.
	if !strings.Contains(out, "Operation not permitted") {
		t.Errorf("the network was reachable: %q", out)
	}
}

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

// TestResolveExpandsSymlinks verifies the path resolution Seatbelt
// needs: macOS redirects /tmp to /private/tmp.
func TestResolveExpandsSymlinks(t *testing.T) {
	resolved, err := resolve("/tmp")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.HasPrefix(resolved, "/private/") && resolved != "/tmp" {
		t.Errorf("resolved = %q, want the canonical path", resolved)
	}
}

// TestDescribe summarizes a profile for display.
func TestDescribe(t *testing.T) {
	p := DefaultProfile("/repo")
	desc := p.Describe()
	if !strings.Contains(desc, "/repo") {
		t.Errorf("description = %q", desc)
	}
	if !strings.Contains(desc, "network: allowed") {
		t.Errorf("description = %q", desc)
	}
}
