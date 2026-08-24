package repl

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/grsh/internal/runner"
	"github.com/rohanthewiz/serr"
)

type step struct {
	line  string
	err   error
	delay time.Duration // simulates the user pausing before this line
}

// fakeReader scripts a Readline sequence and records the prompts shown.
type fakeReader struct {
	steps   []step
	prompts []string
}

func (f *fakeReader) Readline() (string, error) {
	if len(f.steps) == 0 {
		return "", io.EOF
	}
	s := f.steps[0]
	f.steps = f.steps[1:]
	time.Sleep(s.delay)
	return s.line, s.err
}

func (f *fakeReader) SetPrompt(p string) { f.prompts = append(f.prompts, p) }

func run(t *testing.T, steps ...step) (stdout, stderr string, code int, prompts []string) {
	t.Helper()
	var out, errB bytes.Buffer
	sess := runner.NewSession(runner.Options{Stdout: &out, Stderr: &errB, ScriptName: "repl"})
	rd := &fakeReader{steps: steps}
	code = loop(sess, rd, &out, &errB, openHistory(""))
	return out.String(), errB.String(), code, rd.prompts
}

func TestLoopMultiLineBlock(t *testing.T) {
	stdout, stderr, code, prompts := run(t,
		step{line: "x := 41"},
		step{line: "if x > 40 {"},
		step{line: `fmt.Println("big")`},
		step{line: "}"},
	)
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if stdout != "big\n" {
		t.Errorf("stdout %q, want %q", stdout, "big\n")
	}
	// Prompts shown before lines 3 and 4 must be continuations.
	for i, p := range prompts {
		wantCont := i == 2 || i == 3
		if isCont := strings.Contains(p, "..."); isCont != wantCont {
			t.Errorf("prompt %d = %q, continuation = %v, want %v", i, p, isCont, wantCont)
		}
	}
}

// TestInspectorCommand: `?name` pretty-prints a live Go variable.
func TestInspectorCommand(t *testing.T) {
	stdout, stderr, code, _ := run(t,
		step{line: `xs := []string{"alpha", "beta"}`},
		step{line: "?xs"},
		step{line: "?nosuch"},
	)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "[]string (len 2)") || !strings.Contains(stdout, `"alpha"`) {
		t.Errorf("inspector output missing: %q", stdout)
	}
	if !strings.Contains(stderr, "nosuch is not defined") {
		t.Errorf("undefined-variable message missing: %q", stderr)
	}
}

// TestSessionSave: `session save file` round-trips this session's units
// into a runnable script.
func TestSessionSave(t *testing.T) {
	t.Chdir(t.TempDir())
	stdout, stderr, code, _ := run(t,
		step{line: "greeting := \"hello\""},
		step{line: "if true {"},
		step{line: "fmt.Println(greeting)"},
		step{line: "}"},
		step{line: "session save replay.grsh"},
	)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "saved 2 unit(s)") {
		t.Errorf("save confirmation missing: %q", stdout)
	}
	b, err := os.ReadFile("replay.grsh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	if !strings.HasPrefix(script, "#!/usr/bin/env grsh\n") {
		t.Errorf("missing shebang: %q", script)
	}
	// The multi-line block must survive as real lines, not fragments.
	if !strings.Contains(script, "if true {\nfmt.Println(greeting)\n}") {
		t.Errorf("block not round-tripped: %q", script)
	}
	// And the saved script replays cleanly through a fresh session.
	var out bytes.Buffer
	sess := runner.NewSession(runner.Options{Stdout: &out, Stderr: &out})
	if err := sess.RunFile("replay.grsh"); err != nil {
		t.Fatalf("replay failed: %v", err)
	}
	if out.String() != "hello\n" {
		t.Errorf("replay output %q", out.String())
	}
}

// TestHistoryStoreRoundTrip: multi-line units survive escape/persist/load.
func TestHistoryStoreRoundTrip(t *testing.T) {
	path := t.TempDir() + "/units"
	h := openHistory(path)
	unit := "func f() {\n\techo \"back\\\\slash\"\n}"
	h.Append(unit)
	h2 := openHistory(path)
	us := h2.Units()
	if len(us) != 1 || us[0] != unit {
		t.Errorf("round trip = %q, want %q", us, unit)
	}
	if len(h2.SessionUnits()) != 0 {
		t.Error("loaded units must not count as this session's")
	}
}

// TestUserMsgCaret: errors with a resolvable line:col echo the offending
// source line with a caret under the column.
func TestUserMsgCaret(t *testing.T) {
	src := "x := 1\ny := x + nope"
	err := serr.New("undefined: nope", "loc", "<eval>:2:10")
	got := userMsg(src, err)
	want := "line 2: undefined: nope\n    y := x + nope\n             ^"
	if got != want {
		t.Errorf("userMsg = %q, want %q", got, want)
	}
	// No column → no caret block, prior behavior.
	err2 := serr.New("boom", "loc", "<eval>:2")
	if got := userMsg(src, err2); got != "line 2: boom" {
		t.Errorf("no-column userMsg = %q", got)
	}
}

// TestLoopBreadcrumbPrompt: continuation prompts must show the open
// construct trail and depth-based indent.
func TestLoopBreadcrumbPrompt(t *testing.T) {
	stdout, stderr, code, prompts := run(t,
		step{line: "func greet(name string) {"},
		step{line: "for i := range 2 {"},
		step{line: "fmt.Println(name, i)"},
		step{line: "}"},
		step{line: "}"},
		step{line: `greet("hi")`},
	)
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if stdout != "hi 0\nhi 1\n" {
		t.Errorf("stdout %q", stdout)
	}
	// prompts[0] primary; [1] after "func greet(...) {"; [2] after "for ... {".
	if !strings.Contains(prompts[1], "func greet ▸") {
		t.Errorf("prompt after func = %q, want a 'func greet ▸' breadcrumb", prompts[1])
	}
	if !strings.Contains(prompts[2], "func greet ▸ for ▸") {
		t.Errorf("prompt inside for = %q, want 'func greet ▸ for ▸'", prompts[2])
	}
	// Depth-2 indent inside the for body.
	if !strings.HasSuffix(prompts[2], strings.Repeat("  ", 2)) {
		t.Errorf("prompt inside for = %q, want trailing depth-2 indent", prompts[2])
	}
	// prompts[3] is shown before the first "}" — still inside the for.
	// prompts[4], after the inner } was read, is back to one construct.
	if strings.Contains(prompts[4], "for ▸") || !strings.Contains(prompts[4], "func greet ▸") {
		t.Errorf("prompt after inner close = %q, want only 'func greet ▸'", prompts[4])
	}
}

func TestLoopShellContinuation(t *testing.T) {
	stdout, _, code, _ := run(t,
		step{line: "echo one |"},
		step{line: "wc -l"},
	)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if strings.TrimSpace(stdout) != "1" {
		t.Errorf("stdout %q, want 1", stdout)
	}
}

func TestLoopExitBuiltin(t *testing.T) {
	_, _, code, _ := run(t, step{line: "exit 3"})
	if code != 3 {
		t.Errorf("exit code %d, want 3", code)
	}
}

func TestLoopEOFReturnsLastStatus(t *testing.T) {
	_, _, code, _ := run(t, step{line: "false"})
	if code != 1 {
		t.Errorf("exit code %d, want last status 1", code)
	}
}

func TestLoopRuntimeErrorContinues(t *testing.T) {
	stdout, stderr, code, _ := run(t,
		step{line: "fmt.Println(unknownIdent)"},
		step{line: `fmt.Println("still here")`},
	)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, "undefined") {
		t.Errorf("stderr %q, want undefined-identifier error", stderr)
	}
	if strings.Contains(stderr, "<eval>") || strings.Contains(stderr, "line 1:") {
		t.Errorf("stderr %q leaks the eval location on a single-line input", stderr)
	}
	if stdout != "still here\n" {
		t.Errorf("stdout %q — loop did not continue after the error", stdout)
	}
}

func TestLoopInterruptDropsContinuation(t *testing.T) {
	stdout, _, code, _ := run(t,
		step{line: "if true {"},
		step{line: "", err: errInterrupt},
		step{line: `fmt.Println("fresh")`},
	)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if stdout != "fresh\n" {
		t.Errorf("stdout %q — ^C should abandon the open block", stdout)
	}
}

func TestLoopEOFMidContinuationAbandons(t *testing.T) {
	stdout, _, code, _ := run(t,
		step{line: "if true {"},
		step{line: "", err: io.EOF},
		step{line: `fmt.Println("after")`},
	)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if stdout != "after\n" {
		t.Errorf("stdout %q — ^D mid-block should abandon it, not exit", stdout)
	}
}

// A job that finishes uncollected is announced before the next prompt.
// (An explicitly waited job is reaped silently — bash behavior.)
func TestLoopBackgroundJobNotification(t *testing.T) {
	stdout, stderr, code, _ := run(t,
		step{line: "true &"},
		// The pause lets the job finish; the notification prints when the
		// loop comes back around for the line after it.
		step{line: "# prompt cycle", delay: 500 * time.Millisecond},
		step{line: "# one more so the drain after the pause is observed"},
	)
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "[1]") || !strings.Contains(stdout, "Done") {
		t.Errorf("stdout %q, want a [1] Done notification", stdout)
	}
}

func TestLoopStatePersistsAcrossInputs(t *testing.T) {
	stdout, stderr, code, _ := run(t,
		step{line: "n := 2"},
		step{line: "n++"},
		step{line: "fmt.Println(n)"},
	)
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if stdout != "3\n" {
		t.Errorf("stdout %q, want 3", stdout)
	}
}
