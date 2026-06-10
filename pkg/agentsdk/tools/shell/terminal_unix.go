//go:build unix

package shell

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// startTerminalPTY starts cmd attached to a new PTY with the given window size.
// pty sets Setsid (new session = new process group), so killProcessTree's
// group kill works without configurePlatformProcAttrs (Setpgid would conflict
// with Setsid at fork time).
func startTerminalPTY(cmd *exec.Cmd, rows, cols int) (*os.File, error) {
	return pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}
