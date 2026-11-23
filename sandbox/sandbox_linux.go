//go:build linux

package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const helperArg = "__harness_sandbox_helper__"

const (
	// Landlock syscall numbers are identical across linux/amd64 and linux/arm64.
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	landlockCreateRulesetVersion = 1 << 0
	landlockRulePathBeneath      = 1

	landlockAccessFSExecute    uint64 = 1 << 0
	landlockAccessFSWriteFile  uint64 = 1 << 1
	landlockAccessFSReadFile   uint64 = 1 << 2
	landlockAccessFSReadDir    uint64 = 1 << 3
	landlockAccessFSRemoveDir  uint64 = 1 << 4
	landlockAccessFSRemoveFile uint64 = 1 << 5
	landlockAccessFSMakeChar   uint64 = 1 << 6
	landlockAccessFSMakeDir    uint64 = 1 << 7
	landlockAccessFSMakeReg    uint64 = 1 << 8
	landlockAccessFSMakeSock   uint64 = 1 << 9
	landlockAccessFSMakeFifo   uint64 = 1 << 10
	landlockAccessFSMakeBlock  uint64 = 1 << 11
	landlockAccessFSMakeSym    uint64 = 1 << 12
	landlockAccessFSRefer      uint64 = 1 << 13
	landlockAccessFSTruncate   uint64 = 1 << 14

	// O_PATH matches 010000000 on both amd64 and arm64 Linux.
	oPath = 0x200000
)

type landlockRulesetAttr struct {
	HandledAccessFS uint64
}

func init() {
	if len(os.Args) >= 5 && os.Args[1] == helperArg && os.Args[3] == "--" {
		if err := runSandboxHelper(os.Args[2], os.Args[4:]); err != nil {
			fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
			os.Exit(126)
		}
	}
}

func platformBackend() Backend {
	return landlockBackend{}
}

type landlockBackend struct{}

func (landlockBackend) Name() string { return "landlock" }

func (landlockBackend) Available() bool {
	return landlockABIVersion() >= 1
}

func (b landlockBackend) Command(ctx context.Context, name string, args []string, profile *Profile, workdir string) (*exec.Cmd, bool, error) {
	if profile == nil || !b.Available() {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = workdir
		return cmd, false, nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil, false, fmt.Errorf("sandbox: resolve executable: %w", err)
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		return nil, false, fmt.Errorf("sandbox: encode profile: %w", err)
	}
	argv := append([]string{helperArg, string(raw), "--", name}, args...)
	cmd := exec.CommandContext(ctx, self, argv...)
	cmd.Dir = workdir
	return cmd, true, nil
}

func landlockABIVersion() int {
	r0, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, landlockCreateRulesetVersion)
	if errno != 0 {
		return 0
	}
	return int(r0)
}

func runSandboxHelper(rawProfile string, argv []string) error {
	var p Profile
	if err := json.Unmarshal([]byte(rawProfile), &p); err != nil {
		return fmt.Errorf("decode profile: %w", err)
	}
	if err := applyLandlock(p); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	if !p.AllowNetwork {
		if err := denyNetworkSeccomp(); err != nil {
			return fmt.Errorf("seccomp: %w", err)
		}
	}
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("exec %s: %w", argv[0], err)
	}
	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", bin, err)
	}
	return nil
}

func applyLandlock(p Profile) error {
	abi := landlockABIVersion()
	if abi < 1 {
		return fmt.Errorf("kernel Landlock is unavailable")
	}

	writeAccess := landlockAccessFSWriteFile |
		landlockAccessFSRemoveDir |
		landlockAccessFSRemoveFile |
		landlockAccessFSMakeChar |
		landlockAccessFSMakeDir |
		landlockAccessFSMakeReg |
		landlockAccessFSMakeSock |
		landlockAccessFSMakeFifo |
		landlockAccessFSMakeBlock |
		landlockAccessFSMakeSym
	if abi >= 2 {
		writeAccess |= landlockAccessFSRefer
	}
	if abi >= 3 {
		writeAccess |= landlockAccessFSTruncate
	}

	readAccess := landlockAccessFSReadFile | landlockAccessFSReadDir
	if !p.DenyProcessExec {
		readAccess |= landlockAccessFSExecute
	}

	handled := writeAccess
	denied, ancestors, err := resolveDeniedSet(p.DenyReadPaths)
	if err != nil {
		return err
	}
	if len(denied) > 0 {
		handled |= readAccess
	}

	attr := landlockRulesetAttr{HandledAccessFS: handled}
	fd, _, errno := syscall.Syscall(
		sysLandlockCreateRuleset,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	rulesetFD := int(fd)
	defer syscall.Close(rulesetFD)

	// Allow writes to /dev/null so standard shell redirections work.
	if err := addPathRuleIfExists(rulesetFD, "/dev/null", writeAccess); err != nil {
		return err
	}
	for _, wp := range p.WritePaths {
		resolved, err := resolve(wp)
		if err != nil {
			return err
		}
		if err := addPathRuleIfExists(rulesetFD, resolved, writeAccess); err != nil {
			return err
		}
	}

	// If any read paths are denied, grant read access across every
	// existing sibling tree except the denied paths themselves.
	if len(denied) > 0 {
		for dir := range ancestors {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, ent := range entries {
				child := filepath.Join(dir, ent.Name())
				if denied[child] || ancestors[child] {
					continue
				}
				if err := addPathRuleIfExists(rulesetFD, child, readAccess); err != nil {
					return err
				}
			}
		}
	}

	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}
	if _, _, errno := syscall.Syscall(sysLandlockRestrictSelf, uintptr(rulesetFD), 0, 0); errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}
	return nil
}

func resolveDeniedSet(paths []string) (denied map[string]bool, ancestors map[string]bool, err error) {
	denied = map[string]bool{}
	ancestors = map[string]bool{}
	for _, raw := range paths {
		resolved, err := resolve(raw)
		if err != nil {
			return nil, nil, err
		}
		if _, statErr := os.Lstat(resolved); os.IsNotExist(statErr) {
			continue
		}
		denied[resolved] = true
		cur := filepath.Dir(resolved)
		for {
			ancestors[cur] = true
			if cur == "/" || cur == "." {
				break
			}
			next := filepath.Dir(cur)
			if next == cur {
				break
			}
			cur = next
		}
	}
	return denied, ancestors, nil
}

func addPathRuleIfExists(rulesetFD int, path string, access uint64) error {
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer syscall.Close(fd)

	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("fstat %s: %w", path, err)
	}
	isDir := (st.Mode & syscall.S_IFMT) == syscall.S_IFDIR
	if !isDir {
		fileMask := landlockAccessFSExecute | landlockAccessFSWriteFile | landlockAccessFSReadFile | landlockAccessFSTruncate
		access &= fileMask
	}
	if access == 0 {
		return nil
	}

	// struct landlock_path_beneath_attr is __attribute__((packed)): 12 bytes.
	var payload [12]byte
	binary.LittleEndian.PutUint64(payload[0:8], access)
	binary.LittleEndian.PutUint32(payload[8:12], uint32(int32(fd)))

	if _, _, errno := syscall.Syscall6(
		sysLandlockAddRule,
		uintptr(rulesetFD),
		landlockRulePathBeneath,
		uintptr(unsafe.Pointer(&payload[0])),
		0, 0, 0,
	); errno != 0 {
		return fmt.Errorf("landlock_add_rule(%s): %w", strings.TrimSpace(path), errno)
	}
	return nil
}
