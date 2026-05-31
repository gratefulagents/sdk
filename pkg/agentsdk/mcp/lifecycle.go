package mcp

import (
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// terminateProcess ensures the given exec.Cmd's process (and any subprocesses
// in its process group, on platforms that support it) is killed and reaped.
//
// It is safe to call with a nil cmd, a cmd that has not been started, or a
// cmd that has already exited. The grace duration controls how long we wait
// for the process to exit on its own before issuing SIGKILL.
func terminateProcess(cmd *exec.Cmd, grace time.Duration) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if cmd.ProcessState != nil {
		return nil // already reaped
	}

	if grace <= 0 {
		grace = 2 * time.Second
	}

	// Wait in a goroutine so we can race against the grace timer.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return ignoreExitErr(err)
	case <-time.After(grace):
	}

	// Hard kill: process group on Unix (configured via configureProcessGroup),
	// individual process otherwise.
	if killErr := killProcessGroup(cmd); killErr != nil {
		// Fall back to signaling the process directly.
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("kill mcp child: %w", err)
		}
	}

	select {
	case err := <-done:
		return ignoreExitErr(err)
	case <-time.After(2 * time.Second):
		return fmt.Errorf("mcp child %d did not exit after SIGKILL", cmd.Process.Pid)
	}
}

func ignoreExitErr(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Non-zero exit (including signal kill) is expected during cleanup.
		return nil
	}
	return err
}
