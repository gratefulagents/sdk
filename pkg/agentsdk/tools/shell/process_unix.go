//go:build unix

package shell

import (
	"os/exec"
	"syscall"
)

// configurePlatformProcAttrs places the child in its own process group so we
// can deliver SIGKILL to the whole tree (the bash invocation may spawn
// children of its own — e.g. `yes` in a pipeline — that would otherwise
// outlive cmd.Process.Kill()).
func configurePlatformProcAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	// Best effort: kill the entire process group. Fall back to the leader.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
