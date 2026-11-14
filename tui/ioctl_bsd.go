//go:build darwin

package tui

import "syscall"

// ioctl request numbers for terminal attribute access.
//
// Darwin uses the BSD ioctl interface (TIOCGETA / TIOCSETA) and the
// BSD termios layout. Linux uses a different pair of numbers and a
// different layout, so the two live in separate files.
const (
	getTermios = syscall.TIOCGETA
	setTermios = syscall.TIOCSETA
)
