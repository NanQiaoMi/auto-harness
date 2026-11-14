//go:build linux

package tui

import (
	"syscall"
	"unsafe"
)

// terminalSize queries the terminal size via an ioctl.
//
// The ioctl number is the same on Linux and Darwin; the difference
// lies in the termios layout, which is why the raw-mode code is
// separate.
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

// The Linux termios constants.
//
// Go's syscall package does not export the termios ones on Linux, so
// they come from the kernel's termios.h: NCCS control characters, and
// VMIN and VTIME among them (the minimum number of bytes and the
// timeout, in tenths of a second).
const (
	nccs  = 32
	vmin  = 6
	vtime = 5
)

// termios mirrors the Linux termios struct layout.
//
// The control characters come first (NCCS of them), then the four
// flags, then the speeds and the buffer sizes. The order matters: the
// kernel reads this struct as bytes, so a layout from another system
// would put the flags in the wrong place.
type termios struct {
	Cc     [nccs]uint8
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
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
	raw.Cc[vmin] = 1
	raw.Cc[vtime] = 0
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(setTermios),
		uintptr(unsafe.Pointer(&raw)),
	)
	return errno
}
