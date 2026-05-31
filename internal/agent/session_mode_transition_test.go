package agent

import "testing"

func TestValidSessionModeTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from SessionMode
		to   SessionMode
		want bool
	}{
		{name: "chat to plan", from: SessionModeChat, to: SessionModePlan, want: true},
		{name: "plan to chat", from: SessionModePlan, to: SessionModeChat, want: true},
		{name: "idempotent chat", from: SessionModeChat, to: SessionModeChat, want: true},
		{name: "invalid source", from: "", to: SessionModeChat, want: false},
	}
	for _, tt := range tests {
		if got := ValidSessionModeTransition(tt.from, tt.to); got != tt.want {
			t.Fatalf("%s: ValidSessionModeTransition(%q, %q) = %v, want %v", tt.name, tt.from, tt.to, got, tt.want)
		}
	}
}
