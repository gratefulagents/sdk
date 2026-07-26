//go:build linux

package pathutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func mkdirAllInWorkspace(workDir, relPath string, perm os.FileMode) error {
	root, rel, err := confinedRoot(workDir, relPath)
	if err != nil {
		return err
	}
	defer unix.Close(root)

	fd := root
	for _, part := range splitRelative(rel) {
		if err := unix.Mkdirat(fd, part, uint32(perm.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
			if fd != root {
				unix.Close(fd)
			}
			return err
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if fd != root {
			unix.Close(fd)
		}
		if err != nil {
			return fmt.Errorf("opening directory %s: %w", part, err)
		}
		fd = next
	}
	if fd != root {
		return unix.Close(fd)
	}
	return nil
}

func atomicWriteFileInWorkspace(workDir, relPath string, data []byte, perm os.FileMode) error {
	return atomicWriteFileInWorkspaceWithMode(workDir, relPath, data, perm, false)
}

func atomicWriteFilePreservingModeInWorkspace(workDir, relPath string, data []byte, perm os.FileMode) error {
	return atomicWriteFileInWorkspaceWithMode(workDir, relPath, data, perm, true)
}

func atomicWriteFileInWorkspaceWithMode(workDir, relPath string, data []byte, perm os.FileMode, preserveExisting bool) error {
	parent, name, err := openConfinedParent(workDir, relPath)
	if err != nil {
		return err
	}
	defer unix.Close(parent)

	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("target %s is a symlink", relPath)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("target %s is not a regular file", relPath)
		}
		if preserveExisting {
			perm = os.FileMode(st.Mode & 0o777)
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}

	tmpName, fd, err := createTempAt(parent, perm)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		unix.Close(fd)
		if cleanup {
			_ = unix.Unlinkat(parent, tmpName, 0)
		}
	}()
	if err := unix.Fchmod(fd, uint32(perm.Perm())); err != nil {
		return err
	}
	if err := writeAll(fd, data); err != nil {
		return err
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if err := unix.Close(fd); err != nil {
		fd = -1
		return err
	}
	fd = -1
	if err := unix.Renameat(parent, tmpName, parent, name); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func createExclusiveFileInWorkspace(workDir, relPath string, data []byte, perm os.FileMode) error {
	parent, name, err := openConfinedParent(workDir, relPath)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	tmpName, fd, err := createTempAt(parent, perm)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if fd >= 0 {
			unix.Close(fd)
		}
		if cleanup {
			_ = unix.Unlinkat(parent, tmpName, 0)
		}
	}()
	if err := unix.Fchmod(fd, uint32(perm.Perm())); err != nil {
		return err
	}
	if err := writeAll(fd, data); err != nil {
		return err
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if err := unix.Close(fd); err != nil {
		fd = -1
		return err
	}
	fd = -1
	if err := unix.Renameat2(parent, tmpName, parent, name, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func openConfinedParent(workDir, relPath string) (int, string, error) {
	root, rel, err := confinedRoot(workDir, relPath)
	if err != nil {
		return -1, "", err
	}
	parts := splitRelative(rel)
	if len(parts) == 0 {
		unix.Close(root)
		return -1, "", fmt.Errorf("file path is required")
	}
	fd := root
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if fd != root {
			unix.Close(fd)
		}
		if err != nil {
			unix.Close(root)
			return -1, "", fmt.Errorf("opening parent directory %s: %w", part, err)
		}
		fd = next
	}
	if fd != root {
		unix.Close(root)
	}
	return fd, parts[len(parts)-1], nil
}

func confinedRoot(workDir, relPath string) (int, string, error) {
	if strings.TrimSpace(workDir) == "" {
		return -1, "", fmt.Errorf("workspace root is required")
	}
	baseAbs, err := filepath.Abs(workDir)
	if err != nil {
		return -1, "", fmt.Errorf("resolving workspace root: %w", err)
	}
	base, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return -1, "", fmt.Errorf("resolving workspace root symlinks: %w", err)
	}
	rel, err := workspaceRelative(base, relPath)
	if err != nil {
		return -1, "", err
	}
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("opening workspace root: %w", err)
	}
	return fd, rel, nil
}

func workspaceRelative(base, path string) (string, error) {
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(base, filepath.Clean(path))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path %s is outside workspace root %s", path, base)
		}
		path = rel
	}
	path = filepath.Clean(path)
	if path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside workspace root %s", path, base)
	}
	return path, nil
}

func splitRelative(path string) []string {
	if path == "." || path == "" {
		return nil
	}
	return strings.Split(path, string(filepath.Separator))
}

func createTempAt(parent int, perm os.FileMode) (string, int, error) {
	var random [12]byte
	for range 100 {
		if _, err := rand.Read(random[:]); err != nil {
			return "", -1, err
		}
		name := ".agentsdk-" + hex.EncodeToString(random[:])
		fd, err := unix.Openat(parent, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
		if err == nil {
			return name, fd, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", -1, err
		}
	}
	return "", -1, fmt.Errorf("could not create temporary file")
}

func writeAll(fd int, data []byte) error {
	for len(data) > 0 {
		n, err := unix.Write(fd, data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
