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

func TestCommandBlockedForModeHandlesRootRemoveVariants(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{"rm -fr /", "sudo rm -r /*"} {
		blocked, reason := IsCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, cmd)
		if !blocked || !strings.Contains(reason, "recursive removal") {
			t.Fatalf("IsCommandBlockedForMode(%q) = %v, %q; want root removal block", cmd, blocked, reason)
		}
	}
}
