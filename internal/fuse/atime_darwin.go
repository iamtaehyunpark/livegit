//go:build darwin

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
		return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
	}
	return info.ModTime()
}
