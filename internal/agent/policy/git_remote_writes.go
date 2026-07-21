package policy

import "strings"

// GitRemoteWrites controls whether tools may mutate Git remotes.
type GitRemoteWrites string

const (
	GitRemoteWritesEnabled  GitRemoteWrites = "enabled"
	GitRemoteWritesDisabled GitRemoteWrites = "disabled"
)

// NormalizeGitRemoteWrites resolves compatible defaults. Empty enables remote
// writes for backwards compatibility; unrecognized values fail closed.
func NormalizeGitRemoteWrites(value GitRemoteWrites) GitRemoteWrites {
	switch GitRemoteWrites(strings.TrimSpace(string(value))) {
	case GitRemoteWritesEnabled, "":
		return GitRemoteWritesEnabled
	case GitRemoteWritesDisabled:
		return GitRemoteWritesDisabled
	default:
		return GitRemoteWritesDisabled
	}
}
