//go:build linux

package pathutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// MoveFileInWorkspace moves a regular, singly linked file to a new, absent
// path beneath workDir without following symlinks in either path.
func MoveFileInWorkspace(workDir, sourcePath, destinationPath string) error {
	return movePathInWorkspace(workDir, sourcePath, destinationPath, false)
}

// MovePathInWorkspace moves a regular file or directory to a new, absent
// workspace path. The source is first quarantined under a random name in its
// existing parent, then validated, so callers never act on a swapped inode.
func MovePathInWorkspace(workDir, sourcePath, destinationPath string) error {
	return movePathInWorkspace(workDir, sourcePath, destinationPath, true)
}

func movePathInWorkspace(workDir, sourcePath, destinationPath string, allowDirectory bool) (retErr error) {
	sourceParent, sourceName, err := openConfinedParent(workDir, sourcePath)
	if err != nil {
		return err
	}
	defer unix.Close(sourceParent)
	destinationParent, destinationName, err := openConfinedParent(workDir, destinationPath)
	if err != nil {
		return err
	}
	defer unix.Close(destinationParent)

	quarantine, err := quarantineEntry(sourceParent, sourceName)
	if err != nil {
		return fmt.Errorf("quarantining source %s: %w", sourcePath, err)
	}
	restore := true
	defer func() {
		if restore {
			if err := unix.Renameat2(sourceParent, quarantine, sourceParent, sourceName, unix.RENAME_NOREPLACE); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("source remains quarantined as %q: %w", quarantine, err))
			}
		}
	}()
	if err := requireMovableAt(sourceParent, quarantine, sourcePath, allowDirectory); err != nil {
		return err
	}
	if err := unix.Renameat2(sourceParent, quarantine, destinationParent, destinationName, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("moving %s to %s: %w", sourcePath, destinationPath, err)
	}
	restore = false
	return nil
}

// DeleteFileInWorkspace removes a regular, singly linked file beneath workDir
// without following symlinks.
func DeleteFileInWorkspace(workDir, relPath string) error {
	return deletePathInWorkspace(workDir, relPath, false)
}

// DeletePathInWorkspace removes a regular file or an empty directory. The
// recursive parameter is reserved for schema compatibility; recursive tree
// deletion is deliberately rejected because a cross-file delete cannot be
// made atomic or race-safe without filesystem transaction support.
func DeletePathInWorkspace(workDir, relPath string, recursive bool) error {
	if recursive {
		return fmt.Errorf("recursive directory deletion is not supported; delete contained paths explicitly")
	}
	return deletePathInWorkspace(workDir, relPath, true)
}

func deletePathInWorkspace(workDir, relPath string, allowDirectory bool) (retErr error) {
	parent, name, err := openConfinedParent(workDir, relPath)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	quarantine, err := quarantineEntry(parent, name)
	if err != nil {
		return fmt.Errorf("quarantining target %s: %w", relPath, err)
	}
	restore := true
	defer func() {
		if restore {
			if err := unix.Renameat2(parent, quarantine, parent, name, unix.RENAME_NOREPLACE); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("target remains quarantined as %q: %w", quarantine, err))
			}
		}
	}()
	kind, err := movableKindAt(parent, quarantine, relPath, allowDirectory)
	if err != nil {
		return err
	}
	flags := 0
	if kind == unix.S_IFDIR {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(parent, quarantine, flags); err != nil {
		return fmt.Errorf("deleting %s: %w", relPath, err)
	}
	restore = false
	return nil
}

func quarantineEntry(parent int, name string) (string, error) {
	var random [12]byte
	for range 100 {
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		quarantine := ".agentsdk-lifecycle-" + hex.EncodeToString(random[:])
		err := unix.Renameat2(parent, name, parent, quarantine, unix.RENAME_NOREPLACE)
		if err == nil {
			return quarantine, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate quarantine entry")
}

func requireMovableAt(parent int, name, relPath string, allowDirectory bool) error {
	_, err := movableKindAt(parent, name, relPath, allowDirectory)
	return err
}

func movableKindAt(parent int, name, relPath string, allowDirectory bool) (uint32, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, err
	}
	kind := st.Mode & unix.S_IFMT
	switch kind {
	case unix.S_IFREG:
		if st.Nlink != 1 {
			return 0, fmt.Errorf("file must have exactly one hard link")
		}
	case unix.S_IFDIR:
		if !allowDirectory {
			return 0, fmt.Errorf("target %s is not a regular file", relPath)
		}
	case unix.S_IFLNK:
		return 0, fmt.Errorf("target %s is a symlink", relPath)
	default:
		return 0, fmt.Errorf("target %s is not a regular file or directory", relPath)
	}
	return kind, nil
}
