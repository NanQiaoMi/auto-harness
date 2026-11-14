//go:build darwin

package tui

import (
	"syscall"
	"unsafe"
)

// terminalSize queries the terminal size via an ioctl.
func terminalSize(fd int) (int, int, error) {
	var dims [2]uint16
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&dims)),
	)
	if errno != 0 {
		return 0, 0, errno
	}
	return int(dims[0]), int(dims[1]), nil
}

// termios mirrors the Darwin/BSD termios struct layout.
type termios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Cc     [20]uint8
	Ispeed uint32
	Ospeed uint32
}

// setRawMode switches the terminal into raw mode (no echo, no line
// buffering) and back.
//
// The terminal is restored to the state it was in when the mode is
// turned off, so a run that ends leaves the shell usable.
func setRawMode(fd int, on bool) error {
	var old termios
	if _, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(getTermios),
		uintptr(unsafe.Pointer(&old)),
	); errno != 0 {
		return errno
	}
	if !on {
		_, _, errno := syscall.Syscall(
			syscall.SYS_IOCTL,
			uintptr(fd),
			uintptr(setTermios),
			uintptr(unsafe.Pointer(&old)),
		)
		return errno
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(setTermios),
		uintptr(unsafe.Pointer(&raw)),
	)
	return errno
}
