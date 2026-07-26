//go:build !linux

package pathutil

import "fmt"

func MoveFileInWorkspace(workDir, sourcePath, destinationPath string) error {
	return fmt.Errorf("secure workspace lifecycle operations are unsupported on this platform")
}

func DeleteFileInWorkspace(workDir, relPath string) error {
	return fmt.Errorf("secure workspace lifecycle operations are unsupported on this platform")
}

func MovePathInWorkspace(workDir, sourcePath, destinationPath string) error {
	return fmt.Errorf("secure workspace lifecycle operations are unsupported on this platform")
}

func DeletePathInWorkspace(workDir, relPath string, recursive bool) error {
	return fmt.Errorf("secure workspace lifecycle operations are unsupported on this platform")
}
