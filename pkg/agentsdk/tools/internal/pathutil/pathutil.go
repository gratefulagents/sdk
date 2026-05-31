package pathutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Resolve resolves tool input paths relative to the agent work directory.
// Absolute paths are returned as-is.
func Resolve(workDir, inputPath string) string {
	if filepath.IsAbs(inputPath) {
		return filepath.Clean(inputPath)
	}
	return filepath.Clean(filepath.Join(workDir, inputPath))
}

// ResolveWorkspace resolves a path and rejects paths outside the workspace.
func ResolveWorkspace(workDir, inputPath string) (string, error) {
	if strings.TrimSpace(workDir) == "" {
		return "", fmt.Errorf("workspace root is required")
	}

	baseAbs, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolving workspace root: %w", err)
	}
	base, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return "", fmt.Errorf("resolving workspace root symlinks: %w", err)
	}

	resolvedPath, err := filepath.Abs(Resolve(baseAbs, inputPath))
	if err != nil {
		return "", fmt.Errorf("resolving workspace path: %w", err)
	}
	checkedPath, err := resolveExistingPath(resolvedPath)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path symlinks: %w", err)
	}

	rel, err := filepath.Rel(base, checkedPath)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf(
			"path %s is outside the workspace root %s - use a relative path like %q instead",
			inputPath, workDir, SuggestRelative(inputPath),
		)
	}
	return checkedPath, nil
}

// ReadFileNoFollow reads a file after refusing a symlink in the final path
// component. Callers should first resolve and validate workspace membership.
func ReadFileNoFollow(path string) ([]byte, os.FileInfo, error) {
	return ReadFileNoFollowLimit(path, 0)
}

// ReadFileNoFollowLimit behaves like ReadFileNoFollow but rejects regular files
// whose size exceeds maxBytes before reading their contents into memory. A
// maxBytes of zero or less disables the size check.
func ReadFileNoFollowLimit(path string, maxBytes int64) ([]byte, os.FileInfo, error) {
	f, err := OpenFileNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		return nil, nil, fmt.Errorf("%s is too large (%d bytes, limit %d)", path, info.Size(), maxBytes)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

// WriteFileNoFollow writes a file after refusing a symlink in the final path
// component. Callers should first resolve and validate workspace membership.
func WriteFileNoFollow(path string, data []byte, perm os.FileMode) error {
	f, err := OpenFileNoFollow(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// OpenFileNoFollow opens a file after refusing a symlink in the final path
// component. Platforms with O_NOFOLLOW use the kernel flag; other platforms
// use an Lstat preflight so the package still compiles and fails closed for
// ordinary symlink attacks.
func OpenFileNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return openFileNoFollow(path, flag, perm)
}

// OpenInWorkspace opens a file located beneath workDir without exposing a
// canonicalize-then-open TOCTOU window.
//
// On Linux the implementation uses openat2(2) with RESOLVE_BENEATH and
// RESOLVE_NO_SYMLINKS so the kernel atomically refuses any symlink (final or
// intermediate) and any path that escapes the workspace, eliminating the
// race between resolution and open.
//
// On macOS and other non-Linux platforms openat2 is unavailable; the function
// falls back to ResolveWorkspace + OpenFileNoFollow. That fallback fully
// canonicalizes the path through EvalSymlinks before opening and rejects a
// symlink in the final component via O_NOFOLLOW, but a determined attacker
// who can swap a parent directory between resolve and open could still cause
// the open to follow a different path than was validated. This residual TOCTOU
// risk is documented and accepted on those platforms; callers on Linux get
// stronger guarantees automatically.
func OpenInWorkspace(workDir, relPath string, flag int, perm os.FileMode) (*os.File, error) {
	if strings.TrimSpace(workDir) == "" {
		return nil, fmt.Errorf("workspace root is required")
	}
	return openInWorkspace(workDir, relPath, flag, perm)
}

func resolveExistingPath(path string) (string, error) {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	for parent := clean; ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				if resolved, evalErr := filepath.EvalSymlinks(parent); evalErr == nil {
					rel, relErr := filepath.Rel(parent, clean)
					if relErr != nil {
						return "", relErr
					}
					return filepath.Clean(filepath.Join(resolved, rel)), nil
				} else {
					return "", evalErr
				}
			}
			resolved, evalErr := filepath.EvalSymlinks(parent)
			if evalErr != nil {
				return "", evalErr
			}
			rel, relErr := filepath.Rel(parent, clean)
			if relErr != nil {
				return "", relErr
			}
			return filepath.Clean(filepath.Join(resolved, rel)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return clean, nil
		}
	}
}

// SuggestRelative extracts the likely relative portion from a wrong absolute
// path for use in error messages.
func SuggestRelative(inputPath string) string {
	parts := strings.Split(filepath.Clean(inputPath), string(filepath.Separator))
	for i := 2; i < len(parts); i++ {
		candidate := filepath.Join(parts[i:]...)
		if candidate != "" && candidate != "." {
			return candidate
		}
	}
	return filepath.Base(inputPath)
}
