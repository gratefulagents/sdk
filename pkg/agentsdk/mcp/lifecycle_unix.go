//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup arranges for cmd to be started in its own process
// group so killProcessGroup can take down any forked children together.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGKILL to the entire process group rooted at the
// command's PID. Returns an error if the kill could not be issued.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		// Fall back to killing just the process by negating its pid only if
		// it's the leader of its own group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}
