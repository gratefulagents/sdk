//go:build unix

package mcp

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestTerminateProcess_KillsLingeringChild(t *testing.T) {
	t.Parallel()

	// Spawn a sleep that ignores stdin closure so we can verify our explicit
	// Kill+Wait actually reaps it (this simulates an MCP child that doesn't
	// shut down gracefully when stdin closes).
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "sleep 30")
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Logf("started pid %d", pid)

	deadline := time.Now().Add(3 * time.Second)
	if err := terminateProcess(cmd, 200*time.Millisecond); err != nil {
		t.Fatalf("terminateProcess: %v", err)
	}
	if time.Now().After(deadline) {
		t.Fatalf("terminateProcess did not return promptly")
	}

	// Process must already be reaped — Wait should return immediately or have
	// returned. We check via cmd.ProcessState being non-nil after Wait inside
	// terminateProcess.
	if cmd.ProcessState == nil {
		t.Fatalf("ProcessState nil after terminateProcess; child not reaped")
	}
}

func TestTerminateProcess_NilCmdNoOp(t *testing.T) {
	t.Parallel()

	if err := terminateProcess(nil, time.Second); err != nil {
		t.Fatalf("nil cmd should be a no-op, got %v", err)
	}
}
