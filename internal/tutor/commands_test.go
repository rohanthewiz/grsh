package tutor

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestColonIsACompleteUnit is the assumption the whole meta-command
// design rests on: the REPL loop only offers the interceptor a COMPLETE
// input unit, so if the classifier asked for a continuation after
// `:hint` the student would be left staring at a `...` prompt. It also
// pins the collision claim — a colon line classifies as shell, and no Go
// statement starts with one.
func TestColonIsACompleteUnit(t *testing.T) {
	sess, _ := liveSession(t)
	for _, src := range []string{":hint", ":sol", ":skip", ":back", ":menu", ":progress", ":help", ":quit"} {
		if sess.Pending(src).NeedsMore {
			t.Errorf("%q asks for a continuation; the tutor would hang at a `...` prompt", src)
		}
	}
}

// TestCommandOnlyClaimsColonLines: everything else must fall through to
// the real shell, or the tutor would be eating the student's work.
func TestCommandOnlyClaimsColonLines(t *testing.T) {
	e, _ := newTestEngine(t, twoStepLesson())
	for _, src := range []string{"echo hi", "n := 1", "?n", "session save x.grsh", "  ", "cat a:b"} {
		if e.Command(src) {
			t.Errorf("Command claimed %q; only colon lines are the tutor's", src)
		}
	}
	for _, src := range []string{":help", "  :help  ", ":hint"} {
		if !e.Command(src) {
			t.Errorf("Command did not claim %q", src)
		}
	}
}

// TestUnknownCommandIsStillClaimed: a mistyped `:hnt` is answered by the
// tutor. Handing it to the shell would have the thing that invented colon
// commands reply "command not found".
func TestUnknownCommandIsStillClaimed(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())
	if !e.Command(":hnt") {
		t.Fatal(":hnt should be claimed by the tutor")
	}
	if !strings.Contains(out.String(), "no such tutor command") {
		t.Errorf("output = %q", out.String())
	}
}

func TestHintCommandEscalatesThenStops(t *testing.T) {
	l := twoStepLesson()
	l.Steps[0].Hints = []string{"first hint", "second hint"}
	e, out := newTestEngine(t, l)

	e.Command(":hint")
	if !strings.Contains(out.String(), "first hint") || strings.Contains(out.String(), "second hint") {
		t.Fatalf("first :hint gave: %q", out.String())
	}
	out.Reset()
	e.Command(":hint")
	if !strings.Contains(out.String(), "second hint") {
		t.Fatalf("second :hint gave: %q", out.String())
	}
	// Out of hints: say so rather than silently doing nothing.
	out.Reset()
	e.Command(":hint")
	if !strings.Contains(out.String(), "no hints left") {
		t.Errorf("third :hint gave: %q", out.String())
	}
}

// TestSolutionDoesNotAdvance: reading a command and typing it are
// different acts, and only the second builds the muscle memory the
// tutorial exists for.
func TestSolutionDoesNotAdvance(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())
	e.Command(":sol")
	if e.idx != 0 {
		t.Errorf(":sol advanced to step %d; it must not", e.idx)
	}
	if !strings.Contains(out.String(), "echo hello") {
		t.Errorf(":sol did not print the answer: %q", out.String())
	}
	// Hints are moot once the answer is out, so :hint stops offering.
	out.Reset()
	e.Command(":hint")
	if strings.Contains(out.String(), "use echo") {
		t.Errorf(":hint re-offered a hint after :sol: %q", out.String())
	}
}

func TestSkipAdvancesAndIsCounted(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())
	e.Command(":skip")
	if e.idx != 1 {
		t.Fatalf("after :skip idx = %d, want 1", e.idx)
	}
	if e.skipped != 1 {
		t.Errorf("skipped = %d, want 1", e.skipped)
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("output = %q", out.String())
	}
	// Skipping the last step finishes the lesson, exactly as passing it
	// would — the outro is about reaching the end, not about a score.
	e.Command(":skip")
	if _, done := e.Done(); !done {
		t.Error("skipping the final step should end the lesson")
	}
}

func TestBackReturnsAndReposts(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())
	e.Command(":back")
	if !strings.Contains(out.String(), "already at the first step") {
		t.Errorf(":back at step 1 gave: %q", out.String())
	}

	e.submit("echo hello") // pass step 1
	out.Reset()
	e.Command(":back")
	if e.idx != 0 {
		t.Fatalf("after :back idx = %d, want 0", e.idx)
	}
	// The panel must be re-posted, and the hint state reset: a student who
	// goes back to re-read deserves the same silence they had the first
	// time.
	if e.posted || e.revealed != 0 || e.attempts != 0 {
		t.Errorf("after :back posted=%v revealed=%d attempts=%d, want a clean step", e.posted, e.revealed, e.attempts)
	}
	e.BeforePrompt(e.out)
	if !strings.Contains(out.String(), "print hello") {
		t.Errorf("panel not re-posted after :back: %q", out.String())
	}
}

// TestBackUnfinishesTheLesson: stepping out of the outro must clear the
// finished flag, or Done would end the session at the very next prompt.
func TestBackUnfinishesTheLesson(t *testing.T) {
	e, _ := newTestEngine(t, twoStepLesson())
	e.submit("echo hello")
	e.submit("anything")
	if _, done := e.Done(); !done {
		t.Fatal("lesson should be finished")
	}
	e.Command(":back")
	if _, done := e.Done(); done {
		t.Error(":back left the lesson finished; the loop would exit immediately")
	}
}

func TestQuitEndsTheSessionAtZero(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())
	if _, done := e.Done(); done {
		t.Fatal("a fresh lesson is not done")
	}
	e.Command(":quit")
	code, done := e.Done()
	if !done {
		t.Fatal(":quit did not end the session")
	}
	// Walking out of a tutorial is a choice, not a failure: a nonzero
	// code would break `grsh tutor && next-thing` and teach nobody
	// anything.
	if code != 0 {
		t.Errorf(":quit exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "your place is saved") {
		t.Errorf("output = %q", out.String())
	}
}

func TestMenuAndProgressAndHelp(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson())
	e.chapters = []Lesson{twoStepLesson(), {ID: "second", Title: "Second chapter", Steps: []Step{{ID: "x"}}}}

	e.Command(":menu")
	menu := out.String()
	if !strings.Contains(menu, "Second chapter") || !strings.Contains(menu, "grsh tutor N") {
		t.Errorf(":menu = %q", menu)
	}

	out.Reset()
	e.Command(":progress")
	if got := out.String(); !strings.Contains(got, "step 1 of 2") {
		t.Errorf(":progress = %q", got)
	}

	out.Reset()
	e.Command(":help")
	help := out.String()
	for _, c := range metaCommands() {
		if !strings.Contains(help, c.name) {
			t.Errorf(":help omits %s: %q", c.name, help)
		}
	}
}

// TestCommandsPersistProgress: the escalation a student earned has to
// survive the restart, which is the whole reason :hint writes at all.
func TestCommandsPersistProgress(t *testing.T) {
	e, _ := newTestEngine(t, twoStepLesson())
	store := openStore(filepath.Join(t.TempDir(), "p.db"))
	defer store.Close()
	e.store = store

	e.Command(":hint")
	rec, ok := store.Load(e.lesson.ID)
	if !ok {
		t.Fatal("no record saved after :hint")
	}
	if rec.Step != "one" || rec.Revealed != 1 {
		t.Errorf("saved %+v, want step one with 1 hint revealed", rec)
	}

	e.submit("echo hello")
	rec, _ = store.Load(e.lesson.ID)
	if rec.Step != "two" || rec.Revealed != 0 {
		t.Errorf("after passing step 1, saved %+v, want step two with a clean hint state", rec)
	}
}

// TestNoColorLeaksFromCommands: every meta-command renders through the
// style helpers, so NO_COLOR is one boolean rather than a second code
// path. An escape byte here means someone hard-coded one.
func TestNoColorLeaksFromCommands(t *testing.T) {
	e, out := newTestEngine(t, twoStepLesson()) // built with color off
	e.chapters = []Lesson{twoStepLesson()}
	for _, c := range metaCommands() {
		e.Command(c.name)
	}
	e.Command(":nope")
	if strings.Contains(out.String(), "\x1b[") {
		t.Errorf("ANSI escape leaked with color off: %q", out.String())
	}
}
