//go:build linux

package pathutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// openInWorkspace opens a file beneath workDir using openat2 with
// RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS, eliminating any canonicalize-then-open
// TOCTOU window.
func openInWorkspace(workDir, relPath string, flag int, perm os.FileMode) (*os.File, error) {
	baseAbs, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace root: %w", err)
	}
	base, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace root symlinks: %w", err)
	}

	rel := strings.TrimSpace(relPath)
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		// Reduce an absolute path to a workspace-relative one when possible;
		// openat2 with RESOLVE_BENEATH treats absolute paths as escapes.
		r, relErr := filepath.Rel(base, filepath.Clean(rel))
		if relErr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("path %s is outside the workspace root %s", relPath, workDir)
		}
		rel = r
	}
	rel = filepath.Clean(rel)

	dirfd, err := unix.Open(base, unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("opening workspace root: %w", err)
	}
	defer unix.Close(dirfd)

	how := &unix.OpenHow{
		Flags:   uint64(flag) | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	}
	if flag&(os.O_CREATE|unix.O_TMPFILE) != 0 {
		how.Mode = uint64(syscallMode(perm))
	}
	fd, err := unix.Openat2(dirfd, rel, how)
	if err != nil {
		if errors.Is(err, syscall.ENOSYS) {
			return nil, fmt.Errorf("secure workspace opens require openat2: %w", err)
		}
		if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("path %s escapes workspace or contains symlink: %w", relPath, err)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(base, rel)), nil
}

func syscallMode(perm os.FileMode) uint32 {
	var mode uint32
	mode = uint32(perm.Perm())
	if perm&os.ModeSetuid != 0 {
		mode |= syscall.S_ISUID
	}
	if perm&os.ModeSetgid != 0 {
		mode |= syscall.S_ISGID
	}
	if perm&os.ModeSticky != 0 {
		mode |= syscall.S_ISVTX
	}
	return mode
}
