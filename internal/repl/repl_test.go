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
	return runIntercepted(t, nil, steps...)
}

// runIntercepted is run with a loop Interceptor attached (nil = plain REPL),
// so the tutor seam is exercised by the same scripted-reader harness.
func runIntercepted(t *testing.T, ic Interceptor, steps ...step) (stdout, stderr string, code int, prompts []string) {
	t.Helper()
	var out, errB bytes.Buffer
	sess := runner.NewSession(runner.Options{Stdout: &out, Stderr: &errB, ScriptName: "repl"})
	rd := &fakeReader{steps: steps}
	code = loop(sess, rd, &out, &errB, openHistory(""), ic)
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

// stubInterceptor records the loop's hook calls in order so the seam's
// contract can be asserted: one BeforePrompt per input *unit* (not per
// continuation line), one AfterEval per evaluated unit, and Done polled
// before each panel.
type stubInterceptor struct {
	calls    []string
	evals    []string
	errs     []error
	stopAt   int // stop the loop once this many AfterEvals have run (0 = never)
	stopCode int
	nEvals   int
	cmds     []string // units the stub claimed via Command
}

func (s *stubInterceptor) BeforePrompt(w io.Writer) {
	s.calls = append(s.calls, "before")
	io.WriteString(w, "[panel]\n")
}

// Command stands in for the tutor's colon meta-commands: it swallows
// any unit starting with ":" so the test can assert such a unit never
// reaches the classifier, Eval, or unit history.
func (s *stubInterceptor) Command(src string) bool {
	if !strings.HasPrefix(strings.TrimSpace(src), ":") {
		return false
	}
	s.calls = append(s.calls, "command")
	s.cmds = append(s.cmds, src)
	return true
}

func (s *stubInterceptor) AfterEval(src string, err error) {
	s.calls = append(s.calls, "after")
	s.evals = append(s.evals, src)
	s.errs = append(s.errs, err)
	s.nEvals++
}

func (s *stubInterceptor) Done() (int, bool) {
	s.calls = append(s.calls, "done")
	if s.stopAt > 0 && s.nEvals >= s.stopAt {
		return s.stopCode, true
	}
	return 0, false
}

// TestInterceptorUnitGranularity: a four-line multi-line block is ONE
// unit, so the tutor sees one panel and one grade — not four of each.
func TestInterceptorUnitGranularity(t *testing.T) {
	ic := &stubInterceptor{}
	stdout, stderr, code, _ := runIntercepted(t, ic,
		step{line: "x := 41"},
		step{line: "if x > 40 {"},
		step{line: `fmt.Println("big")`},
		step{line: "}"},
	)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	// 2 units in, 2 units evaluated.
	if got := len(ic.evals); got != 2 {
		t.Fatalf("AfterEval called %d times, want 2: %q", got, ic.evals)
	}
	if want := "if x > 40 {\nfmt.Println(\"big\")\n}"; ic.evals[1] != want {
		t.Errorf("second unit = %q, want %q", ic.evals[1], want)
	}
	// The panel must not repeat on continuation lines: two units plus the
	// final prompt the loop offers before hitting EOF. (The tutor itself
	// prints a panel once per *step*, so extra BeforePrompts are no-ops
	// there — what matters is that continuation lines produce none.)
	if n := strings.Count(stdout, "[panel]"); n != 3 {
		t.Errorf("panel printed %d times, want 3 (2 units + the final prompt)\n%s", n, stdout)
	}
	// Every unit is preceded by Done then BeforePrompt.
	want := []string{"done", "before", "after", "done", "before", "after", "done", "before"}
	if strings.Join(ic.calls, ",") != strings.Join(want, ",") {
		t.Errorf("hook order = %v, want %v", ic.calls, want)
	}
}

// TestInterceptorSeesEvalError: a failing unit still reaches AfterEval,
// carrying the error, so a verifier can grade "this was supposed to fail".
func TestInterceptorSeesEvalError(t *testing.T) {
	ic := &stubInterceptor{}
	_, _, _, _ = runIntercepted(t, ic, step{line: "nope := undefinedThing"})
	if len(ic.errs) != 1 {
		t.Fatalf("AfterEval called %d times, want 1", len(ic.errs))
	}
	if ic.errs[0] == nil {
		t.Error("AfterEval got a nil error for a failing unit")
	}
}

// TestInterceptorGradesReplCommands: `?name` never reaches Eval, but it
// is still a completed unit a lesson step can be built on.
func TestInterceptorGradesReplCommands(t *testing.T) {
	ic := &stubInterceptor{}
	_, _, _, _ = runIntercepted(t, ic,
		step{line: "xs := []string{\"a\"}"},
		step{line: "?xs"},
	)
	if len(ic.evals) != 2 || ic.evals[1] != "?xs" {
		t.Fatalf("evals = %q, want the ?xs unit graded too", ic.evals)
	}
	if ic.errs[1] != nil {
		t.Errorf("repl command reported error %v, want nil", ic.errs[1])
	}
}

// TestInterceptorDoneEndsLoop: Done ends the session with its code even
// though the scripted reader still has lines queued.
func TestInterceptorDoneEndsLoop(t *testing.T) {
	ic := &stubInterceptor{stopAt: 1, stopCode: 7}
	_, _, code, _ := runIntercepted(t, ic,
		step{line: "echo one"},
		step{line: "echo two"},
	)
	if code != 7 {
		t.Fatalf("exit %d, want 7 (Done's code)", code)
	}
	if len(ic.evals) != 1 {
		t.Errorf("evals = %q, want only the first unit", ic.evals)
	}
}

// TestInterceptorCtrlCDropsUnit: ^C mid-continuation abandons the unit
// without a phantom grade, and the next unit gets a fresh panel.
func TestInterceptorCtrlCDropsUnit(t *testing.T) {
	ic := &stubInterceptor{}
	_, _, _, _ = runIntercepted(t, ic,
		step{line: "if true {"},
		step{line: "", err: errInterrupt},
		step{line: "echo after"},
	)
	if len(ic.evals) != 1 || ic.evals[0] != "echo after" {
		t.Fatalf("evals = %q, want only the post-interrupt unit", ic.evals)
	}
	if n := strings.Count(strings.Join(ic.calls, ","), "before"); n != 3 {
		t.Errorf("BeforePrompt fired %d times, want 3 (initial, post-^C, post-eval)", n)
	}
}

// TestInterceptorCommandClaimsUnit: a unit the interceptor claims never
// reaches Eval, never reaches AfterEval, and — the part that matters for
// the capstone chapter — never lands in unit history, which `session
// save` turns into a runnable script.
func TestInterceptorCommandClaimsUnit(t *testing.T) {
	var out, errB bytes.Buffer
	sess := runner.NewSession(runner.Options{Stdout: &out, Stderr: &errB, ScriptName: "repl"})
	hist := openHistory("")
	ic := &stubInterceptor{}
	rd := &fakeReader{steps: []step{
		{line: "echo one"},
		{line: ":hint"},
		{line: "echo two"},
	}}
	loop(sess, rd, &out, &errB, hist, ic)

	if len(ic.cmds) != 1 || ic.cmds[0] != ":hint" {
		t.Fatalf("Command claimed %q, want [\":hint\"]", ic.cmds)
	}
	if len(ic.evals) != 2 || ic.evals[0] != "echo one" || ic.evals[1] != "echo two" {
		t.Errorf("evals = %q, want the two shell units only", ic.evals)
	}
	// A claimed unit is not shell, so it must not have run: `:hint` as a
	// command would print nothing but would set a status and pollute the
	// transcript the student is about to save.
	if strings.Contains(out.String(), "hint") {
		t.Errorf("claimed unit reached the shell; stdout = %q", out.String())
	}
	for _, u := range hist.SessionUnits() {
		if strings.TrimSpace(u) == ":hint" {
			t.Errorf("meta-command entered unit history: %q", hist.SessionUnits())
		}
	}
}
