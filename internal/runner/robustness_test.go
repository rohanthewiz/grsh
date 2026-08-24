package runner

import (
	"bytes"
	"strings"
	"testing"
)

func newTestSession(out *bytes.Buffer) *Session {
	return NewSession(Options{Stdin: strings.NewReader(""), Stdout: out, Stderr: out})
}

// TestPanicVectorsReturnErrors: the known reflect panic vectors must come
// back as positioned errors, and the session must stay usable afterward.
func TestPanicVectorsReturnErrors(t *testing.T) {
	tests := []struct {
		name, src, wantMsg string
	}{
		{"nil map write", "var m map[string]int\nm[\"k\"] = 1\n", "nil map"},
		{"delete on nil map is a no-op", "var m map[string]int\ndelete(m, \"k\")\nfmt.Println(\"ok\")\n", ""},
		{"make negative length", "x := make([]int, -1)\n", "negative length"},
		{"make len over cap", "x := make([]int, 5, 2)\n", "length larger than capacity"},
		{"copy element mismatch", "a := []int{1}\nb := []string{\"x\"}\ncopy(a, b)\n", "cannot copy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			sess := newTestSession(&out)
			err := sess.RunSource("t.grsh", tc.src)
			if tc.wantMsg == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected an error containing %q", tc.wantMsg)
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantMsg)
				}
			}
			// The session survives whatever happened above.
			if err := sess.Eval("y := 41 + 1"); err != nil {
				t.Fatalf("session unusable after error: %v", err)
			}
		})
	}
}

// TestRunSourceRecoversPanics: even an unforeseen interpreter panic must
// surface as an error, not kill the process hosting the session.
func TestRunSourceRecoversPanics(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)
	// regexp.MustCompile panics inside callReflect's recover; a panic that
	// escapes any inner guard is caught by the RunSource backstop. Either
	// way the contract is: error out, session lives.
	if err := sess.Eval(`re := regexp.MustCompile("(unclosed")`); err == nil {
		t.Fatal("expected an error from the panicking call")
	}
	if err := sess.Eval("z := 1"); err != nil {
		t.Fatalf("session unusable after panic: %v", err)
	}
}

// TestClassifierNotWedgedByFailedParse: a failed input that opened a brace
// must not advance classifier depth (the phantom-`}` REPL wedge).
func TestClassifierNotWedgedByFailedParse(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)
	// Balanced braces so classification succeeds, but go/parser fails.
	if err := sess.Eval("func f() {\nx := ]bad\n}"); err == nil {
		t.Fatal("expected a parse error")
	}
	if sess.NeedsMore("}") {
		t.Error("classifier depth advanced on failed input: phantom brace demanded")
	}
	// Shell classification is intact.
	out.Reset()
	if err := sess.Eval("echo hi"); err != nil {
		t.Fatalf("echo failed: %v", err)
	}
	if !strings.Contains(out.String(), "hi") {
		t.Errorf("expected shell output, got %q", out.String())
	}
}

// TestClassifierCommitsOnRuntimeError: declared names must still commit
// when the input parses but fails at runtime (the shell already ran).
func TestClassifierCommitsOnRuntimeError(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)
	if err := sess.Eval("y := 1\nboomVar := undefinedThing\n"); err == nil {
		t.Fatal("expected a runtime error")
	}
	// y was declared before the failure; `y = 2` must classify as Go.
	if err := sess.Eval("y = 2"); err != nil {
		t.Fatalf("y = 2 after runtime error: %v", err)
	}
}

// TestCompoundBitOps: the full compound-assignment set classifies as Go
// and evaluates (a missed <<= used to hang the classifier as a heredoc).
func TestCompoundBitOps(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)
	src := `x := 1
x <<= 4
x >>= 1
x |= 2
x &= 14
x ^= 1
x &^= 4
y := ^0
fmt.Println(x, y)
`
	if err := sess.RunSource("bits.grsh", src); err != nil {
		t.Fatalf("bit ops failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "11 -1" {
		t.Errorf("got %q, want %q", got, "11 -1")
	}
	// And NeedsMore must not treat <<= as a pending heredoc.
	if sess.NeedsMore("x <<= 2") {
		t.Error("x <<= 2 reported as incomplete (heredoc misparse)")
	}
}

// TestPrefixEnvAssignment: FOO=bar cmd sets a per-command environment
// without persisting; bare FOO=bar is rejected with a hint.
func TestPrefixEnvAssignment(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)
	if err := sess.Eval(`FOO=barval sh -c 'echo prefix:$FOO'`); err != nil {
		t.Fatalf("prefix env failed: %v", err)
	}
	if !strings.Contains(out.String(), "prefix:barval") {
		t.Errorf("child did not see FOO: %q", out.String())
	}
	out.Reset()
	if err := sess.Eval(`sh -c 'echo after:$FOO'`); err != nil {
		t.Fatalf("followup failed: %v", err)
	}
	if !strings.Contains(out.String(), "after:\n") && strings.Contains(out.String(), "after:barval") {
		t.Errorf("FOO leaked into the session environment: %q", out.String())
	}

	out.Reset()
	if err := sess.Eval("BAREVAR=oops"); err != nil {
		t.Fatalf("bare assignment should be user-level, got hard error: %v", err)
	}
	if !strings.Contains(out.String(), "shell assignment is not supported") {
		t.Errorf("bare assignment message missing, got %q", out.String())
	}
	if sess.LastStatus() == 0 {
		t.Error("bare assignment should set nonzero status")
	}
}

// TestParamExpansionRejected: ${VAR:-default}-style forms must fail at
// parse time instead of silently expanding to "".
func TestParamExpansionRejected(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)
	// Shell parse errors surface as user-level messages (status 1).
	if err := sess.Eval(`echo ${HOME:-/tmp}`); err != nil {
		var msg = err.Error()
		if !strings.Contains(msg, "parameter expansion") {
			t.Errorf("unexpected hard error: %v", err)
		}
		return
	}
	if !strings.Contains(out.String(), "parameter expansion") {
		t.Errorf("no rejection message printed, got %q", out.String())
	}
	// Plain ${NAME} still works.
	out.Reset()
	if err := sess.Eval("export GRSH_PE_T=okval\necho ${GRSH_PE_T}"); err != nil {
		t.Fatalf("plain ${NAME}: %v", err)
	}
	if !strings.Contains(out.String(), "okval") {
		t.Errorf("plain ${NAME} did not expand: %q", out.String())
	}
}

// TestErrexitInteractiveSurvives: errexit(true) at an interactive prompt
// must not exit the session on a failing command.
func TestErrexitInteractiveSurvives(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)
	sess.SetInteractive(true)
	if err := sess.Eval("errexit(true)"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Eval("false"); err != nil {
		t.Fatalf("interactive errexit killed the session: %v", err)
	}
	if sess.LastStatus() == 0 {
		t.Error("status should be nonzero after false")
	}
	if err := sess.Eval("echo alive"); err != nil {
		t.Fatalf("session dead after errexit failure: %v", err)
	}
}

// TestInterpolationErrorPosition: an error inside {expr} must report the
// script line of the enclosing shell statement, not line 1. Expansion
// failures are user-level (stderr + status 1, script continues), so the
// position is asserted on the printed message.
func TestInterpolationErrorPosition(t *testing.T) {
	var out bytes.Buffer
	sess := newTestSession(&out)
	src := "x := 1\n\n\n\necho {undefinedVar}\n"
	if err := sess.RunSource("pos.grsh", src); err != nil {
		t.Fatalf("expansion failures are user-level, got hard error: %v", err)
	}
	msg := out.String()
	if !strings.Contains(msg, "pos.grsh:4") && !strings.Contains(msg, "pos.grsh:5") {
		t.Errorf("printed message %q does not point at the echo line", msg)
	}
	if !strings.Contains(msg, "undefinedVar") {
		t.Errorf("printed message %q does not name the undefined variable", msg)
	}
	if sess.LastStatus() == 0 {
		t.Error("failed expansion should set a nonzero status")
	}
}
