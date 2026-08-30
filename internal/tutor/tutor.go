// Package tutor implements `grsh tutor`: an interactive lesson engine
// that runs *inside* the real REPL rather than simulating one.
//
// The whole design turns on one idea — the student types at the actual
// grsh prompt, with every convenience live (highlighting, ghost text, the
// continuation breadcrumb, the signature hint line), while the lesson
// engine sits around the loop as a repl.Interceptor:
//
//	repl.loop ──BeforePrompt──▶ engine: clear the tee, print the panel
//	          ◀── user types at the real prompt, real Eval runs ──
//	          ──AfterEval─────▶ engine: grade, advance or nudge
//	          ──Done?─────────▶ engine: lesson finished, leave the loop
//
// Because the tutor never forks the loop, the continuation, Ctrl-C and
// EOF semantics it teaches are the ones it is running on.
package tutor

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rohanthewiz/grsh/internal/repl"
	"github.com/rohanthewiz/grsh/internal/runner"
	"golang.org/x/term"
)

// hintAfter is the number of misses that earns an unsolicited hint. Two
// is the point where a stuck student is still trying but no longer
// learning from the silence.
const hintAfter = 2

// engine is the lesson state machine. It implements repl.Interceptor.
type engine struct {
	lesson Lesson
	sess   *runner.Session
	cap    *capture
	// out is the single sink for everything the engine prints — panels,
	// ticks, hints. BeforePrompt is handed the loop's writer too, but the
	// engine ignores it: AfterEval has no writer parameter, and feedback
	// that interleaved across two sinks would reorder unpredictably when
	// one of them is a buffer (as in tests) and the other a terminal.
	out io.Writer

	idx      int  // index of the current step
	attempts int  // failed attempts at the current step
	revealed int  // hints already shown for the current step
	posted   bool // has the current step's panel been printed?

	finished bool // every step passed; Done ends the loop
	st       style
}

func newEngine(l Lesson, sess *runner.Session, cap *capture, out io.Writer, color bool) *engine {
	return &engine{lesson: l, sess: sess, cap: cap, out: out, st: newStyle(color)}
}

// current returns the step being graded, or nil past the end.
func (e *engine) current() *Step {
	if e.idx < 0 || e.idx >= len(e.lesson.Steps) {
		return nil
	}
	return &e.lesson.Steps[e.idx]
}

// BeforePrompt prints the current step's panel (once) and clears the
// output capture so the next attempt is graded on its own output alone.
func (e *engine) BeforePrompt(io.Writer) {
	e.cap.Reset()
	step := e.current()
	if step == nil || e.posted {
		return
	}
	e.posted = true
	e.printPanel(e.out, step)
}

// AfterEval grades one completed input unit and advances, or nudges.
//
// Note that a failed attempt is not an error state: the student's command
// really ran, its output and its exit status are real, and the shell is
// exactly where they left it. That is the point of grading in the live
// session — the only thing a miss costs is the step not ticking over.
func (e *engine) AfterEval(src string, err error) {
	step := e.current()
	if step == nil {
		return
	}
	a := Attempt{Input: src, Output: e.cap.String(), Err: err, Sess: e.sess}
	if !step.Verify.Verify(a) {
		e.miss(e.out, step)
		return
	}
	e.pass(e.out)
}

// Done reports lesson completion. The loop polls it before each prompt,
// so the closing banner is the last thing on screen.
func (e *engine) Done() (int, bool) {
	if !e.finished {
		return 0, false
	}
	return 0, true
}

// pass advances to the next step, resetting the per-step counters.
func (e *engine) pass(w io.Writer) {
	fmt.Fprintf(w, "\n%s  %s\n", e.st.ok("✓"), e.st.dim("nice — that's it."))
	e.idx++
	e.attempts, e.revealed, e.posted = 0, 0, false
	if e.idx >= len(e.lesson.Steps) {
		e.finished = true
		e.printOutro(w)
	}
}

// miss records a failed attempt and escalates: a bare nudge first, then
// hints one at a time, then the solution once the hints run out.
func (e *engine) miss(w io.Writer, step *Step) {
	e.attempts++
	fmt.Fprintf(w, "\n%s  %s\n", e.st.warn("·"), e.st.dim("not quite — try again."))
	if e.attempts < hintAfter {
		return
	}
	switch {
	case e.revealed < len(step.Hints):
		fmt.Fprintf(w, "   %s %s\n", e.st.label("hint:"), step.Hints[e.revealed])
		e.revealed++
	case step.Solution != "":
		fmt.Fprintf(w, "   %s %s\n", e.st.label("answer:"), e.st.code(step.Solution))
	}
}

// printPanel renders the step above the prompt:
//
//	── A three-step tour ─────────────────────────── 1/3 ──
//
//	  This is a real shell. Everything you already know works.
//
//	  try:  echo hello
func (e *engine) printPanel(w io.Writer, step *Step) {
	width := termWidth()
	counter := fmt.Sprintf("%d/%d", e.idx+1, len(e.lesson.Steps))
	fmt.Fprintf(w, "\n%s\n\n", e.st.rule(width, e.lesson.Title, counter))
	for _, line := range step.Prose {
		if line == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "  %s\n", line)
	}
	if step.Try != "" {
		fmt.Fprintf(w, "\n  %s %s\n", e.st.label("try:"), e.st.code(step.Try))
	}
	fmt.Fprintln(w)
}

func (e *engine) printOutro(w io.Writer) {
	fmt.Fprintf(w, "\n%s %s\n", e.st.ok("★"), e.st.bold("Lesson complete."))
	fmt.Fprintf(w, "  %s\n\n", e.st.dim("Shell and Go, one prompt, one language for scripts and sessions."))
}

// Run is the `grsh tutor [chapter]` entry point; it returns a process
// exit code.
//
// Two deliberate choices about the session it builds:
//
//   - Its stdout/stderr are teed. The student sees output exactly as
//     usual; the engine keeps the copy it grades against. This is why the
//     tutor owns session construction instead of taking one from main.
//   - ~/.grshrc is skipped and unit history stays in memory. A lesson
//     must be reproducible, and someone else's alias for `ls` breaking
//     step 1 would be a bad first impression of the language.
func Run(version string, args []string, out, errOut io.Writer) int {
	// Argument validation first: a typo in the chapter number deserves to
	// be reported as such even when the tutor could not have run anyway.
	all := lessons()
	idx, err := chapterIndex(args, len(all))
	if err != nil {
		fmt.Fprintf(errOut, "grsh tutor: %v\n", err)
		return 2
	}
	// The tutor is inherently interactive: it prints a panel, waits for the
	// student, and grades. Without a terminal the line editor has nothing
	// to drive, so fail loudly rather than hanging on a pipe.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(errOut, "grsh tutor: needs an interactive terminal")
		return 2
	}

	cap := newCapture(64 << 10)
	sess := runner.NewSession(runner.Options{
		ScriptName: "grsh",
		Stdout:     io.MultiWriter(os.Stdout, cap),
		Stderr:     io.MultiWriter(os.Stderr, cap),
	})

	e := newEngine(all[idx], sess, cap, out, colorEnabled())
	printIntro(out, e.st, version)

	return repl.RunOptions(sess, repl.Options{
		Version:     version,
		NoRC:        true,
		Quiet:       true,
		Ephemeral:   true,
		Interceptor: e,
	})
}

// chapterIndex resolves the optional chapter argument (1-based, as the
// user counts chapters) to a slice index.
func chapterIndex(args []string, n int) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	ch, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, fmt.Errorf("chapter must be a number, got %q", args[0])
	}
	if ch < 1 || ch > n {
		return 0, fmt.Errorf("no chapter %d (have 1..%d)", ch, n)
	}
	return ch - 1, nil
}

func printIntro(w io.Writer, st style, version string) {
	fmt.Fprintf(w, "\n%s %s\n", st.bold("grsh tutor"), st.dim("· "+version))
	fmt.Fprintf(w, "%s\n", st.dim("You are at a real grsh prompt. Answer each step by running it."))
	fmt.Fprintf(w, "%s\n", st.dim("Ctrl+D leaves the tutor at any time."))
}

// termWidth is the panel rule width, clamped so the lesson reads the same
// in a narrow split as in a maximized window.
func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 64
	}
	return min(max(w, 40), 76)
}

// colorEnabled follows the same informal standard as the prompt's own
// check: NO_COLOR wins, dumb terminals and non-terminals stay plain.
func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// style renders the panel's few roles. Keeping them behind named methods
// (rather than sprinkling escape codes through the engine) is what makes
// the NO_COLOR path a single boolean instead of a second code path.
type style struct{ on bool }

func newStyle(on bool) style { return style{on: on} }

func (s style) wrap(code, txt string) string {
	if !s.on {
		return txt
	}
	return "\x1b[" + code + "m" + txt + "\x1b[0m"
}

func (s style) bold(t string) string  { return s.wrap("1", t) }
func (s style) dim(t string) string   { return s.wrap("2", t) }
func (s style) ok(t string) string    { return s.wrap("32", t) }
func (s style) warn(t string) string  { return s.wrap("33", t) }
func (s style) label(t string) string { return s.wrap("36", t) }
func (s style) code(t string) string  { return s.wrap("1;36", t) }

// rule draws "── Title ────────── 1/3 ──", padding the middle so the
// counter lands at the right edge. Padding is computed on the plain text,
// then the pieces are colored — measuring after coloring would count the
// escape bytes as visible columns.
func (s style) rule(width int, title, counter string) string {
	// Width is measured in runes, not bytes: a title with a box-drawing
	// or accented character would otherwise over-count and short the fill.
	plain := utf8.RuneCountInString("──  "+title) + utf8.RuneCountInString("  "+counter+" ──")
	fill := max(width-plain, 1)
	return s.dim("── ") + s.bold(title) + s.dim(" "+strings.Repeat("─", fill)+" "+counter+" ──")
}
