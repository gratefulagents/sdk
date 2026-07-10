//go:build !linux

package pathutil

import (
	"fmt"
	"os"
)

func mkdirAllInWorkspace(workDir, relPath string, perm os.FileMode) error {
	return fmt.Errorf("secure workspace filesystem operations are unsupported on this platform")
}

func atomicWriteFileInWorkspace(workDir, relPath string, data []byte, perm os.FileMode) error {
	return fmt.Errorf("secure workspace filesystem operations are unsupported on this platform")
}

func atomicWriteFilePreservingModeInWorkspace(workDir, relPath string, data []byte, perm os.FileMode) error {
	return fmt.Errorf("secure workspace filesystem operations are unsupported on this platform")
}

func createExclusiveFileInWorkspace(workDir, relPath string, data []byte, perm os.FileMode) error {
	return fmt.Errorf("secure workspace filesystem operations are unsupported on this platform")
}
