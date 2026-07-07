package mcp

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// stderrTailCap bounds how much of an MCP child's stderr is retained per
// server for diagnostics.
const stderrTailCap = 4096

// stderrTail keeps the last stderrTailCap bytes an MCP child writes to
// stderr, so startup failures surface the real reason (a crash's panic or
// traceback) instead of a bare "EOF". Safe for concurrent use: os/exec
// writes from its pipe-copy goroutine while error paths read the tail.
type stderrTail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newStderrTail(capacity int) *stderrTail { return &stderrTail{max: capacity} }

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// Tail returns the captured stderr, trimmed; "" when nothing was written.
func (t *stderrTail) Tail() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// tailAfterGrace waits briefly for the exec pipe-copy goroutine to flush a
// crashed child's final stderr before reading the tail. Best-effort: returns
// as soon as anything is captured or the grace period lapses.
func (t *stderrTail) tailAfterGrace(grace time.Duration) string {
	if t == nil {
		return ""
	}
	deadline := time.Now().Add(grace)
	for {
		if s := t.Tail(); s != "" || !time.Now().Before(deadline) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// errWithStderr annotates err with the child's captured stderr tail, if any.
func errWithStderr(err error, tail string) error {
	if err == nil || tail == "" {
		return err
	}
	return fmt.Errorf("%w; server stderr: %s", err, tail)
}
