package tutor

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/repl"
	"github.com/rohanthewiz/grsh/internal/runner"
)

// threeChapters is the fixture the navigation tests run on. A fixture
// rather than the shipped curriculum on purpose: chapter movement is
// engine behaviour, and pinning it to real content would make every
// curriculum edit look like a navigation regression.
func threeChapters() []Lesson {
	return []Lesson{
		{ID: "01-a", Title: "First", Steps: []Step{{ID: "a1"}}},
		{ID: "02-b", Title: "Second", Steps: []Step{{ID: "b1"}}},
		{ID: "03-c", Title: "Third", Steps: []Step{{ID: "c1"}}},
	}
}

// wired builds an engine that believes it is chapter idx of the fixture
// curriculum — which is what turns on :next, :menu N, and the
// chapter-aware outro.
func wired(t *testing.T, idx int) (*engine, *strings.Builder) {
	t.Helper()
	all := threeChapters()
	e, _ := newTestEngine(t, all[idx])
	var out strings.Builder
	e.out = &out
	e.chapters, e.chIdx = all, idx
	return e, &out
}

// TestNextRequestsTheFollowingChapter: the engine records the jump and
// ends the loop; it never switches in place, because a new chapter needs
// a new playground and a new session and only tutor.chapter can build
// those.
func TestNextRequestsTheFollowingChapter(t *testing.T) {
	e, out := wired(t, 0)
	if _, done := e.Done(); done {
		t.Fatal("a fresh chapter is not done")
	}
	e.Command(":next")
	if e.jump != 1 {
		t.Fatalf("jump = %d after :next, want chapter index 1", e.jump)
	}
	code, done := e.Done()
	if !done || code != 0 {
		t.Errorf("Done = (%d, %v) after :next, want (0, true)", code, done)
	}
	if !strings.Contains(out.String(), "chapter 2: Second") {
		t.Errorf(":next did not announce the chapter: %q", out.String())
	}
}

// TestNextOnTheLastChapterSaysSo rather than silently doing nothing.
func TestNextOnTheLastChapterSaysSo(t *testing.T) {
	e, out := wired(t, 2)
	e.Command(":next")
	if e.jump != -1 {
		t.Errorf("jump = %d on the last chapter, want -1 (no jump)", e.jump)
	}
	if !strings.Contains(out.String(), "last chapter") {
		t.Errorf("output = %q", out.String())
	}
}

// TestMenuJumpValidates: `:menu N` is a jump, and a fat-fingered number
// is answered rather than obeyed.
func TestMenuJumpValidates(t *testing.T) {
	tests := []struct {
		arg      string
		wantJump int
		wantSay  string
	}{
		{"3", 2, "chapter 3: Third"},
		{"1", 0, "chapter 1: First"},
		{"2", -1, "already in that chapter"}, // the engine is chapter 2
		{"9", -1, "have 1..3"},
		{"0", -1, "have 1..3"},
		{"two", -1, "have 1..3"},
	}
	for _, tc := range tests {
		e, out := wired(t, 1)
		e.Command(":menu " + tc.arg)
		if e.jump != tc.wantJump {
			t.Errorf(":menu %s: jump = %d, want %d", tc.arg, e.jump, tc.wantJump)
		}
		if !strings.Contains(out.String(), tc.wantSay) {
			t.Errorf(":menu %s: output = %q, want it to mention %q", tc.arg, out.String(), tc.wantSay)
		}
	}
}

// TestFinishedChapterHoldsThePrompt is the rule the outro depends on: it
// offers `:next`, so the session must still be there to take it. Without
// a following chapter — the single-lesson case the unit tests build —
// the tutor still closes itself, since there is nothing left to offer.
func TestFinishedChapterHoldsThePrompt(t *testing.T) {
	e, out := wired(t, 0)
	e.Command(":skip") // the fixture chapter has one step
	if !e.finished {
		t.Fatal("skipping the only step should finish the chapter")
	}
	if _, done := e.Done(); done {
		t.Error("the session ended while the outro was offering :next")
	}
	if !strings.Contains(out.String(), "Chapter 1 of 3 complete.") {
		t.Errorf("outro = %q", out.String())
	}
	if !strings.Contains(out.String(), "2. Second") {
		t.Errorf("outro does not name the next chapter: %q", out.String())
	}

	last, lastOut := wired(t, 2)
	last.Command(":skip")
	if _, done := last.Done(); !done {
		t.Error("the final chapter should end the session by itself")
	}
	if !strings.Contains(lastOut.String(), "Lesson complete.") {
		t.Errorf("final outro = %q", lastOut.String())
	}
}

// TestResumeChapter covers the policy behind a bare `grsh tutor`: carry
// on from the FURTHEST chapter touched, which is not the same as the
// first one left unfinished.
func TestResumeChapter(t *testing.T) {
	all := threeChapters()
	// records is a stand-in store: lesson ID → saved step, where "" is
	// the empty step a finished chapter writes.
	tests := []struct {
		name        string
		records     map[string]string
		wantIdx     int
		wantResumed bool
	}{
		{"nothing saved", nil, 0, false},
		{"mid-chapter", map[string]string{"01-a": "a1"}, 0, true},
		{"finished one, carry on", map[string]string{"01-a": ""}, 1, true},
		// The furthest chapter wins over the first unfinished one: a
		// student who jumped to chapter 3 on purpose wants chapter 3 back,
		// not chapter 1 because they skipped the basics.
		{"jumped ahead", map[string]string{"03-c": "c1"}, 2, true},
		{"finished ahead", map[string]string{"01-a": "", "02-b": ""}, 2, true},
		// Graduated: start the curriculum over rather than dropping the
		// student back onto the completion banner.
		{"all finished", map[string]string{"01-a": "", "02-b": "", "03-c": ""}, 0, false},
	}
	for _, tc := range tests {
		load := func(id string) (Record, bool) {
			step, ok := tc.records[id]
			return Record{Lesson: id, Step: step}, ok
		}
		idx, resumed := resumeChapter(all, load)
		if idx != tc.wantIdx || resumed != tc.wantResumed {
			t.Errorf("%s: resumeChapter = (%d, %v), want (%d, %v)", tc.name, idx, resumed, tc.wantIdx, tc.wantResumed)
		}
	}
}

// keepEngine is an engine on a chapter with a keepsake file, with the
// file already written into a stand-in playground.
func keepEngine(t *testing.T) (*engine, *strings.Builder, string) {
	t.Helper()
	all := threeChapters()
	all[2].Keep = "report.grsh"
	e, _ := newTestEngine(t, all[2])
	var out strings.Builder
	e.out, e.chapters, e.chIdx = &out, all, 2
	e.dir = t.TempDir()
	body := "#!/usr/bin/env grsh\nfmt.Println(\"hi\")\n"
	if err := os.WriteFile(filepath.Join(e.dir, "report.grsh"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return e, &out, body
}

// TestKeepSavesTheScriptOut: the capstone's script is the one thing that
// outlives the playground, and it leaves only when the student asks.
func TestKeepSavesTheScriptOut(t *testing.T) {
	e, out, body := keepEngine(t)
	dest := filepath.Join(t.TempDir(), "kept.grsh")

	e.Command(":keep " + dest)
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("nothing saved: %v (output %q)", err, out.String())
	}
	if string(got) != body {
		t.Errorf("saved %q, want %q", got, body)
	}
	// A saved script the student cannot run is a souvenir, not a tool.
	fi, err := os.Stat(dest)
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("saved mode = %v, want the owner-execute bit set", fi.Mode())
	}
	if !strings.Contains(out.String(), "saved to "+dest) {
		t.Errorf("output = %q", out.String())
	}
}

// TestKeepNeverOverwrites. A tutorial writing into someone's home is
// already pushing its luck; doing it over an existing file would not be
// defensible.
func TestKeepNeverOverwrites(t *testing.T) {
	e, out, _ := keepEngine(t)
	dest := filepath.Join(t.TempDir(), "kept.grsh")
	if err := os.WriteFile(dest, []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	e.Command(":keep " + dest)
	got, _ := os.ReadFile(dest)
	if string(got) != "precious\n" {
		t.Errorf(":keep clobbered an existing file: %q", got)
	}
	if !strings.Contains(out.String(), "already exists") {
		t.Errorf("output = %q", out.String())
	}
	if e.keepDone {
		t.Error("a refused :keep must not count as done — the student has to be able to retry")
	}
}

// TestKeepHoldsAndThenReleasesTheFinalPrompt: the final chapter's outro
// offers :keep, so the session waits for it — and once the file is out,
// there is nothing left to offer and the tutor closes itself.
func TestKeepHoldsAndThenReleasesTheFinalPrompt(t *testing.T) {
	e, out, _ := keepEngine(t)
	e.Command(":skip") // finish the (one-step) final chapter
	if !strings.Contains(out.String(), "report.grsh") {
		t.Errorf("the outro did not offer the file: %q", out.String())
	}
	if _, done := e.Done(); done {
		t.Fatal("the session ended while the outro was offering :keep")
	}
	e.Command(":keep " + filepath.Join(t.TempDir(), "kept.grsh"))
	if _, done := e.Done(); !done {
		t.Error("the session should close once the file has been saved out")
	}
}

// TestKeepWithoutTheFile explains rather than failing silently: the step
// that writes the script may simply not have been run yet.
func TestKeepWithoutTheFile(t *testing.T) {
	e, out, _ := keepEngine(t)
	os.Remove(filepath.Join(e.dir, "report.grsh"))
	e.Command(":keep")
	if !strings.Contains(out.String(), "no report.grsh in the playground") {
		t.Errorf("output = %q", out.String())
	}

	plain, plainOut := wired(t, 0) // a chapter with nothing to keep
	plain.Command(":keep")
	if !strings.Contains(plainOut.String(), "no file to keep") {
		t.Errorf("output = %q", plainOut.String())
	}
}

// TestKeepDest resolves the destination the way a shell user expects:
// home by default, ~ expanded, and a directory argument taking the
// file's own name (what `cp` would do).
func TestKeepDest(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	dir := t.TempDir()
	tests := []struct{ arg, want string }{
		{"", filepath.Join(home, "report.grsh")},
		{"~", filepath.Join(home, "report.grsh")},
		{"~/x.grsh", filepath.Join(home, "x.grsh")},
		{dir, filepath.Join(dir, "report.grsh")},
		{filepath.Join(dir, "named.grsh"), filepath.Join(dir, "named.grsh")},
	}
	for _, tc := range tests {
		got, err := keepDest(tc.arg, "report.grsh")
		if err != nil {
			t.Errorf("keepDest(%q): %v", tc.arg, err)
			continue
		}
		if got != tc.want {
			t.Errorf("keepDest(%q) = %q, want %q", tc.arg, got, tc.want)
		}
	}
}

// TestChapterDirectives: the two chapter-level knobs parse, and the rest
// of the front matter stays the commentary it has always been.
func TestChapterDirectives(t *testing.T) {
	src := `# A Chapter

explain: on
keep: report.grsh

A note to the next editor: this line has a colon in it, and the ratio
2:1 must not become a directive.

## step: one
Body.
---
verify: any-input
solution: echo hi
`
	l, err := parseLesson("02-x", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !l.Explain {
		t.Error("explain: on did not take")
	}
	if l.Keep != "report.grsh" {
		t.Errorf("keep = %q, want report.grsh", l.Keep)
	}
	if len(l.Steps) != 1 {
		t.Errorf("%d steps, want 1 — front-matter prose leaked into the lesson", len(l.Steps))
	}

	// A known key with an unusable value IS an error: silently ignoring
	// `explain: yes` would leave a chapter author wondering why the hint
	// lane stayed quiet.
	for _, bad := range []string{"explain: yes", "explain:", "keep:"} {
		if _, err := parseLesson("02-x", "# T\n\n"+bad+"\n\n## step: one\nB.\n---\nverify: any-input\n"); err == nil {
			t.Errorf("parseLesson accepted %q", bad)
		}
	}
}

// TestContentChapterDirectives pins the wiring between the curriculum
// and the two engine features it asks for. Both are invisible in the
// content self-check — a chapter with `explain:` lost grades exactly the
// same — so without this the directive could be dropped in an edit and
// nothing would notice.
func TestContentChapterDirectives(t *testing.T) {
	var explain, keep []string
	for _, l := range lessons() {
		if l.Explain {
			explain = append(explain, l.ID)
		}
		if l.Keep != "" {
			keep = append(keep, l.ID+":"+l.Keep)
		}
	}
	if len(explain) != 1 || !strings.HasPrefix(explain[0], "02-") {
		t.Errorf("chapters running with --explain = %v, want just the classification chapter", explain)
	}
	if len(keep) != 1 || !strings.HasSuffix(keep[0], ":report.grsh") {
		t.Errorf("chapters with a keepsake file = %v, want just the capstone's report.grsh", keep)
	}
}

// TestCapstoneOffersTheScript walks the real final chapter and checks the
// end of the tutorial as a student meets it: the outro names the script
// they built, the prompt stays open long enough to rescue it, and the
// tutor closes itself once it is out.
//
// The fixture tests above cover the same machinery in isolation. This one
// exists because the wiring runs through three separate things that can
// drift apart independently — the capstone's `keep:` directive, the file
// its `session save` step actually writes, and the engine's stat for that
// name — and nothing else fails when they disagree.
func TestCapstoneOffersTheScript(t *testing.T) {
	box, err := newSandbox()
	if err != nil {
		t.Fatalf("playground: %v", err)
	}
	defer box.cleanup()

	all := lessons()
	idx := len(all) - 1
	if all[idx].Keep == "" {
		t.Skipf("%s names no keepsake file", all[idx].ID)
	}

	var out strings.Builder
	cap := newCapture(64 << 10)
	sess := runner.NewSession(runner.Options{ScriptName: "tutor-capstone", Stdout: cap, Stderr: cap})
	e := newEngine(all[idx], sess, cap, &out, false)
	e.chapters, e.chIdx, e.dir = all, idx, box.dir

	// The chapter runs AS a chapter — one session, solutions in order —
	// because the capstone composes on purpose: it captures counts, prints
	// a report from them, saves the last three units, and sources them
	// back.
	units := repl.NewUnitLog()
	for _, st := range all[idx].Steps {
		e.BeforePrompt(&out)
		e.AfterEval(st.Solution, units.Submit(st.Solution, sess, io.Discard, io.Discard))
	}
	if !e.finished {
		t.Fatalf("the capstone did not finish on its own solutions:\n%s", out.String())
	}
	if !strings.Contains(out.String(), all[idx].Keep) {
		t.Errorf("the outro never offered %s:\n%s", all[idx].Keep, out.String())
	}
	if _, done := e.Done(); done {
		t.Fatal("the session ended while the outro was offering :keep")
	}

	dest := filepath.Join(t.TempDir(), "kept.grsh")
	e.Command(":keep " + dest)
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf(":keep saved nothing: %v (output %q)", err, out.String())
	}
	if !strings.HasPrefix(string(body), "#!/usr/bin/env grsh") {
		t.Errorf("the kept file is not a runnable script:\n%s", body)
	}
	if _, done := e.Done(); !done {
		t.Error("the tutor should close once the script is safely out")
	}
}
