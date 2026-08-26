package runner

import (
	"bytes"
	"errors"
	"testing"

	"github.com/rohanthewiz/grsh/internal/shellexec"
)

// LastStatus is written by the shell leg only (shellexec.State.SetStatus).
// Before RunSource reconciled it, a unit containing no shell command left
// whatever the previous shell pipeline had set — so `false` followed by a
// perfectly good Go statement kept reporting 1, both in the REPL prompt
// badge and to status()/ok() in internal/builtins.
//
// The reconciliation is guarded by StatusSeq rather than by chunk kinds,
// because a Go statement can still reach the shell (a {expr} interpolation,
// a $() capture) and that pipeline's status must win. The cases below pin
// both directions: the Go leg gets a status when it is genuinely alone, and
// never overwrites one the shell produced.
func TestGoUnitStatus(t *testing.T) {
	tests := []struct {
		name string
		// steps run in order on one session; only the last one's effect on
		// LastStatus is asserted, so each case can set up a dirty status first.
		steps []string
		want  int
	}{
		{"shell failure is reported", []string{"false\n"}, 1},
		{"shell success clears it", []string{"false\n", "true\n"}, 0},

		// The regression itself, in both shapes a REPL user hits: an
		// expression statement and a declaration.
		{"go call after a failure", []string{"false\n", "fmt.Println(\"ok\")\n"}, 0},
		{"go declaration after a failure", []string{"false\n", "x := 5\n"}, 0},

		// A failing Go unit must not report success. It cannot report a
		// meaningful code either — Go statements have no exit status — so it
		// takes the shell's general-error convention.
		{"go runtime error", []string{"true\n", "var m map[string]int\nm[\"k\"] = 1\n"}, 1},
		{"go parse error", []string{"true\n", "x := (\n"}, 1},

		// Mixed units: whatever the shell leg last said stands, because the
		// shell is the only leg that can produce a real status.
		{"shell inside a go unit wins", []string{"true\n", "s := $(false)\nfmt.Println(len(s))\n"}, 1},
		{"trailing shell command wins", []string{"true\n", "x := 1\nfalse\n"}, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			sess := newTestSession(&out)
			for _, src := range tc.steps {
				// Errors are the point of several cases; the session must stay
				// usable either way, which the final LastStatus read proves.
				_ = sess.RunSource("<test>", src)
			}
			if got := sess.LastStatus(); got != tc.want {
				t.Errorf("LastStatus = %d, want %d\noutput: %s", got, tc.want, out.String())
			}
		})
	}
}

// TestExitKeepsItsCode guards the one path the reconciliation must not
// touch: `exit N` reports its code through ExitErr, and rewriting
// LastStatus to the general-error 1 would make the shell exit with the
// wrong code.
func TestExitKeepsItsCode(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)

	err := sess.RunSource("<test>", "exit 7\n")
	exit, ok := errors.AsType[shellexec.ExitErr](err)
	if !ok {
		t.Fatalf("RunSource(exit 7) = %v, want ExitErr", err)
	}
	if exit.Code != 7 {
		t.Errorf("exit code = %d, want 7", exit.Code)
	}
	if got := sess.LastStatus(); got == 1 {
		t.Errorf("LastStatus = 1: the exit path was rewritten to a general error")
	}
}
