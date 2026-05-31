package agentsdk

import "strings"

// IsSessionModeSlashCommand reports whether a user message is a control slash
// command for session-mode transitions rather than normal chat input.
func IsSessionModeSlashCommand(message string) bool {
	msg := strings.ToLower(strings.TrimSpace(message))
	switch {
	case msg == "/plan", msg == "/chat", msg == "/stop":
		return true
	case strings.HasPrefix(msg, "/mode "):
		return true
	default:
		return false
	}
}
