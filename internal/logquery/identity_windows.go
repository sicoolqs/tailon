//go:build windows

package logquery

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity approximates a stable file identity on Windows so development
// builds and tests can detect replacement. Release artifacts target Linux.
func fileIdentity(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	if d, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return fmt.Sprintf("%d:%d:%d", d.CreationTime.HighDateTime, d.CreationTime.LowDateTime, d.FileSizeHigh)
	}
	return ""
}
