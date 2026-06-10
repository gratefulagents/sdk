//go:build !unix

package shell

import (
	"errors"
	"os"
	"os/exec"
)

// startTerminalPTY is unsupported on non-Unix platforms.
func startTerminalPTY(_ *exec.Cmd, _, _ int) (*os.File, error) {
	return nil, errors.New("interactive terminal sessions require a Unix platform")
}
