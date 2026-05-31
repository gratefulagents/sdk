//go:build !linux

package pathutil

import "os"

// openInWorkspace falls back to canonicalize-then-open on non-Linux platforms.
//
// Residual risk: between ResolveWorkspace (which canonicalizes via
// EvalSymlinks) and OpenFileNoFollow (which refuses a symlink in the final
// path component via O_NOFOLLOW), an attacker with the ability to swap a
// parent directory for a symlink could cause the eventual open to traverse
// outside the workspace. Linux mitigates this with openat2 + RESOLVE_BENEATH;
// macOS and other Unixes do not currently expose an equivalent atomic primitive
// that ships with golang.org/x/sys, so this fallback is the documented best
// effort. Callers on those platforms should treat this as an additional reason
// to validate workspace inputs and avoid running with attacker-controlled
// directory contents.
func openInWorkspace(workDir, relPath string, flag int, perm os.FileMode) (*os.File, error) {
	resolved, err := ResolveWorkspace(workDir, relPath)
	if err != nil {
		return nil, err
	}
	return OpenFileNoFollow(resolved, flag, perm)
}
