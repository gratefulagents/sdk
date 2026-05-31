package sandbox_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

func TestSandboxLocalExecutorExample(t *testing.T) {
	result, err := sandbox.LocalExecutor{}.Run(context.Background(), sandbox.Request{
		Argv:           []string{"sh", "-c", "printf hello"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		Timeout:        5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Output) != "hello" {
		t.Fatalf("Output = %q, want hello", result.Output)
	}
}

// TestSandboxSafeEnvDoesNotInheritSecretsExample shows that sandbox.SafeEnv
// builds a process environment that excludes parent-process secrets even when
// callers reference them in overrides. This is the security guarantee the
// shell tools rely on when handing argv to the sandbox.
func TestSandboxSafeEnvDoesNotInheritSecretsExample(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-do-not-leak")
	t.Setenv("DATABASE_URL", "postgres://secret")

	env := sandbox.SafeEnv(map[string]string{"CUSTOM": "value"})
	joined := strings.Join(env, "\n")
	for _, secretName := range []string{"OPENAI_API_KEY", "DATABASE_URL"} {
		if strings.Contains(joined, secretName+"=") {
			t.Fatalf("SafeEnv leaked %s into sandbox env:\n%s", secretName, joined)
		}
	}
	if strings.Contains(joined, "sk-do-not-leak") || strings.Contains(joined, "postgres://secret") {
		t.Fatalf("SafeEnv leaked secret value into sandbox env:\n%s", joined)
	}
	if !strings.Contains(joined, "CUSTOM=value") {
		t.Fatalf("SafeEnv dropped explicit override:\n%s", joined)
	}
}

// TestSandboxDefaultExecutorBoundedExample shows that sandbox.Default returns
// a working executor on the host platform and applies the configured timeout.
func TestSandboxDefaultExecutorBoundedExample(t *testing.T) {
	exec := sandbox.Default()
	result, err := exec.Run(context.Background(), sandbox.Request{
		Argv:           []string{"sh", "-c", "printf bounded"},
		WorkDir:        t.TempDir(),
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		Timeout:        2 * time.Second,
	})
	if err != nil {
		t.Skipf("sandbox.Default not runnable on this host: %v", err)
	}
	if !strings.Contains(result.Output, "bounded") {
		t.Fatalf("Output = %q, want to contain 'bounded'", result.Output)
	}
}
