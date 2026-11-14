//go:build linux

package tui

import "syscall"

// ioctl request numbers for terminal attribute access.
//
// Linux uses the termios interface (TCGETS / TCSETS) rather than the
// BSD one, and the termios struct has a different layout — the
// control characters come first and the speed fields are at the end.
// The two systems therefore need separate implementations.
const (
	getTermios = syscall.TCGETS
	setTermios = syscall.TCSETS
)
