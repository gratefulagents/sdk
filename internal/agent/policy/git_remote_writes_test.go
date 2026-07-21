package policy

import "testing"

func TestNormalizeGitRemoteWrites(t *testing.T) {
	tests := []struct {
		input GitRemoteWrites
		want  GitRemoteWrites
	}{
		{"", GitRemoteWritesEnabled},
		{GitRemoteWritesEnabled, GitRemoteWritesEnabled},
		{GitRemoteWritesDisabled, GitRemoteWritesDisabled},
		{"unexpected", GitRemoteWritesDisabled},
	}
	for _, tt := range tests {
		if got := NormalizeGitRemoteWrites(tt.input); got != tt.want {
			t.Errorf("NormalizeGitRemoteWrites(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRuntimePolicyNormalizesGitRemoteWrites(t *testing.T) {
	if got := (RuntimePolicy{}).Normalize().GitRemoteWrites; got != GitRemoteWritesEnabled {
		t.Fatalf("zero-value GitRemoteWrites = %q, want %q", got, GitRemoteWritesEnabled)
	}
	if got := (RuntimePolicy{GitRemoteWrites: "unexpected"}).Normalize().GitRemoteWrites; got != GitRemoteWritesDisabled {
		t.Fatalf("invalid GitRemoteWrites = %q, want %q", got, GitRemoteWritesDisabled)
	}
}
