// Package livetest centralizes live-test gating for feature examples.
package livetest

import (
	"os"
	"strings"
	"testing"
)

// Skipped reports whether all live provider calls should be skipped.
func Skipped() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GRATEFUL_LIVE_TESTS")), "skip")
}

// Required reports whether missing live-test credentials should fail instead
// of skip. This is intended for live CI workflows that must prove secrets are
// configured.
func Required() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("GRATEFUL_LIVE_TESTS")))
	return v == "required" || v == "require"
}

// MissingCredential skips or fails the current test for an unavailable live
// provider credential, depending on Required().
func MissingCredential(t *testing.T, msg string) {
	t.Helper()
	if Required() {
		t.Fatal(msg)
	}
	t.Skip(msg)
}
