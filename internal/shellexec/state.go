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
)

// State is the mutable shell state threaded through execution.
type State struct {
	LastStatus  int
	ErrExit     bool // abort script when a statement-position command fails
	PipeFail    bool // pipeline status = rightmost nonzero, not just the last
	Interactive bool // REPL session: enables job control (own pgroups, ^Z)
	Aliases     map[string]string
	ScriptName  string   // $0
	ScriptArgs  []string // $1.. / $@ / $#

	// SourceFn runs another script in the current session (the `source`
	// builtin). Wired up by the runner to avoid an import cycle.
	SourceFn func(path string) error

	// Jobs tracks background (&) jobs for this session.
	Jobs *JobTable
}

func NewState() *State {
	return &State{Aliases: map[string]string{}, Jobs: NewJobTable()}
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
