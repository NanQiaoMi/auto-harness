//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func platformBackend() Backend {
	return seatbeltBackend{}
}

type seatbeltBackend struct{}

func (seatbeltBackend) Name() string { return "seatbelt" }

func (seatbeltBackend) Available() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

func (b seatbeltBackend) Command(ctx context.Context, name string, args []string, profile *Profile, workdir string) (*exec.Cmd, bool, error) {
	if profile == nil || !b.Available() {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = workdir
		return cmd, false, nil
	}
	policy, err := policyFor(*profile)
	if err != nil {
		return nil, false, err
	}
	argv := append([]string{"-p", policy, name}, args...)
	cmd := exec.CommandContext(ctx, "sandbox-exec", argv...)
	cmd.Dir = workdir
	return cmd, true, nil
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
