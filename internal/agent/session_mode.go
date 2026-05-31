package agent

import (
	"strings"
)

// SessionMode is an SDK-native conversation mode.
type SessionMode string

const (
	SessionModeChat SessionMode = "chat"
	SessionModePlan SessionMode = "plan"
)

// NormalizeSessionMode normalizes session mode strings to known values.
func NormalizeSessionMode(mode string) SessionMode {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case string(SessionModeChat):
		return SessionModeChat
	case string(SessionModePlan):
		return SessionModePlan
	default:
		return ""
	}
}

// ValidSessionModeTransition checks if a session mode transition is valid.
// Plan and chat are bidirectional — the toggle always succeeds.
func ValidSessionModeTransition(from, to SessionMode) bool {
	from = NormalizeSessionMode(string(from))
	to = NormalizeSessionMode(string(to))
	if from == "" || to == "" {
		return false
	}
	return true
}
