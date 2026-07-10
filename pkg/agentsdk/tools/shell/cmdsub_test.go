package shell

import (
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

func TestDynamicShellSyntaxBlockedInRestrictedModes(t *testing.T) {
	commands := []string{
		`wc -l $(find . -name '*.go')`,
		"cat `rg -l foo`",
		"printf '%s' \"$HOME\"",
		"diff <(sort a) <(sort b)",
		"cat <<< data",
		"cat <<EOF\ndata\nEOF",
		"eval 'git status'",
		"source script.sh",
		"alias x=ls",
		"function x { true; }",
		"x() { true; }",
	}
	for _, mode := range []policy.PermissionMode{policy.PermissionModeReadOnly, policy.PermissionModeWorkspaceWrite} {
		for _, cmd := range commands {
			if blocked, _ := IsCommandBlockedForMode(mode, cmd); !blocked {
				t.Errorf("IsCommandBlockedForMode(%s, %q) = allowed; want dynamic syntax blocked", mode, cmd)
			}
		}
	}
	for _, cmd := range commands {
		if blocked, reason := IsCommandBlockedForMode(policy.PermissionModeDangerFullAccess, cmd); blocked {
			t.Errorf("danger-full-access blocked %q: %s", cmd, reason)
		}
	}
}

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
