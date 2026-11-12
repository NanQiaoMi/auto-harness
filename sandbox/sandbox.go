// Package sandbox runs commands inside an operating-system sandbox.
//
// The permission layer (package perm) decides whether a call may run;
// this package decides what the command can touch once it runs.
//
// On macOS it uses sandbox-exec (Apple's Seatbelt), which restricts
// file access at the kernel level. On other platforms it falls back to
// running the command unchanged and reports that no sandbox is
// available, so the caller can decide what to do.
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Profile describes what a sandboxed command may access.
//
// The model is a write allowlist and a read denylist. An allowlist for
// writes is the practical choice: a command that may write anywhere can
// do anything. Reads are denied only for the listed sensitive paths,
// because a shell cannot even start when it may not read the system
// libraries.
type Profile struct {
	// WritePaths are the directories the command may write. An empty
	// list denies all writes.
	WritePaths []string
	// DenyReadPaths are the paths the command may not read.
	DenyReadPaths []string
	// AllowNetwork permits network access.
	AllowNetwork bool
	// DenyProcessExec denies spawning other processes. It is off by
	// default: most commands need to run other programs.
	DenyProcessExec bool
}

// DefaultProfile returns the profile used when nothing is configured:
// the working directory is writable, the usual credential locations are
// not readable, and the network is allowed.
func DefaultProfile(workdir string) Profile {
	return Profile{
		WritePaths:    []string{workdir},
		DenyReadPaths: defaultDenyReadPaths(),
		AllowNetwork:  true,
	}
}

// defaultDenyReadPaths lists the credential and key locations that a
// generated command should not be able to read.
func defaultDenyReadPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	var paths []string
	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".ssh"),
			filepath.Join(home, ".aws"),
			filepath.Join(home, ".gnupg"),
			filepath.Join(home, ".config", "gh"),
			filepath.Join(home, ".kube"),
			filepath.Join(home, ".docker"),
		)
	}
	return paths
}

// Available reports whether the platform supports sandboxing.
func Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

// Exec runs a command inside the sandbox.
//
// On a platform without sandbox support the command runs unchanged and
// the returned string says so, so the caller can surface the gap.
func Exec(ctx context.Context, command string, profile Profile, workdir string) (output string, sandboxed bool, err error) {
	if !Available() {
		out, runErr := runCommand(ctx, command, workdir)
		return out, false, runErr
	}
	policy, err := policyFor(profile)
	if err != nil {
		return "", false, err
	}
	cmd := exec.CommandContext(ctx, "sandbox-exec", "-p", policy, "/bin/bash", "-c", command)
	if workdir != "" {
		cmd.Dir = workdir
	}
	out, err := cmd.CombinedOutput()
	return string(out), true, err
}

// runCommand runs a command without sandboxing.
func runCommand(ctx context.Context, command, workdir string) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", command)
	if workdir != "" {
		cmd.Dir = workdir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// policyFor builds a Seatbelt policy for the profile.
//
// Seatbelt evaluates the rules in order and the last match wins, so the
// denials come first and the blanket allow comes last.
func policyFor(p Profile) (string, error) {
	var b strings.Builder
	b.WriteString("(version 1)\n")

	if !p.AllowNetwork {
		b.WriteString("(deny network*)\n")
	}
	if p.DenyProcessExec {
		b.WriteString("(deny process-exec*)\n")
	}

	// Reads: deny the sensitive paths, then allow everything else.
	for _, path := range p.DenyReadPaths {
		resolved, err := resolve(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", resolved)
	}

	// Writes: denied everywhere, then allowed for the listed paths.
	b.WriteString("(deny file-write*)\n")
	for _, path := range p.WritePaths {
		resolved, err := resolve(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", resolved)
	}

	b.WriteString("(allow default)\n")
	return b.String(), nil
}

// resolve turns a path into the canonical form Seatbelt needs.
//
// macOS resolves /tmp and /var to /private/tmp and /private/var; the
// sandbox matches on the canonical path, so the symlinks must be
// expanded.
func resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("sandbox: empty path")
	}
	expanded := path
	if strings.HasPrefix(expanded, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		expanded = filepath.Join(home, strings.TrimPrefix(expanded, "~"))
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	// Follow symlinks so the sandbox sees the real path.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// Describe returns a human-readable summary of the profile, for the
// /permissions output.
func (p Profile) Describe() string {
	var b strings.Builder
	b.WriteString("write: ")
	if len(p.WritePaths) == 0 {
		b.WriteString("none")
	} else {
		b.WriteString(strings.Join(p.WritePaths, ", "))
	}
	if len(p.DenyReadPaths) > 0 {
		b.WriteString("; deny read: " + strings.Join(p.DenyReadPaths, ", "))
	}
	if p.AllowNetwork {
		b.WriteString("; network: allowed")
	} else {
		b.WriteString("; network: denied")
	}
	return b.String()
}
