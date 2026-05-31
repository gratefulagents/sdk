//go:build unix

package sandbox

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// sandboxKillGrace is how long the executor waits between SIGTERM-to-group
// and SIGKILL-to-group when a command times out. Children that fork or
// daemonize otherwise escape ctx-based cancellation, which only signals the
// leader.
const sandboxKillGrace = 3 * time.Second

func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func terminateProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
}

func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
}
