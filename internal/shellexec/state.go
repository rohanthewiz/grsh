// Package shellexec executes shell ASTs from shellparse: word expansion,
// pipelines, redirections, and builtins.
//
// Environment variables use the real process environment (os.Setenv et al)
// and cd changes the real working directory — grsh is single-threaded per
// script, and this keeps globbing, child processes, and relative paths
// consistent for free.
package shellexec

import (
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
)

// State is the mutable shell state threaded through execution.
type State struct {
	LastStatus  int
	ErrExit     bool // abort script when a statement-position command fails
	PipeFail    bool // pipeline status = rightmost nonzero, not just the last
	Interactive bool // REPL session: enables job control (own pgroups, ^Z)
	Embedded    bool // host-embedded session: own fg pgroups (cancelable), no tty ownership
	Aliases     map[string]string
	ScriptName  string   // $0
	ScriptArgs  []string // $1.. / $@ / $#

	// The embedded foreground process group. Guarded by its own mutex
	// because SignalForeground is the one State entry point that runs on
	// a different goroutine than the (single-threaded) evaluator: the
	// host's cancel button fires while Eval is blocked in Wait.
	fgMu   sync.Mutex
	fgPgid int

	// SourceFn runs another script in the current session (the `source`
	// builtin). Wired up by the runner to avoid an import cycle.
	SourceFn func(path string) error

	// CaptureErr is where $() substitution stderr goes; nil falls back
	// to the process stderr. The runner points it at the session's
	// stderr so an embedded host never gets stray writes to the real
	// terminal it owns.
	CaptureErr io.Writer

	// Jobs tracks background (&) jobs for this session.
	Jobs *JobTable
}

func NewState() *State {
	return &State{Aliases: map[string]string{}, Jobs: NewJobTable()}
}

// setForegroundPgid records (or with 0, clears) the process group of
// the embedded foreground pipeline currently running.
func (st *State) setForegroundPgid(pgid int) {
	st.fgMu.Lock()
	st.fgPgid = pgid
	st.fgMu.Unlock()
}

// SignalForeground delivers sig to the whole foreground process group
// of an embedded session. Returns false when no external pipeline is
// running (builtins and pure-Go evaluation are not signalable).
func (st *State) SignalForeground(sig syscall.Signal) bool {
	st.fgMu.Lock()
	pgid := st.fgPgid
	st.fgMu.Unlock()
	if pgid == 0 {
		return false
	}
	return syscall.Kill(-pgid, sig) == nil
}

// Stdio is the trio of streams a command runs with.
type Stdio struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// OSStdio inherits the process's own streams (interactive tools work).
func OSStdio() Stdio {
	return Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
}

// ExitErr is returned by the exit builtin; it propagates up to main.
type ExitErr struct{ Code int }

func (e ExitErr) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// WordEvaluator evaluates {expr} Go interpolations found in shell words.
// A string result is one field; a []string result splices into several.
// It is nil until the Go engine milestone wires the interpreter in.
type WordEvaluator interface {
	EvalGoExpr(src string) (fields []string, err error)
}
