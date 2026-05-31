//go:build !unix

package sandbox

import (
	"os"
	"os/exec"
	"time"
)

const sandboxKillGrace = 3 * time.Second

func configureProcessGroup(cmd *exec.Cmd) {}

func terminateProcessGroup(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}

func killProcessGroup(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}
