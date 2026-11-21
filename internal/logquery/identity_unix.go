//go:build !windows

package logquery

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity returns a stable device/inode pair used to detect rotation.
func fileIdentity(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino))
	}
	return ""
}
