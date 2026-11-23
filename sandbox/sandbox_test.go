package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
//
// The test is self-contained: it listens on a loopback port and asks nc
// to connect to it, so the result does not depend on the machine having
// a network, a DNS server, or an outside host that answers. The same
// connection is made twice — once with network denied, once allowed —
// so a failure to connect for any other reason cannot pass as a
// successful denial.
//
// Every exec gets a deadline. A denied socket can hang the command
// instead of failing it (the old version used ping with no timeout and
// hung a CI run for ten minutes), and a hung test is worse than a
// failing one: it says nothing about what went wrong.
func TestSandboxBlocksNetwork(t *testing.T) {
	if !Available() {
		t.Skip("no sandbox available on this platform")
	}
	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc is not available to probe the connection")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split address: %v", err)
	}

	dir := t.TempDir()
	probe := func(allowNetwork bool) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, _, err := Exec(ctx, "nc -z -w 2 "+host+" "+port, Profile{
			WritePaths:   []string{dir},
			AllowNetwork: allowNetwork,
		}, dir)
		if ctx.Err() != nil {
			t.Fatalf("the probe did not finish within the deadline (allowNetwork=%v)", allowNetwork)
		}
		return out, err
	}

	// The control: with the network allowed the same probe succeeds, so
	// a refused connection below is the sandbox's doing.
	if out, err := probe(true); err != nil {
		t.Fatalf("the control connection failed: %v (%q)", err, out)
	}
	out, err := probe(false)
	if err == nil {
		t.Errorf("the connection succeeded with the network denied: %q", out)
	}
	if strings.Contains(out, "succeeded") {
		t.Errorf("the network was reachable: %q", out)
	}
}

// TestCommandConfinesAProcess checks the entry point the extension
// runtime uses: it must return a confined command when a profile is
// given, and an unconfined one when it is not.
func TestCommandConfinesAProcess(t *testing.T) {
	dir := t.TempDir()
	profile := Profile{WritePaths: []string{dir}, AllowNetwork: true}

	cmd, sandboxed, err := Command(context.Background(), "/bin/echo", []string{"hi"}, &profile, dir)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if sandboxed != Available() {
		t.Errorf("sandboxed = %v, want %v", sandboxed, Available())
	}
	if cmd.Dir != dir {
		t.Errorf("dir = %q, want %q", cmd.Dir, dir)
	}
	if Available() {
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if strings.TrimSpace(string(out)) != "hi" {
			t.Errorf("output = %q", out)
		}
	}

	// No profile means no confinement: a caller that wants a plain
	// child must not get one silently wrapped.
	plain, sandboxed, err := Command(context.Background(), "/bin/echo", []string{"hi"}, nil, dir)
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if sandboxed {
		t.Error("a nil profile produced a sandboxed command")
	}
	if strings.HasSuffix(plain.Path, "sandbox-exec") {
		t.Errorf("path = %q, want the command itself", plain.Path)
	}
}

type fakeBackend struct{}

func (fakeBackend) Name() string    { return "fake-container" }
func (fakeBackend) Available() bool { return true }
func (fakeBackend) Command(ctx context.Context, name string, args []string, profile *Profile, workdir string) (*exec.Cmd, bool, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workdir
	return cmd, true, nil
}

func TestSetBackendPluggability(t *testing.T) {
	orig := BackendName()
	if orig == "" {
		t.Fatal("BackendName() must not be empty")
	}
	SetBackend(fakeBackend{})
	defer SetBackend(nil)

	if BackendName() != "fake-container" {
		t.Errorf("BackendName() = %q, want fake-container", BackendName())
	}
	if !Available() {
		t.Error("Available() = false with fakeBackend")
	}
	_, sandboxed, err := Command(context.Background(), "/bin/echo", []string{"ok"}, &Profile{}, t.TempDir())
	if err != nil || !sandboxed {
		t.Errorf("Command() = sandboxed:%v err:%v, want true, nil", sandboxed, err)
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
