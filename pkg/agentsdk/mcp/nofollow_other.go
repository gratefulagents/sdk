//go:build !linux

package mcp

import (
	"fmt"
	"os"
)

func mkdirAllBeneath(workDir, relPath string, perm os.FileMode) error {
	return fmt.Errorf("secure blob persistence is unsupported on this platform")
}

func createExclusiveBeneath(workDir, relPath string, data []byte, perm os.FileMode) error {
	return fmt.Errorf("secure blob persistence is unsupported on this platform")
}
