package grsh

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// newBufSession builds an embedded session writing into one shared
// buffer, the shape a host UI uses (minus the event plumbing).
func newBufSession() (*Session, *bytes.Buffer) {
	var out bytes.Buffer
	return NewSession(Options{Stdout: &out, Stderr: &out}), &out
}

// The core embedding promise: Eval runs shell and output lands on the
// caller's writer, not the process stdout.
func TestSessionEvalStreamsOutput(t *testing.T) {
	sess, out := newBufSession()
	if err := sess.Eval("echo hello"); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got := out.String(); got != "hello\n" {
		t.Errorf("got %q, want %q", got, "hello\n")
	}
	if sess.LastStatus() != 0 {
		t.Errorf("LastStatus = %d, want 0", sess.LastStatus())
	}
}

// Session state (Go variables, the classifier's scope) must persist
// across Evals — that is what makes the panel a shell, not a runner.
func TestSessionStatePersistsAcrossEvals(t *testing.T) {
	sess, out := newBufSession()
	if err := sess.Eval("x := 6"); err != nil {
		t.Fatalf("Eval declare: %v", err)
	}
	if err := sess.Eval("echo {x * 7}"); err != nil {
		t.Fatalf("Eval use: %v", err)
	}
	if got := out.String(); got != "42\n" {
		t.Errorf("got %q, want %q", got, "42\n")
	}
}

// Pipelines exercise runForegroundEmbedded's multi-command start/wait
// path, including the copier-goroutine flush into a non-file writer.
func TestSessionEvalPipeline(t *testing.T) {
	sess, out := newBufSession()
	if err := sess.Eval("echo hello | tr a-z A-Z"); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got := out.String(); got != "HELLO\n" {
		t.Errorf("got %q, want %q", got, "HELLO\n")
	}
}

// NeedsMore drives the host's continuation prompt: an open block is
// incomplete, a closed one is not.
func TestSessionNeedsMore(t *testing.T) {
	sess, _ := newBufSession()
	if !sess.NeedsMore("if true {") {
		t.Error("open block should need more input")
	}
	if sess.NeedsMore("echo done") {
		t.Error("complete command should not need more input")
	}
}

// The exit builtin must surface as ExitCode so the host can close the
// panel instead of printing an error.
func TestSessionExitCode(t *testing.T) {
	sess, _ := newBufSession()
	err := sess.Eval("exit 3")
	code, ok := ExitCode(err)
	if !ok || code != 3 {
		t.Errorf("ExitCode = (%d, %v), want (3, true)", code, ok)
	}
	if _, ok := ExitCode(nil); ok {
		t.Error("ExitCode(nil) should be ok=false")
	}
}

// Children read EOF by default — a command that consumes stdin must
// finish immediately instead of stealing the host's terminal input.
func TestSessionStdinDefaultsToEOF(t *testing.T) {
	sess, out := newBufSession()
	done := make(chan error, 1)
	go func() { done <- sess.Eval("cat") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cat blocked — embedded stdin is not EOF")
	}
	if out.Len() != 0 {
		t.Errorf("unexpected output %q", out.String())
	}
}

// Command substitution must also stay off the process stdin and route
// its stderr to the session's writer, not the host's terminal.
func TestSessionCaptureUsesSessionStreams(t *testing.T) {
	sess, out := newBufSession()
	if err := sess.Eval(`echo got $(cat)`); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got := out.String(); got != "got\n" {
		t.Errorf("got %q, want %q", got, "got\n")
	}
	out.Reset()
	// A failing command inside $() reports on the substitution's stderr
	// — which must be the session's writer, never the process stderr.
	_ = sess.Eval(`x := $(definitely-not-a-command-grsh)`)
	if !strings.Contains(out.String(), "command not found") {
		t.Errorf("substitution stderr did not reach the session writer: %q", out.String())
	}
}

// Interrupt is the host's stop button: a foreground sleep dies with
// SIGINT (status 130) well before its timer, and the blocked Eval
// returns. Polling covers the window before the child starts.
func TestSessionInterrupt(t *testing.T) {
	sess, _ := newBufSession()
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- sess.Eval("sleep 30") }()

	deadline := time.After(5 * time.Second)
	for {
		if sess.Interrupt() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("foreground pipeline never became signalable")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Eval did not return after Interrupt")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("interrupt took %v", elapsed)
	}
	if sess.LastStatus() != 130 { // 128 + SIGINT
		t.Errorf("LastStatus = %d, want 130", sess.LastStatus())
	}
}

// With nothing running there is nothing to signal.
func TestSessionInterruptIdle(t *testing.T) {
	sess, _ := newBufSession()
	if sess.Interrupt() {
		t.Error("Interrupt on idle session should report false")
	}
}

// A panic anywhere under Eval must come back as an error, never crash
// the host. Forced here via a nil inner runner; the recover in Eval is
// the guard being pinned.
func TestSessionEvalRecoversPanic(t *testing.T) {
	s := &Session{} // nil runner → guaranteed panic inside Eval
	err := s.Eval("echo hi")
	if err == nil {
		t.Fatal("expected an error from a panicking Eval")
	}
	if !strings.Contains(err.Error(), "grsh internal error") {
		t.Errorf("unexpected error text: %v", err)
	}
}

// Eval is documented as serialized: two concurrent Evals must not
// interleave interpreter state (the mutex is the guard being pinned).
func TestSessionEvalSerialized(t *testing.T) {
	sess, out := newBufSession()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sess.Eval("echo tick")
		}()
	}
	wg.Wait()
	if got := strings.Count(out.String(), "tick"); got != 8 {
		t.Errorf("got %d ticks, want 8 (output %q)", got, out.String())
	}
}

// Cwd never returns empty — the panel prompt renders it directly.
func TestSessionCwd(t *testing.T) {
	sess, _ := newBufSession()
	if sess.Cwd() == "" {
		t.Error("Cwd returned empty")
	}
}
