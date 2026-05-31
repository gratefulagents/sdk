package shell

import (
	"io"
	"os/exec"
	"sync"
)

const boundedOmissionMarker = "\n[output truncated: middle omitted]\n"

// boundedWriter accumulates output up to cap bytes. Once the cap is exceeded it
// keeps the beginning and the latest tail of the stream while continuing to
// accept writes. It always reports n == len(p), so verbose commands are not
// killed just because the tool response reached its display cap.
type boundedWriter struct {
	mu     sync.Mutex
	buf    []byte
	head   []byte
	tail   []byte
	cap    int
	total  int64
	capped bool
}

func newBoundedWriter(cap int) *boundedWriter {
	return &boundedWriter{cap: cap}
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cap <= 0 {
		b.total += int64(len(p))
		b.capped = true
		return len(p), nil
	}
	b.total += int64(len(p))
	if !b.capped && len(b.buf)+len(p) <= b.cap {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	if !b.capped {
		combined := make([]byte, 0, len(b.buf)+len(p))
		combined = append(combined, b.buf...)
		combined = append(combined, p...)
		b.buf = nil
		b.capped = true
		b.head, b.tail = splitHeadTail(combined, b.cap)
		return len(p), nil
	}
	_, tailLimit := boundedHeadTailLimits(b.cap)
	b.tail = appendTail(b.tail, p, tailLimit)
	return len(p), nil
}

func (b *boundedWriter) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capped {
		out := make([]byte, 0, len(b.head)+len(boundedOmissionMarker)+len(b.tail))
		out = append(out, b.head...)
		out = append(out, boundedOmissionMarker...)
		out = append(out, b.tail...)
		return out
	}
	out := make([]byte, len(b.buf))
	copy(out, b.buf)
	return out
}

func (b *boundedWriter) Capped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capped
}

func (b *boundedWriter) TotalBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func boundedHeadTailLimits(cap int) (int, int) {
	if cap <= 1 {
		return cap, 0
	}
	head := cap / 2
	return head, cap - head
}

func splitHeadTail(data []byte, cap int) ([]byte, []byte) {
	headLimit, tailLimit := boundedHeadTailLimits(cap)
	head := append([]byte(nil), data[:minInt(len(data), headLimit)]...)
	var tail []byte
	if tailLimit > 0 {
		start := len(data) - tailLimit
		if start < 0 {
			start = 0
		}
		tail = append([]byte(nil), data[start:]...)
	}
	return head, tail
}

func appendTail(tail, p []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	tail = append(tail, p...)
	if len(tail) <= limit {
		return tail
	}
	out := make([]byte, limit)
	copy(out, tail[len(tail)-limit:])
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// attachBoundedPipes wires cmd's stdout and stderr to the same bounded
// writer. It must be called before Start.
func attachBoundedPipes(cmd *exec.Cmd, w io.Writer) {
	cmd.Stdout = w
	cmd.Stderr = w
}
