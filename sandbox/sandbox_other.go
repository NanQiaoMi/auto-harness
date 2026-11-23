//go:build !darwin && !linux

package sandbox

import (
	"context"
	"os/exec"
)

func platformBackend() Backend {
	return noopBackend{}
}

type noopBackend struct{}

func (noopBackend) Name() string    { return "none" }
func (noopBackend) Available() bool { return false }

func (noopBackend) Command(ctx context.Context, name string, args []string, profile *Profile, workdir string) (*exec.Cmd, bool, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workdir
	return cmd, false, nil
}
