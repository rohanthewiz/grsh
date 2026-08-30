package tutor

import (
	"strings"
	"sync"
)

// capture is the tee half of the tutor's output grading: the lesson
// session's stdout/stderr go through io.MultiWriter(os.Stdout, capture),
// so the user sees their command's output normally while the engine keeps
// a copy to grade against.
//
// Two constraints shape it:
//
//   - Writes arrive from os/exec's copier goroutines while Eval runs, so
//     every method takes the mutex (the embedding contract in session.go
//     already warns hosts that writers must be goroutine-safe).
//   - A `yes | head -100000` step must not grow the process without bound,
//     so the buffer keeps only the last `limit` bytes. Verifiers look for
//     what a command printed, and a truncated head is the cheap tail-drop
//     that keeps regexps meaningful for realistic lesson output.
type capture struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func newCapture(limit int) *capture { return &capture{limit: limit} }

// Write implements io.Writer. It never reports an error: a failure here
// would surface as a spurious command failure in the user's lesson.
func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, p...)
	if over := len(c.buf) - c.limit; over > 0 {
		// Slide the window rather than reallocating: copy keeps the
		// backing array, so a long-running step doesn't churn.
		c.buf = c.buf[:copy(c.buf, c.buf[over:])]
	}
	return len(p), nil
}

// Reset clears the buffer. The engine calls it once per input unit, so a
// verifier only ever sees the current attempt — not the accumulated
// output of every command since the lesson started.
func (c *capture) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = c.buf[:0]
}

// String returns the captured output of the current attempt.
func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

// trimmed is String with surrounding whitespace removed — what most
// verifiers actually want to match against.
func (c *capture) trimmed() string { return strings.TrimSpace(c.String()) }
