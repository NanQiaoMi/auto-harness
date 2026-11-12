//go:build darwin || linux

package tui

import "syscall"

// ioctl request numbers for terminal attribute access.
const (
	getTermios = syscall.TIOCGETA
	setTermios = syscall.TIOCSETA
)
