//go:build linux

package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func mkdirAllBeneath(workDir, relPath string, perm os.FileMode) error {
	root, rel, err := openBlobRoot(workDir, relPath)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	fd := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
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
			return err
		}
		fd = next
	}
	if fd != root {
		return unix.Close(fd)
	}
	return nil
}

func createExclusiveBeneath(workDir, relPath string, data []byte, perm os.FileMode) error {
	root, rel, err := openBlobRoot(workDir, relPath)
	if err != nil {
		return err
	}
	defer unix.Close(root)
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 0 || parts[len(parts)-1] == "." || parts[len(parts)-1] == "" {
		return fmt.Errorf("file path is required")
	}
	fd := root
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if fd != root {
			unix.Close(fd)
		}
		if err != nil {
			return err
		}
		fd = next
	}
	if fd != root {
		defer unix.Close(fd)
	}
	name := parts[len(parts)-1]
	fileFD, err := unix.Openat(fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		unix.Close(fileFD)
		if cleanup {
			_ = unix.Unlinkat(fd, name, 0)
		}
	}()
	for len(data) > 0 {
		n, err := unix.Write(fileFD, data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	if err := unix.Close(fileFD); err != nil {
		fileFD = -1
		return err
	}
	fileFD = -1
	cleanup = false
	return nil
}

func openBlobRoot(workDir, relPath string) (int, string, error) {
	if strings.TrimSpace(workDir) == "" {
		return -1, "", fmt.Errorf("workspace root is required")
	}
	baseAbs, err := filepath.Abs(workDir)
	if err != nil {
		return -1, "", err
	}
	base, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return -1, "", err
	}
	path := relPath
	if filepath.IsAbs(path) {
		path, err = filepath.Rel(base, filepath.Clean(path))
		if err != nil {
			return -1, "", err
		}
	}
	path = filepath.Clean(path)
	if path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return -1, "", fmt.Errorf("output path %s is outside workspace root %s", relPath, workDir)
	}
	fd, err := unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	return fd, path, err
}
