package shell

import (
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

// TestSafeCommandSubstitutionAllowed verifies that command substitution used to
// compute arguments for non-destructive programs is permitted. The recursive
// destructive/git checks already inspect the substitution body, and the
// substitution cannot turn a benign program destructive, so blocking these was
// a false positive that broke common read-only workflows.
func TestSafeCommandSubstitutionAllowed(t *testing.T) {
	allowed := []string{
		`wc -l $(find . -name '*.go')`,
		"cat $(rg -l foo)",
		"echo $(git rev-parse HEAD)",
		"head -n 5 $(ls -t | head -1)",
		"grep TODO $(git ls-files)",
		"diff <(sort a) <(sort b)",
	}
	for _, mode := range []policy.PermissionMode{policy.PermissionModeReadOnly, policy.PermissionModeWorkspaceWrite} {
		for _, cmd := range allowed {
			if blocked, reason := IsCommandBlockedForMode(mode, cmd); blocked {
				t.Errorf("IsCommandBlockedForMode(%s, %q) = blocked %q; want allowed (safe command substitution)", mode, cmd, reason)
			}
		}
	}
}

// TestCommandSubstitutionEvasionsBlocked ensures the relaxation did not reopen
// the flag/target-injection hole: substitution feeding a statically-checked
// destructive head (rm, chmod, dd, tee, mkfs, shell) stays blocked, and a
// destructive command inside the substitution is still caught by recursion.
func TestCommandSubstitutionEvasionsBlocked(t *testing.T) {
	blockedCmds := []string{
		"rm $(echo -rf) /",
		"rm `echo -rf` /",
		"chmod $(echo -R) 777 /",
		"dd if=/dev/zero of=$(echo /dev/sda)",
		"tee $(echo /etc/passwd)",
		"$(echo rm) -rf /",
		"$(echo rm -rf /)",
		"sh -c \"$(echo rm -rf /)\"",
	}
	for _, mode := range []policy.PermissionMode{policy.PermissionModeReadOnly, policy.PermissionModeWorkspaceWrite} {
		for _, cmd := range blockedCmds {
			if blocked, _ := IsCommandBlockedForMode(mode, cmd); !blocked {
				t.Errorf("IsCommandBlockedForMode(%s, %q) = allowed; want blocked (command-substitution evasion)", mode, cmd)
			}
		}
	}
}
