//go:build !linux

package pathutil

import (
	"fmt"
	"os"
)

func openInWorkspace(workDir, relPath string, flag int, perm os.FileMode) (*os.File, error) {
	return nil, fmt.Errorf("secure workspace opens are unsupported on this platform")
}
