// Package grsh embeds a grsh session in another Go program: an editor's
// terminal panel, a bot, a task runner. It is the only public API of
// this module — everything under internal/ can change without notice.
//
// The embedding contract, in one place:
//
//   - Streaming: Stdout/Stderr receive child output as it is produced.
//     Writers may be called from os/exec copier goroutines; make them
//     goroutine-safe (a mutex or an event-posting wrapper both work).
//   - Cancellation: foreground pipelines run in their own process
//     group; Interrupt/Kill signal that group from any goroutine
//     without touching the host process. Pure-Go evaluation (an
//     interpreted `for {}`) is not currently interruptible.
//   - No terminal claims: the session never calls tcsetpgrp and never
//     reads the host's stdin unless Options.Stdin is set explicitly —
//     children and $() substitutions see EOF by default, so a raw-mode
//     tty owned by the host (tcell, bubbletea) stays untouched.
//   - Process-global state: cd and export mutate the host process's
//     working directory and environment. That is grsh's deliberate
//     design (see internal/shellexec) — hosts should use absolute
//     paths for their own file operations and treat Cwd as shared.
//   - One command at a time: Eval serializes on an internal mutex and
//     blocks until the input finishes. Call it off the UI loop. The
//     other methods reflect state as of the last completed Eval; only
//     Interrupt/Kill are meant for use while an Eval is in flight.
//
// Minimal host loop:
//
//	sess := grsh.NewSession(grsh.Options{Stdout: out, Stderr: out})
//	for line := range inputLines {
//	    buf = append(buf, line)
//	    src := strings.Join(buf, "\n")
//	    if sess.NeedsMore(src) {
//	        continue // show a continuation prompt
//	    }
//	    buf = buf[:0]
//	    if err := sess.Eval(src); err != nil {
//	        if code, ok := grsh.ExitCode(err); ok {
//	            closePanel(code)
//	        } else {
//	            fmt.Fprintln(out, grsh.UserMessage(err))
//	        }
//	    }
//	}
package grsh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"syscall"

	"github.com/rohanthewiz/grsh/internal/runner"
	"github.com/rohanthewiz/grsh/internal/shellexec"
	"github.com/rohanthewiz/serr"
)

// Options configures an embedded Session. Stdout/Stderr default to the
// process streams; Stdin defaults to an always-EOF reader (NOT the
// process stdin — see the package comment).
type Options struct {
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	ScriptName string   // $0 as seen by scripts; defaults to "grsh"
	ScriptArgs []string // $1..
}

// Session is a persistent embedded grsh session: variables, aliases,
// the classifier's scope, and background jobs all survive across Eval
// calls, exactly like lines typed at the standalone REPL.
type Session struct {
	mu sync.Mutex // serializes Eval; see the package comment
	r  *runner.Session
}

// eofReader keeps embedded children away from the host's stdin.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// NewSession creates an embedded-mode session. It never fails: a
// session is pure in-process state until the first Eval runs something.
func NewSession(o Options) *Session {
	in := o.Stdin
	if in == nil {
		in = eofReader{}
	}
	name := o.ScriptName
	if name == "" {
		name = "grsh"
	}
	return &Session{r: runner.NewSession(runner.Options{
		Stdin:      in,
		Stdout:     o.Stdout,
		Stderr:     o.Stderr,
		ScriptName: name,
		ScriptArgs: o.ScriptArgs,
		Embedded:   true,
	})}
}

// Eval runs one complete input unit (use NeedsMore to accumulate lines
// into one) and blocks until it finishes. Interpreter panics are
// recovered and returned as errors so a host UI never dies to an
// interpreter bug; the stack rides along for bug reports
// (serr attribute "stack").
func (s *Session) Eval(src string) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			err = serr.New(fmt.Sprintf("grsh internal error: %v", r),
				"stack", string(debug.Stack()))
		}
	}()
	return s.r.Eval(src)
}

// NeedsMore reports whether src is an incomplete input unit (unclosed
// block, trailing shell continuation); the host keeps appending lines
// while true. Never blocks on Eval and mutates nothing.
func (s *Session) NeedsMore(src string) bool { return s.r.NeedsMore(src) }

// Idents lists every identifier the session currently knows — builtins,
// stdlib registry names, and user declarations — for tab completion.
func (s *Session) Idents() []string { return s.r.Idents() }

// Notifications drains "[1]  Done  cmd &" messages for finished
// background jobs; show them before the next prompt.
func (s *Session) Notifications() []string { return s.r.Notifications() }

// LastStatus is the exit status of the last completed pipeline, for
// prompt decoration ("[1]>").
func (s *Session) LastStatus() int { return s.r.LastStatus() }

// Cwd returns the process working directory (shared with the host —
// cd moves both), abbreviated by nothing; hosts format as they like.
func (s *Session) Cwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "?"
	}
	return wd
}

// Interrupt sends SIGINT to the running foreground pipeline's process
// group — the host's Ctrl+C / stop button. Safe from any goroutine.
// Returns false when nothing signalable is running (idle, or a builtin
// or pure-Go evaluation is in progress).
func (s *Session) Interrupt() bool { return s.r.SignalForeground(syscall.SIGINT) }

// Kill sends SIGKILL to the foreground process group — the escalation
// when Interrupt was ignored. Same visibility rules as Interrupt.
func (s *Session) Kill() bool { return s.r.SignalForeground(syscall.SIGKILL) }

// UserMessage renders an Eval error as the concise one-liner the
// standalone shell would print ("script:12: <root cause>").
func UserMessage(err error) string { return runner.UserMessage(err) }

// ExitCode unwraps the `exit` builtin (and errexit trips): ok=true
// means the user asked the session to end and the host should close
// the panel with code.
func ExitCode(err error) (code int, ok bool) {
	if xe, isExit := errors.AsType[shellexec.ExitErr](err); isExit {
		return xe.Code, true
	}
	return 0, false
}
