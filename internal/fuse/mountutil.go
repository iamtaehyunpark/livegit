package fuse

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/taehyun/lg/internal/logx"
)

// IsStaleMount reports whether path is a FUSE mount whose server has died,
// leaving the mount orphaned. Touching such a path fails with ENXIO ("device
// not configured") on macOS or ENOTCONN on Linux. This happens when a previous
// `lg shell` was killed without unmounting (e.g. a crash or closed terminal).
func IsStaleMount(path string) bool {
	_, err := os.ReadDir(path)
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ENXIO) || errors.Is(err, syscall.ENOTCONN)
}

// ForceUnmount tears down a mount at path (used for cleanup and `lg unmount`).
func ForceUnmount(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("umount", "-f", path)
	default: // linux
		// fusermount is the right tool for FUSE; fall back to umount.
		if _, err := exec.LookPath("fusermount"); err == nil {
			cmd = exec.Command("fusermount", "-u", path)
		} else {
			cmd = exec.Command("umount", path)
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(string(out))
	}
	return nil
}

// RecoverStaleMount clears a stale mount at path if present, so a fresh
// `lg shell` can mount cleanly. Returns true if it recovered one.
func RecoverStaleMount(path string) (bool, error) {
	if !IsStaleMount(path) {
		return false, nil
	}
	logx.For("fuse").Warn("found stale mount; cleaning up", "path", path)
	if err := ForceUnmount(path); err != nil {
		return false, err
	}
	return true, nil
}

// IsNonEmptyDir reports whether path exists and contains entries. Used to warn
// before mounting over a directory that already has files (they'd be hidden
// while the mount is active).
func IsNonEmptyDir(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	return len(entries) > 0
}
