//go:build unix

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the child in its own process group so that a
// single signal can stop the command and all processes in its pipeline.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessGroup asks the whole process group to stop.
func terminateProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGTERM)
}

// killProcessGroup forcibly terminates the whole process group.
func killProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

func isProcessGoneError(err error) bool {
	return err != nil && (errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone))
}
