package tutor

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/repl"
	"github.com/rohanthewiz/grsh/internal/runner"
)

var update = flag.Bool("update", false, "rewrite the golden transcript from this run")

// TestChapterOneTranscript is the golden transcript: the exact bytes a
// student sees walking chapter 1 — intro, panels, a miss, the earned
// hint, each tick, and the outro.
//
// It guards what no other test can. The content self-check proves every
// solution still passes; the pty tests prove the loop is really driven.
// Neither notices if a panel loses its counter, a rule stops padding, an
// outro forgets to name the next chapter, or a chapter's prose picks up
// a stray blank line. Those are exactly the regressions that reach a
// student unannounced, because nothing about them fails.
//
// Only the tutor's own writer is captured — the session's stdout goes to
// the grading tee alone — so the transcript is the engine's chrome and
// the chapter's words, not the local `ls` implementation's column
// widths.
//
// Regenerate deliberately, and read the diff:
//
//	go test ./internal/tutor -run TestChapterOneTranscript -update
func TestChapterOneTranscript(t *testing.T) {
	got := renderChapterOne(t)
	path := filepath.Join("testdata", "chapter01.golden")

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s (%d bytes)", path, len(got))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — run with -update to create it", err)
	}
	if got != string(want) {
		t.Errorf("chapter 1's transcript changed.\n%s\nRe-run with -update once the diff above is what you meant.",
			firstDiff(string(want), got))
	}
}

// renderChapterOne walks chapter 1 the way a student would — including a
// miss that earns a hint — and returns everything the tutor printed.
func renderChapterOne(t *testing.T) string {
	t.Helper()
	box, err := newSandbox()
	if err != nil {
		t.Fatalf("playground: %v", err)
	}
	defer box.cleanup()

	var out strings.Builder
	all := lessons()
	cap := newCapture(64 << 10)
	sess := runner.NewSession(runner.Options{ScriptName: "tutor-golden", Stdout: cap, Stderr: cap})

	e := newEngine(all[0], sess, cap, &out, false)
	e.chapters, e.chIdx, e.dir = all, 0, box.dir
	// A fixed width, so the rule's padding is a property of the renderer
	// rather than of the terminal the test happened to run in.
	e.width = 64

	tu := &tutor{version: "0.0.0-test", all: all, store: nopStore{}, out: &out, errOut: io.Discard}
	// A stand-in for the playground path: the real one is a fresh mkdtemp
	// on every run, and a golden file cannot hold a different string each
	// time it is written.
	tu.printIntro(e, "/tmp/grsh-tutor-PLAYGROUND", true, false)

	units := repl.NewUnitLog()
	submit := func(src string) {
		e.BeforePrompt(&out)
		fmt.Fprintf(&out, "grsh> %s\n", src) // the student's keystrokes, for readability
		e.AfterEval(src, units.Submit(src, sess, io.Discard, io.Discard))
	}

	// Two misses first: the nudge alone, then the nudge plus the hint the
	// second miss earns. `pwd` really runs — a miss is not an error state
	// — it simply is not the exercise.
	submit("pwd")
	submit("pwd")
	for _, st := range all[0].Steps {
		submit(st.Solution)
	}
	return out.String()
}

// firstDiff reports the first differing line with a little context, so a
// failure names the change instead of dumping two transcripts.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range max(len(w), len(g)) {
		lw, lg := lineAt(w, i), lineAt(g, i)
		if lw == lg {
			continue
		}
		return fmt.Sprintf("line %d:\n  want: %q\n   got: %q\n  (context: %q)", i+1, lw, lg, lineAt(w, i-1))
	}
	return "(no line differs — trailing bytes?)"
}

func lineAt(ls []string, i int) string {
	if i < 0 || i >= len(ls) {
		return ""
	}
	return ls[i]
}
