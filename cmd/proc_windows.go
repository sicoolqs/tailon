//go:build windows

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Windows has no POSIX process groups. The helper functions keep the cmd
// package buildable and testable on Windows; the released binaries target
// Unix-like systems where process-group termination is available.
func configureProcessGroup(cmd *exec.Cmd) {}

func terminateProcessGroup(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

func killProcessGroup(pid int) error {
	return terminateProcessGroup(pid)
}

func isProcessGoneError(err error) bool {
	const errorInvalidParameter syscall.Errno = 87
	return err != nil && (errors.Is(err, os.ErrProcessDone) ||
		errors.Is(err, syscall.ERROR_ACCESS_DENIED) ||
		errors.Is(err, errorInvalidParameter))
}
