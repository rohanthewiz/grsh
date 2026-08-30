package tutor

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/repl"
	"github.com/rohanthewiz/grsh/internal/runner"
)

// newTestEngine builds an engine over a headless session writing into
// one buffer, with color off so assertions match plain text.
func newTestEngine(t *testing.T, l Lesson) (*engine, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	cap := newCapture(4 << 10)
	sess := runner.NewSession(runner.Options{
		ScriptName: "tutor-test",
		Stdout:     io.MultiWriter(&out, cap),
		Stderr:     io.MultiWriter(&out, cap),
	})
	return newEngine(l, sess, cap, &out, false), &out
}

// submit drives one full input unit through the engine the way repl.loop
// does: BeforePrompt, then a real Eval against the live session, then
// AfterEval with whatever the eval returned.
func (e *engine) submit(src string) {
	e.BeforePrompt(e.out)
	err := e.sess.Eval(src)
	e.AfterEval(src, err)
}

func twoStepLesson() Lesson {
	return Lesson{
		ID:    "t",
		Title: "Test",
		Steps: []Step{
			{
				ID:       "one",
				Prose:    []string{"print hello"},
				Hints:    []string{"use echo"},
				Solution: "echo hello",
				Verify:   MustVerifier("output-regexp (?m)^hello$"),
			},
			{
				ID:       "two",
				Prose:    []string{"anything goes"},
				Solution: "echo whatever",
				Verify:   MustVerifier("any-input"),
			},
		},
	}
}

// TestContentSolutionsPass is the highest-value test in the package: every
// shipped step's canonical solution must satisfy that step's own verifier.
// A lesson whose answer doesn't work fails CI rather than a student.
//
// A chapter runs as a chapter: one fresh playground, one session, the
// solutions submitted IN ORDER — because that is the only thing a student
// can do, and because the curriculum is written that way on purpose.
// Chapter 4 captures a count into `errs` and splices it back three steps
// later; the capstone builds a report, saves the session as a script, and
// sources it again. Grading each step in isolation would forbid exactly
// the composition the language is for.
//
// Units go through repl.UnitLog rather than straight to Eval, so `?count`
// and `session save` — prompt affordances that never reach the
// interpreter — are checked on the same path the student's keystrokes
// take, not on a reimplementation of it that could quietly drift.
func TestContentSolutionsPass(t *testing.T) {
	for _, l := range lessons() {
		t.Run(l.ID, func(t *testing.T) {
			box, err := newSandbox()
			if err != nil {
				t.Fatalf("playground: %v", err)
			}
			defer box.cleanup()

			cap := newCapture(64 << 10)
			sess := runner.NewSession(runner.Options{
				ScriptName: "tutor-check",
				Stdout:     cap,
				Stderr:     cap,
			})
			units := repl.NewUnitLog()

			for i := range l.Steps {
				st := l.Steps[i]
				if st.Solution == "" {
					t.Errorf("%s: step has no solution to check", st.ID)
					continue
				}
				// The capture is reset per step exactly as BeforePrompt does
				// it, so a step is graded on its own output and never on the
				// previous step's leftovers.
				cap.Reset()
				evalErr := units.Submit(st.Solution, sess, io.Discard, io.Discard)
				a := Attempt{Input: st.Solution, Output: cap.String(), Err: evalErr, Sess: sess, Dir: box.dir}
				if !st.Verify.Verify(a) {
					t.Errorf("%s: solution %q failed its own verifier (%s)\noutput: %q\nerr: %v",
						st.ID, st.Solution, st.Verify.Spec(), cap.String(), evalErr)
				}
			}
		})
	}
}

// TestContentParses is the guard on the format itself. lessons() panics on
// a malformed chapter — the right call for content compiled into the
// binary — so this test is what turns that panic into a failing build
// instead of a failing `grsh tutor`.
func TestContentParses(t *testing.T) {
	all := lessons()
	if len(all) != 8 {
		t.Errorf("%d chapters embedded, want the 8 the curriculum plans", len(all))
	}
	for _, l := range all {
		if l.Title == "" {
			t.Errorf("%s: no title", l.ID)
		}
		if len(l.Steps) < 4 {
			t.Errorf("%s: %d steps — a chapter that short is probably a parse failure", l.ID, len(l.Steps))
		}
	}
}

// TestContentStepsAreWellFormed guards the invariants the engine assumes
// but cannot enforce: every step needs a stable, unique ID (progress
// records name steps by ID, so a duplicate would resume the wrong one)
// and a verifier (a nil one would panic mid-lesson).
func TestContentStepsAreWellFormed(t *testing.T) {
	for _, l := range lessons() {
		seen := map[string]bool{}
		for _, st := range l.Steps {
			switch {
			case st.ID == "":
				t.Errorf("%s: a step has no ID", l.ID)
			case seen[st.ID]:
				t.Errorf("%s: duplicate step ID %q — progress would resume the wrong step", l.ID, st.ID)
			}
			seen[st.ID] = true
			if st.Verify == nil {
				t.Errorf("%s/%s: no verifier", l.ID, st.ID)
			}
			if len(st.Prose) == 0 {
				t.Errorf("%s/%s: no prose — the panel would be blank", l.ID, st.ID)
			}
		}
	}
}

// TestEngineAdvancesOnCorrectAnswer walks a whole lesson and checks the
// state machine ends finished, with the outro printed once.
func TestEngineAdvancesOnCorrectAnswer(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())

	if _, done := e.Done(); done {
		t.Fatal("engine reports done before the first step")
	}
	e.submit("echo hello")
	if e.idx != 1 {
		t.Fatalf("idx = %d after a correct answer, want 1", e.idx)
	}
	e.submit("echo anything at all")
	if _, done := e.Done(); !done {
		t.Fatal("engine not done after the last step passed")
	}
	if n := strings.Count(out.String(), "Lesson complete."); n != 1 {
		t.Errorf("outro printed %d times, want 1\n%s", n, out.String())
	}
}

// TestEngineHintEscalation: silence on the first miss, then a hint, then
// the answer once hints run out — and a later pass still advances.
func TestEngineHintEscalation(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())

	e.submit("echo wrong")
	if strings.Contains(out.String(), "hint:") {
		t.Errorf("hint offered on the first miss:\n%s", out.String())
	}
	out.Reset()

	e.submit("echo wrong")
	if !strings.Contains(out.String(), "hint: use echo") {
		t.Errorf("no hint on the second miss:\n%s", out.String())
	}
	out.Reset()

	e.submit("echo wrong")
	if !strings.Contains(out.String(), "answer: echo hello") {
		t.Errorf("no answer after the hints ran out:\n%s", out.String())
	}
	out.Reset()

	e.submit("echo hello")
	if e.idx != 1 || e.attempts != 0 || e.revealed != 0 {
		t.Errorf("after passing: idx=%d attempts=%d revealed=%d, want 1/0/0", e.idx, e.attempts, e.revealed)
	}
}

// TestCaptureIsPerAttempt: the tee is cleared at each prompt, so step 2 is
// never graded on output step 1 produced.
func TestCaptureIsPerAttempt(t *testing.T) {
	l := twoStepLesson()
	// Make step two demand output that step one already produced, so a
	// leaking buffer would pass it without the user typing anything.
	l.Steps[1].Verify = MustVerifier("output-regexp (?m)^hello$")
	l.Steps[1].Solution = "echo hello"

	e, _ := newTestEngine(t, l)
	e.submit("echo hello") // step 1 passes, printing "hello"
	e.submit("echo other") // step 2 must NOT see step 1's output
	if e.idx != 1 {
		t.Errorf("idx = %d — step 2 was graded on stale captured output", e.idx)
	}
}

// TestPanelPrintedOncePerStep: repeated prompts (a bare Enter, an
// interrupt) must not re-post the panel; a new step must.
func TestPanelPrintedOncePerStep(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())
	e.BeforePrompt(out)
	e.BeforePrompt(out)
	if n := strings.Count(out.String(), "print hello"); n != 1 {
		t.Errorf("step 1 panel printed %d times, want 1", n)
	}
	e.submit("echo hello")
	e.BeforePrompt(out)
	if !strings.Contains(out.String(), "anything goes") {
		t.Errorf("step 2 panel never printed:\n%s", out.String())
	}
}

func TestVerifiers(t *testing.T) {
	tests := []struct {
		name string
		spec string
		a    Attempt
		want bool
	}{
		{"any-input accepts", "any-input", Attempt{Input: "x"}, true},
		{"any-input accepts an error", "any-input", Attempt{Err: io.EOF}, true},
		{"regexp matches trimmed output", "output-regexp ^hello$", Attempt{Output: "hello\n"}, true},
		{"regexp needs the anchor to hold", "output-regexp ^hello$", Attempt{Output: "say hello\n"}, false},
		{"multiline flag matches a line", "output-regexp (?m)^42$", Attempt{Output: "x\n42\ny\n"}, true},
		{"regexp fails on eval error", "output-regexp ^hello$", Attempt{Output: "hello", Err: io.EOF}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := MustVerifier(tc.spec)
			if got := v.Verify(tc.a); got != tc.want {
				t.Errorf("%s.Verify(%+v) = %v, want %v", tc.spec, tc.a, got, tc.want)
			}
		})
	}
}

func TestParseVerifierErrors(t *testing.T) {
	for _, spec := range []string{"", "nosuch-kind", "output-regexp", "output-regexp [unclosed", "any-input extra"} {
		if _, err := ParseVerifier(spec); err == nil {
			t.Errorf("ParseVerifier(%q) succeeded, want an error", spec)
		}
	}
}

// TestCaptureWindow: the tee drops the head rather than growing without
// bound, and keeps the tail a verifier is most likely to want.
func TestCaptureWindow(t *testing.T) {
	c := newCapture(8)
	c.Write([]byte("abcdefghij"))
	if got := c.String(); got != "cdefghij" {
		t.Errorf("capture = %q, want the last 8 bytes %q", got, "cdefghij")
	}
	c.Reset()
	if got := c.String(); got != "" {
		t.Errorf("capture after Reset = %q, want empty", got)
	}
}

func TestChapterIndex(t *testing.T) {
	tests := []struct {
		args    []string
		n       int
		want    int
		wantErr bool
	}{
		{nil, 3, 0, false},
		{[]string{"2"}, 3, 1, false},
		{[]string{"0"}, 3, 0, true},
		{[]string{"4"}, 3, 0, true},
		{[]string{"two"}, 3, 0, true},
	}
	for _, tc := range tests {
		got, err := chapterIndex(tc.args, tc.n)
		if (err != nil) != tc.wantErr {
			t.Errorf("chapterIndex(%q, %d) err = %v, wantErr %v", tc.args, tc.n, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("chapterIndex(%q, %d) = %d, want %d", tc.args, tc.n, got, tc.want)
		}
	}
}

// TestStyleNoColor: with color off, nothing emits an escape sequence —
// the NO_COLOR path is one boolean, not a second renderer.
func TestStyleNoColor(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())
	e.BeforePrompt(out)
	e.submit("echo hello")
	if strings.Contains(out.String(), "\x1b[") {
		t.Errorf("ANSI escape leaked with color disabled:\n%q", out.String())
	}
}
