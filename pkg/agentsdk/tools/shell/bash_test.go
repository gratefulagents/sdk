package shell

import (
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

func TestBashToolForAccessDowngradesToReadOnly(t *testing.T) {
	t.Parallel()

	for _, tool := range []agentsdk.Tool{
		&BashTool{},
		&WorkspaceWriteBashTool{},
	} {
		adapter, ok := tool.(agentsdk.ToolAccessAdapter)
		if !ok {
			t.Fatalf("%T should implement ToolAccessAdapter", tool)
		}
		adapted := adapter.ToolForAccess(agentsdk.ToolAccessLevelReadOnly)
		if adapted == nil {
			t.Fatalf("%T ToolForAccess(read-only) returned nil", tool)
		}
		if adapted.Name() != "Bash" {
			t.Fatalf("adapted tool name = %q, want Bash", adapted.Name())
		}
		if !adapted.IsReadOnly() {
			t.Fatalf("adapted %T should be read-only", adapted)
		}
	}
}

func TestIsPushToProtectedBranchHandlesSpacingAndRefspecs(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{
		"git  push origin main",
		"git\tpush origin HEAD:main",
		"/usr/bin/git -C repo push origin +feature:refs/heads/master",
		"command git push origin master:release",
	} {
		if !IsPushToProtectedBranch(cmd) {
			t.Fatalf("IsPushToProtectedBranch(%q) = false, want true", cmd)
		}
	}

	if IsPushToProtectedBranch("git push origin feature:review") {
		t.Fatal("feature branch push should not be classified as protected")
	}
}

func TestCommandBlockedForModeHandlesGitPushSpacing(t *testing.T) {
	t.Parallel()

	blocked, reason := IsCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, "git  push origin HEAD:main")
	if !blocked || !strings.Contains(reason, "main/master") {
		t.Fatalf("blocked=%v reason=%q, want protected-branch block", blocked, reason)
	}

	blocked, reason = IsCommandBlockedForMode(policy.PermissionModeReadOnly, "git  push origin feature")
	if !blocked || !strings.Contains(reason, "git push") {
		t.Fatalf("blocked=%v reason=%q, want read-only git push block", blocked, reason)
	}
}

func TestCommandBlockedForModeRejectsLeadingAssignments(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{
		"X=1 git push origin main",
		"X=1 Y=2 gh pr merge 123",
		"X=1 env Y=2 git push origin master",
		"X=1 command gh issue create --title t",
		"BASH_ENV=./payload bash -c true",
		"GIT_CONFIG_COUNT=1 git status",
	} {
		blocked, _ := IsCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, cmd)
		if !blocked {
			t.Fatalf("IsCommandBlockedForMode(workspace-write, %q) = allowed; want block", cmd)
		}
	}
}

func TestCommandBlockedForModeBlocksGHCLI(t *testing.T) {
	t.Parallel()

	blockedCommands := []string{
		"gh pr merge 5 --squash",
		"gh api repos/acme/repo/pulls",
		"bash -c 'gh pr create --fill'",
		"/usr/local/bin/gh auth token",
		"echo body | gh issue create --title t --body-file -",
		"wc -l $(gh pr list)",
		"eval gh pr view 7",
		// Wrappers whose options take operands must not hide the gh head.
		"env -u GITHUB_TOKEN gh pr merge 5",
		"env FOO=bar --unset GH_TOKEN gh pr view 7",
		"env -S 'gh pr list'",
		"sudo -u deploy gh pr merge 5",
		"nice -n 10 gh api /user",
		"timeout 30 gh pr checks 5",
	}
	for _, mode := range []policy.PermissionMode{policy.PermissionModeReadOnly, policy.PermissionModeWorkspaceWrite} {
		for _, cmd := range blockedCommands {
			blocked, reason := IsCommandBlockedForMode(mode, cmd)
			if !blocked {
				t.Fatalf("IsCommandBlockedForMode(%v, %q) = false, %q; want block", mode, cmd, reason)
			}
			// Dynamic constructs fail earlier at the stricter static-authorization
			// boundary; direct invocations must report the gh policy.
			if !strings.Contains(cmd, "$(") && !strings.HasPrefix(cmd, "eval ") && !strings.Contains(cmd, "=") && !strings.Contains(reason, "gh CLI is not allowed") {
				t.Fatalf("IsCommandBlockedForMode(%v, %q) reason = %q; want gh CLI block", mode, cmd, reason)
			}
		}
	}

	for _, cmd := range []string{
		"echo gh",             // gh as argument, not an invocation
		"ghost --version",     // different binary
		"grep -rn gh_token .", // substring only
		"git status",
		"env -u GITHUB_TOKEN ls", // wrapper hiding a benign command
		"timeout 30 make test",
	} {
		if blocked, reason := IsCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, cmd); blocked {
			t.Fatalf("IsCommandBlockedForMode(workspace-write, %q) blocked: %q", cmd, reason)
		}
	}

	// The same operand-aware wrapper stripping must keep protecting git
	// policy: env/sudo/nice must not hide a protected-branch push.
	for _, cmd := range []string{
		"env -u GIT_ASKPASS git push origin main",
		"sudo -u deploy git push origin HEAD:master",
		"nice -n 5 git push origin main",
	} {
		blocked, reason := IsCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, cmd)
		if !blocked || !strings.Contains(reason, "main/master") {
			t.Fatalf("IsCommandBlockedForMode(workspace-write, %q) = %v, %q; want protected-branch block", cmd, blocked, reason)
		}
	}

	// gh side effects are remote, so the policy applies even when an
	// enforcing OS sandbox skips the destructive-command classifier.
	if blocked, _ := isCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, "gh pr merge 5", true); !blocked {
		t.Fatal("gh policy must apply even under an enforcing sandbox")
	}
	// Unrestricted mode keeps raw gh available.
	if blocked, reason := IsCommandBlockedForMode(policy.PermissionModeDangerFullAccess, "gh pr view 5"); blocked {
		t.Fatalf("gh blocked in danger-full-access mode: %q", reason)
	}
}

func TestCommandBlockedForModeHandlesRootRemoveVariants(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{"rm -fr /", "sudo rm -r /*"} {
		blocked, reason := IsCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, cmd)
		if !blocked || !strings.Contains(reason, "recursive removal") {
			t.Fatalf("IsCommandBlockedForMode(%q) = %v, %q; want root removal block", cmd, blocked, reason)
		}
	}
}

func TestCommandBlockedRejectsDynamicSyntaxWhenSandboxEnforces(t *testing.T) {
	t.Parallel()

	if blocked, _ := isCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, "echo $HOME", true); !blocked {
		t.Fatal("dynamic shell syntax must remain blocked under an enforcing sandbox")
	}
}

func TestCommandBlockedSkipsDestructiveClassifierWhenSandboxEnforces(t *testing.T) {
	t.Parallel()

	// With an enforcing OS sandbox the filesystem classifier is redundant and
	// must not block; git policy still applies because the sandbox cannot
	// contain remote-side effects.
	if blocked, reason := isCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, "rm -rf /", true); blocked {
		t.Fatalf("destructive classifier ran despite enforcing sandbox: %q", reason)
	}
	if blocked, _ := isCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, "git push origin main", true); !blocked {
		t.Fatal("git protected-branch policy must apply even under an enforcing sandbox")
	}
	if blocked, _ := isCommandBlockedForMode(policy.PermissionModeReadOnly, "git commit -m x", true); !blocked {
		t.Fatal("read-only git policy must apply even under an enforcing sandbox")
	}
}
