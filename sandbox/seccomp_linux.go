//go:build linux

package sandbox

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	prSetNoNewPrivs   = 38
	prSetSeccomp      = 22
	seccompModeFilter = 2

	auditArchX86_64  uint32 = 0xc000003e
	auditArchAarch64 uint32 = 0xc00000b7

	sysSocketX86_64  uint32 = 41
	sysSocketAarch64 uint32 = 198

	afInet  uint32 = 2
	afInet6 uint32 = 10

	bpfLD  uint16 = 0x00
	bpfJMP uint16 = 0x05
	bpfRET uint16 = 0x06
	bpfW   uint16 = 0x00
	bpfABS uint16 = 0x20
	bpfJEQ uint16 = 0x10
	bpfK   uint16 = 0x00

	seccompRetKillProcess uint32 = 0x80000000
	seccompRetErrno       uint32 = 0x00050000
	seccompRetAllow       uint32 = 0x7fff0000
)

type sockFilter struct {
	Code uint16
	JT   uint8
	JF   uint8
	K    uint32
}

type sockFprog struct {
	Len    uint16
	Filter *sockFilter
}

func bpfStmt(code uint16, k uint32) sockFilter {
	return sockFilter{Code: code, K: k}
}

func bpfJump(code uint16, k uint32, jt, jf uint8) sockFilter {
	return sockFilter{Code: code, JT: jt, JF: jf, K: k}
}

// denyNetworkSeccomp installs a minimal classic-BPF seccomp filter that
// returns EACCES on socket(AF_INET, ...) and socket(AF_INET6, ...) while
// leaving AF_UNIX and all other syscalls allowed.
func denyNetworkSeccomp() error {
	var arch, sysSocket uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = auditArchX86_64
		sysSocket = sysSocketX86_64
	case "arm64":
		arch = auditArchAarch64
		sysSocket = sysSocketAarch64
	default:
		return fmt.Errorf("seccomp network filter unsupported on %s", runtime.GOARCH)
	}

	// seccomp_data layout:
	//   offset 0:  nr   (int)
	//   offset 4:  arch (__u32)
	//   offset 16: args[0] low 32 bits (domain on little-endian)
	filter := []sockFilter{
		// [0] load arch
		bpfStmt(bpfLD|bpfW|bpfABS, 4),
		// [1] verify arch; mismatch -> kill
		bpfJump(bpfJMP|bpfJEQ|bpfK, arch, 1, 0),
		// [2] kill
		bpfStmt(bpfRET|bpfK, seccompRetKillProcess),
		// [3] load syscall nr
		bpfStmt(bpfLD|bpfW|bpfABS, 0),
		// [4] if socket -> inspect domain, else allow
		bpfJump(bpfJMP|bpfJEQ|bpfK, sysSocket, 0, 4),
		// [5] load args[0] (domain)
		bpfStmt(bpfLD|bpfW|bpfABS, 16),
		// [6] if AF_INET -> deny
		bpfJump(bpfJMP|bpfJEQ|bpfK, afInet, 1, 0),
		// [7] if AF_INET6 -> deny, else allow
		bpfJump(bpfJMP|bpfJEQ|bpfK, afInet6, 0, 1),
		// [8] deny with EACCES
		bpfStmt(bpfRET|bpfK, seccompRetErrno|uint32(syscall.EACCES)),
		// [9] allow
		bpfStmt(bpfRET|bpfK, seccompRetAllow),
	}

	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}
	prog := sockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}
	if _, _, errno := syscall.Syscall6(
		syscall.SYS_PRCTL,
		prSetSeccomp,
		seccompModeFilter,
		uintptr(unsafe.Pointer(&prog)),
		0, 0, 0,
	); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_SECCOMP): %w", errno)
	}
	return nil
}
