//go:build unix

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

// TestMain doubles as a helper-process entry point for
// TestRunBuiltCommandKillsDaemonizedChildOnTimeout. Two phases:
//   - SANDBOX_TEST_DAEMONIZE_MARKER set: spawn a grandchild via exec, then sleep
//     long. The grandchild inherits the leader's process group. The parent
//     (this process) is the leader spawned by the test.
//   - SANDBOX_TEST_DAEMONIZE_GRANDCHILD set: record pid then sleep long.
func TestMain(m *testing.M) {
	if marker := os.Getenv("SANDBOX_TEST_DAEMONIZE_GRANDCHILD"); marker != "" {
		_ = os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if marker := os.Getenv("SANDBOX_TEST_DAEMONIZE_MARKER"); marker != "" {
		runDaemonizeHelper(marker)
		return
	}
	os.Exit(m.Run())
}

func runDaemonizeHelper(marker string) {
	exe, err := os.Executable()
	if err != nil {
		os.Exit(3)
	}
	cmd := exec.Command(exe)
	cmd.Env = []string{"SANDBOX_TEST_DAEMONIZE_GRANDCHILD=" + marker}
	devnull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if devnull != nil {
		cmd.Stdin = devnull
		cmd.Stdout = devnull
		cmd.Stderr = devnull
	}
	if err := cmd.Start(); err != nil {
		os.Exit(4)
	}
	// Do not wait; let the grandchild run independently in the same process group.
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestRunBuiltCommandKillsDaemonizedChildOnTimeout(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "child.pid")

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	_, err = LocalExecutor{Config: Config{ExtraEnv: map[string]string{"SANDBOX_TEST_DAEMONIZE_MARKER": marker}}}.Run(context.Background(), Request{
		Argv:           []string{exe},
		WorkDir:        tmp,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		Timeout:        500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	pidDeadline := time.Now().Add(2 * time.Second)
	var pidStr string
	for time.Now().Before(pidDeadline) {
		b, readErr := os.ReadFile(marker)
		if readErr == nil && len(strings.TrimSpace(string(b))) > 0 {
			pidStr = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pidStr == "" {
		t.Fatal("grandchild never recorded its pid")
	}
	pid, convErr := strconv.Atoi(pidStr)
	if convErr != nil {
		t.Fatalf("parse pid %q: %v", pidStr, convErr)
	}

	// Process must be reaped within timeout (500ms) + grace (~3s) + slack.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("grandchild pid=%d survived past timeout+grace; process group not killed", pid)
}
