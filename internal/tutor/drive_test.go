package tutor

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// newTestDriver starts a driver on a chapter with colour off and a fixed
// width, writing its transcript into the returned buffer.
//
// No Store: a test must not read or write the developer's own
// ~/.grsh_tutor.db, and resume is covered by the resume policy's own test.
func newTestDriver(t *testing.T, chapter int) (*Driver, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	d, err := NewDriver(&buf, chapter, DriverOptions{Width: 64, Embedded: true})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(d.Close)
	return d, &buf
}

// submit feeds a possibly multi-line solution the way a host does: one
// physical line at a time, letting the driver decide when the unit is
// complete. A test that handed over the whole block at once would skip the
// continuation path entirely, which is exactly where a headless host is
// most likely to be wrong.
func submit(d *Driver, src string) {
	for _, line := range strings.Split(src, "\n") {
		d.Submit(line)
	}
}

// TestDriverRunsTheCurriculum is the web tour's version of
// TestContentSolutionsPass, and the reason the tour needs no content of
// its own: every shipped step's canonical solution, submitted through the
// headless driver, must tick that step over.
//
// It is worth having BOTH: the content check proves the lessons are right,
// and this proves the driver reproduces repl.loop faithfully enough to
// grade them. A driver that dropped the capture reset, mis-ordered Command
// against Eval, or lost the continuation buffer would still pass the first
// and fail here.
func TestDriverRunsTheCurriculum(t *testing.T) {
	for i, l := range lessons() {
		t.Run(l.ID, func(t *testing.T) {
			d, buf := newTestDriver(t, i)
			for n := range l.Steps {
				st := l.Steps[n]
				if got := d.View().Step; got != n+1 {
					t.Fatalf("at %s: on step %d, want %d\n%s", st.ID, got, n+1, buf.String())
				}
				submit(d, st.Solution)
				if d.View().Attempts != 0 {
					t.Errorf("%s: solution %q was graded a miss\n%s", st.ID, st.Solution, buf.String())
				}
			}
			// One past the last step is how a finished chapter reads.
			if v := d.View(); !v.Finished || v.Step != len(l.Steps)+1 {
				t.Errorf("chapter not finished: step %d/%d finished=%v", v.Step, v.Steps, v.Finished)
			}
		})
	}
}

// TestDriverContinuation: an input unit may span several lines, and the
// driver has to hold them the way the loop's stack does — reporting
// Pending in between so a host can draw a continuation prompt, and
// grading only once the block closes.
func TestDriverContinuation(t *testing.T) {
	d, buf := newTestDriver(t, 2) // chapter 3: Go at the prompt

	d.Submit("for i := range 3 {")
	if !d.View().Pending {
		t.Fatal("an unclosed block did not leave the driver pending")
	}
	d.Submit("    fmt.Println(i)")
	if !d.View().Pending {
		t.Fatal("still-open block reported complete")
	}
	d.Submit("}")
	if d.View().Pending {
		t.Error("closed block still pending")
	}
	if out := buf.String(); !strings.Contains(out, "0\n1\n2") {
		t.Errorf("loop did not run: %q", out)
	}
	// The echo shows the unit as it was typed: a prompt marker on the first
	// line and a continuation marker on the rest.
	if out := buf.String(); !strings.Contains(out, "▸ for i := range 3 {") ||
		!strings.Contains(out, "… }") {
		t.Errorf("unit not echoed as a multi-line unit: %q", out)
	}
}

// TestDriverWithholdsTheAnswer: hints and the solution reach a host only
// once the student has earned or asked for them. Anything the View carries
// is readable in the page source, so "revealed" has to mean revealed.
func TestDriverWithholdsTheAnswer(t *testing.T) {
	d, _ := newTestDriver(t, 0)

	v := d.View()
	if len(v.Hints) != 0 || v.Solution != "" {
		t.Fatalf("step 1 gave away help unasked: hints=%q solution=%q", v.Hints, v.Solution)
	}
	if !v.HasSolution {
		t.Error("HasSolution should say an answer exists without disclosing it")
	}

	d.Submit(":hint")
	if v := d.View(); len(v.Hints) != 1 {
		t.Errorf(":hint revealed %d hints, want 1", len(v.Hints))
	}
	if d.View().Solution != "" {
		t.Error("a hint disclosed the solution")
	}

	d.Submit(":sol")
	if d.View().Solution == "" {
		t.Error(":sol did not disclose the solution")
	}
	// Moving on re-arms the withholding for the next step.
	submit(d, d.View().Solution)
	if v := d.View(); v.Solution != "" || len(v.Hints) != 0 {
		t.Errorf("help carried over into the next step: hints=%q solution=%q", v.Hints, v.Solution)
	}
}

// TestDriverJumpsChapters: `:next` is a teardown and a rebuild here too —
// a new chapter, a new playground, and none of the last one's files.
func TestDriverJumpsChapters(t *testing.T) {
	d, _ := newTestDriver(t, 0)
	first := d.Dir()

	d.Submit(":next")
	v := d.View()
	if v.Chapter != 1 {
		t.Fatalf("after :next, chapter %d, want 1", v.Chapter)
	}
	if v.Step != 1 {
		t.Errorf("new chapter started at step %d, want 1", v.Step)
	}
	if d.Dir() == first {
		t.Error("the new chapter reused the old playground")
	}
	if v.Title != d.View().Chapters[1].Title {
		t.Errorf("title %q does not match the table of contents", v.Title)
	}
}

// TestDriverMarksOnlyFinishedChapters: the table of contents must tick off
// what the student did, not what they skipped past. Jumping from chapter 1
// to chapter 4 finishes nothing, and a UI that inferred otherwise would be
// congratulating them on work they never saw.
func TestDriverMarksOnlyFinishedChapters(t *testing.T) {
	d, _ := newTestDriver(t, 0)

	d.Submit(":menu 4")
	for i, ch := range d.View().Chapters {
		if ch.Done {
			t.Errorf("chapter %d marked done after a jump past it", i+1)
		}
	}

	// Finish chapter 4 for real, and it ticks over immediately — before
	// the student leaves it, which is when they want to see it.
	for _, st := range lessons()[3].Steps {
		submit(d, st.Solution)
	}
	v := d.View()
	if !v.Chapters[3].Done {
		t.Error("a finished chapter did not tick over")
	}
	if v.Chapters[0].Done {
		t.Error("finishing chapter 4 marked chapter 1 done")
	}

	// And it survives the move to another chapter.
	d.Submit(":menu 2")
	if !d.View().Chapters[3].Done {
		t.Error("the tick was lost when the chapter was torn down")
	}
}

// TestDriverClassifies covers the half of `--explain` a browser can show:
// chapter 2 asks for the classifier's live verdict, and a host with no
// prompt to decorate gets it as text.
func TestDriverClassifies(t *testing.T) {
	plain, _ := newTestDriver(t, 0)
	if got := plain.Classify("ls"); got != "" {
		t.Errorf("a chapter without `explain: on` classified anyway: %q", got)
	}

	d, _ := newTestDriver(t, 1) // chapter 2 carries `explain: on`
	if !d.View().Explain {
		t.Fatal("chapter 2's explain directive did not reach the View")
	}
	if got := d.Classify("wc -l access.log"); !strings.HasPrefix(got, "shell · rule=") {
		t.Errorf("Classify(shell) = %q", got)
	}
	if got := d.Classify("n := 42"); !strings.HasPrefix(got, "go · rule=") {
		t.Errorf("Classify(go) = %q", got)
	}
	if got := d.Classify("   "); got != "" {
		t.Errorf("blank input classified: %q", got)
	}
}

// TestDriversKeepSeparatePlaygrounds is the test for the eval gate.
//
// A grsh session's working directory is the PROCESS's working directory,
// so two drivers cannot both be standing in their own playground at once.
// They take turns instead, and the whole point is that neither can tell:
// interleaved commands must each see their own fixtures, and a `cd` in one
// must not move the other.
func TestDriversKeepSeparatePlaygrounds(t *testing.T) {
	a, bufA := newTestDriver(t, 0)
	b, bufB := newTestDriver(t, 0)
	if a.Dir() == b.Dir() {
		t.Fatal("two drivers shared one playground")
	}

	// Interleaved, and each writes a file only its own playground should
	// have — a check that survives both of them printing the same thing.
	a.Submit("echo a-was-here > mine.txt")
	b.Submit("echo b-was-here > mine.txt")
	bufA.Reset()
	bufB.Reset()
	a.Submit("cat mine.txt")
	b.Submit("cat mine.txt")
	if got := bufA.String(); !strings.Contains(got, "a-was-here") {
		t.Errorf("driver A read someone else's file: %q", got)
	}
	if got := bufB.String(); !strings.Contains(got, "b-was-here") {
		t.Errorf("driver B read someone else's file: %q", got)
	}

	// A cd is per-driver too, because each one re-enters where it left off.
	a.Submit("cd notes")
	bufA.Reset()
	bufB.Reset()
	a.Submit("pwd")
	b.Submit("pwd")
	if got := bufA.String(); !strings.Contains(got, a.Dir()) {
		t.Errorf("driver A's cd did not stick: %q", got)
	}
	if got := bufB.String(); strings.Contains(got, "/notes") {
		t.Errorf("driver A's cd moved driver B: %q", got)
	}
}

// TestDriversRunConcurrently exercises the same gate under the race
// detector, from the goroutines a web host actually serves requests on.
func TestDriversRunConcurrently(t *testing.T) {
	const n = 4
	var wg sync.WaitGroup
	for range n {
		d, buf := newTestDriver(t, 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				d.Submit("ls *.go | wc -l")
			}
			if got := buf.String(); !strings.Contains(got, "3") {
				t.Errorf("playground fixtures missing: %q", got)
			}
		}()
	}
	wg.Wait()
}

// TestDriverEndsOnExit: `exit` at the prompt ends the run rather than
// taking the host process down with it, and Submit goes quiet afterwards.
func TestDriverEndsOnExit(t *testing.T) {
	d, _ := newTestDriver(t, 0)
	d.Submit("exit 3")
	v := d.View()
	if !v.Ended || v.Code != 3 {
		t.Fatalf("exit 3 gave ended=%v code=%d", v.Ended, v.Code)
	}
	d.Submit("echo still-here") // must be ignored, not panic
	if !d.View().Ended {
		t.Error("a submit after the end revived the run")
	}
}

// TestDriverQuits: `:quit` ends the run the same way, at code 0 — walking
// out of a tutorial is a choice, not a failure.
func TestDriverQuits(t *testing.T) {
	d, _ := newTestDriver(t, 0)
	d.Submit(":quit")
	if v := d.View(); !v.Ended || v.Code != 0 {
		t.Errorf(":quit gave ended=%v code=%d", v.Ended, v.Code)
	}
}
