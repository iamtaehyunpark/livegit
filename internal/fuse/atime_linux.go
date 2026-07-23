//go:build linux

package fuse

import (
	"os"
	"syscall"
	"time"
)

// atimeOf returns a file's last-access time (falls back to mtime if the
// platform stat shape is unavailable).
func atimeOf(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Atim.Sec, st.Atim.Nsec)
	}
	return info.ModTime()
}
