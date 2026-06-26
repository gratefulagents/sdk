package shell

import (
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
)

// TestSafeDeviceRedirectsAllowed verifies that redirecting to standard safe
// pseudo-devices (notably the ubiquitous "2>/dev/null") is NOT blocked in
// restricted modes, while writes to real device files stay blocked.
func TestSafeDeviceRedirectsAllowed(t *testing.T) {
	allowed := []string{
		`find . -name '*.go' 2>/dev/null`,
		`grep -r foo . 2>/dev/null`,
		`rg --json pattern src 2>/dev/null | head`,
		`ls /nope >/dev/null 2>&1`,
		`echo hi >/dev/stderr`,
		`sed -n '1,40p' file.go 2>/dev/null`,
	}
	for _, mode := range []policy.PermissionMode{policy.PermissionModeReadOnly, policy.PermissionModeWorkspaceWrite} {
		for _, cmd := range allowed {
			if blocked, reason := IsCommandBlockedForMode(mode, cmd); blocked {
				t.Errorf("IsCommandBlockedForMode(%s, %q) = blocked %q; want allowed (safe device redirect)", mode, cmd, reason)
			}
		}
	}
}

// TestRealDeviceWritesStillBlocked ensures the /dev/null whitelist did not open
// a hole for destructive device writes.
func TestRealDeviceWritesStillBlocked(t *testing.T) {
	blockedCmds := []string{
		`echo x > /dev/sda`,
		`cat foo > /etc/passwd`,
		`echo 1 > /proc/sys/kernel/foo`,
		`echo x > /sys/class/leds/foo`,
	}
	for _, cmd := range blockedCmds {
		if blocked, _ := IsCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, cmd); !blocked {
			t.Errorf("IsCommandBlockedForMode(workspace-write, %q) = allowed; want blocked (protected write)", cmd)
		}
	}
}
