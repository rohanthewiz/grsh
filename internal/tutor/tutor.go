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
//
// One engine runs one chapter. Moving between chapters is a teardown and
// a rebuild — new playground, new session, new engine — driven by the
// chapter loop in Run, because a chapter is a clean slate and pretending
// otherwise would leave a student in chapter 5 surrounded by chapter 2's
// variables and files.
package tutor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	skipped  int  // steps left behind with :skip, reported by :progress

	finished bool // every step passed; the outro has been printed
	quit     bool // :quit — Done ends the loop early, at the same code
	st       style

	// jump is the chapter index `:next` / `:menu N` asked for, or -1.
	// A chapter switch cannot happen in place — it needs a fresh
	// playground and a fresh session — so the engine only records the
	// request, ends the loop through Done, and lets tutor.chapter rebuild
	// both around a new engine.
	jump int
	// keepDone records that the chapter's keepsake file has been copied
	// out. It is what lets a finished last chapter hold the prompt open
	// for one `:keep` and then close by itself.
	keepDone bool
	// width pins the panel rule; 0 asks the terminal. Fixing it is what
	// makes a golden transcript a test of the renderer rather than of
	// the window the test happened to run in.
	width int

	// Host wiring, set by tutor.chapter after construction so the
	// engine's constructor stays the small thing the unit tests build.
	// Each is optional: a nil store or an empty dir degrades a feature,
	// never the lesson.
	chapters []Lesson // the whole curriculum, for :menu / :next
	chIdx    int      // this lesson's index in chapters ("chapter 3 of 8")
	dir      string   // sandbox root, handed to `file` verifiers
	store    Store    // progress persistence; nil in tests
}

func newEngine(l Lesson, sess *runner.Session, cap *capture, out io.Writer, color bool) *engine {
	// jump starts at -1, not 0: zero is chapter 1, a perfectly good jump
	// target, so "no jump requested" needs a value outside the range.
	return &engine{lesson: l, sess: sess, cap: cap, out: out, st: newStyle(color), jump: -1}
}

// hasNext reports whether a chapter follows this one. With no curriculum
// wired in (the unit tests build a lone Lesson) there is nothing to go
// on to, which is what makes a single-lesson engine end by itself.
func (e *engine) hasNext() bool { return e.chIdx+1 < len(e.chapters) }

// keepPending reports whether the chapter's keepsake file exists and has
// not been saved out yet — the one reason a finished FINAL chapter holds
// the prompt open instead of exiting. Statting per prompt is fine: the
// file only appears once the student runs the step that writes it.
func (e *engine) keepPending() bool {
	if e.lesson.Keep == "" || e.dir == "" || e.keepDone {
		return false
	}
	_, err := os.Stat(filepath.Join(e.dir, e.lesson.Keep))
	return err == nil
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
	a := Attempt{Input: src, Output: e.cap.String(), Err: err, Sess: e.sess, Dir: e.dir}
	if !step.Verify.Verify(a) {
		e.miss(e.out, step)
		e.save()
		return
	}
	e.pass(e.out)
}

// Done tells the loop when to stop. It is polled before each prompt, so
// the closing banner is the last thing on screen — and so that a chapter
// change (which the engine cannot perform itself) happens between two
// prompts rather than in the middle of one.
func (e *engine) Done() (int, bool) {
	switch {
	case e.quit, e.jump >= 0:
		// :quit leaves; :next / :menu N leave so tutor.chapter can rebuild
		// the playground and session around the next chapter.
	case e.finished && !e.hasNext() && !e.keepPending():
		// The chapter is over and there is genuinely nothing further to
		// offer, so the tutor closes itself — no Ctrl+D required.
		//
		// When there IS something (a next chapter to start with :next, a
		// capstone script to rescue with :keep) the loop stays alive at the
		// prompt instead: the outro just named an action, and exiting out
		// from under it would make the offer a lie.
	default:
		return 0, false
	}
	// Exit code 0 either way. A finished lesson is obviously a success,
	// and so is a deliberate :quit — walking out of a tutorial is a
	// choice, not a failure, and a nonzero code would break the entirely
	// reasonable `grsh tutor && next-thing` while teaching nobody
	// anything. The signature keeps the code in case a future mode (a
	// graded exam, say) has an actual verdict to report.
	return 0, true
}

// pass ticks the step over and moves on.
func (e *engine) pass(w io.Writer) {
	fmt.Fprintf(w, "\n%s  %s\n", e.st.ok("✓"), e.st.dim("nice — that's it."))
	e.advance(w)
}

// advance is the only place the lesson's position moves — a pass, a
// :skip, or falling off the end. Keeping it single means the per-step
// counters are reset in exactly one place, and so is the progress write.
func (e *engine) advance(w io.Writer) {
	e.idx++
	e.attempts, e.revealed, e.posted = 0, 0, false
	if e.idx >= len(e.lesson.Steps) {
		e.finished = true
		e.printOutro(w)
	}
	e.save()
}

// save records the student's place. Failures are swallowed on purpose:
// progress is a convenience, and a full disk or a second tutor holding
// the database must not interrupt a lesson to say so.
func (e *engine) save() {
	if e.store == nil {
		return
	}
	// A finished lesson saves an empty step, which resumeAt reads as
	// "start over" — running `grsh tutor` again after the outro should
	// begin the chapter, not drop the student straight back into the
	// completion banner.
	step := ""
	if cur := e.current(); cur != nil {
		step = cur.ID
	}
	_ = e.store.Save(Record{
		Lesson:   e.lesson.ID,
		Step:     step,
		Attempts: e.attempts,
		Revealed: e.revealed,
	})
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
	width := e.width
	if width <= 0 {
		width = termWidth()
	}
	counter := fmt.Sprintf("%d/%d", e.idx+1, len(e.lesson.Steps))
	fmt.Fprintf(w, "\n%s\n\n", e.st.rule(width, e.lesson.Title, counter))
	for _, line := range step.Prose {
		if line == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "  %s\n", e.st.inline(line))
	}
	if step.Try != "" {
		// A `try:` may be a whole block (chapter 3 hands the student a
		// three-line `for`), so the label goes on the first line and the
		// rest is aligned under it — the shape the student has to type.
		for i, line := range strings.Split(step.Try, "\n") {
			if i == 0 {
				fmt.Fprintf(w, "\n  %s %s\n", e.st.label("try:"), e.st.code(line))
				continue
			}
			fmt.Fprintf(w, "       %s\n", e.st.code(line))
		}
	}
	fmt.Fprintln(w)
}

// printOutro closes a chapter, and its job is to name what to do next —
// the tutor holds the prompt open for exactly the actions it prints here
// (see Done), so an offer made in this function is one the student can
// actually take.
func (e *engine) printOutro(w io.Writer) {
	if e.hasNext() {
		fmt.Fprintf(w, "\n%s %s\n", e.st.ok("★"),
			e.st.bold(fmt.Sprintf("Chapter %d of %d complete.", e.chIdx+1, len(e.chapters))))
		e.offerKeep(w)
		fmt.Fprintf(w, "  %s %s\n", e.st.dim("next —"),
			e.st.bold(fmt.Sprintf("%d. %s", e.chIdx+2, e.chapters[e.chIdx+1].Title)))
		fmt.Fprintf(w, "  %s\n", e.st.dim("`:next` starts it in a fresh playground."))
		fmt.Fprintf(w, "  %s\n\n", e.st.dim("Ctrl+D stops here; your place is saved."))
		return
	}
	fmt.Fprintf(w, "\n%s %s\n", e.st.ok("★"), e.st.bold("Lesson complete."))
	e.offerKeep(w)
	fmt.Fprintf(w, "  %s\n\n", e.st.dim("Shell and Go, one prompt, one language for scripts and sessions."))
}

// offerKeep points at the file this chapter made that is worth more than
// the playground it lives in.
//
// It is an offer, not an action: the playground is deleted on exit, so
// the tutor could quietly copy the script into the student's home and
// call it a service — but a tutorial that leaves files in ~ without
// asking is a surprise, and the ask costs one line.
func (e *engine) offerKeep(w io.Writer) {
	if !e.keepPending() {
		return
	}
	fmt.Fprintf(w, "  %s %s %s\n", e.st.dim("you made"), e.st.code(e.lesson.Keep),
		e.st.dim("— it goes away with the playground."))
	fmt.Fprintf(w, "  %s\n", e.st.dim("`:keep` saves it to your home directory (`:keep <path>` chooses where)."))
}

// tutor is one `grsh tutor` invocation: the whole curriculum, the
// progress store that outlives any single chapter, and the writers.
//
// Chapters come and go inside it — each one gets a fresh playground and
// a fresh runner.Session — which is why the chapter loop lives here and
// not in the engine. A chapter switch is a teardown and a rebuild, and
// pretending otherwise (swapping the lesson under a live session) would
// leave the student in chapter 5 with chapter 4's variables, files and
// working directory still around them.
type tutor struct {
	version string
	all     []Lesson
	store   Store
	out     io.Writer
	errOut  io.Writer
	color   bool
}

// Run is the `grsh tutor [chapter]` entry point; it returns a process
// exit code.
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

	t := &tutor{version: version, all: all, out: out, errOut: errOut, color: colorEnabled()}
	// One store for the whole run: the chapters share a single bytdb file,
	// and reopening it per chapter would take and drop the lock eight
	// times for no gain.
	t.store = openStore(progressPath())

	// With no argument the store picks the chapter (`grsh tutor` means
	// "carry on"); with one, the student did.
	resumed := false
	if len(args) == 0 {
		idx, resumed = resumeChapter(all, t.store.Load)
	}

	code := 0
	for first := true; ; first = false {
		next := -1
		code, next = t.chapter(idx, first, resumed)
		if next < 0 {
			break
		}
		// Only the first chapter of a run can be a resume; a chapter
		// reached with :next starts at its top by construction.
		idx, resumed = next, false
	}
	t.store.Close()
	return code
}

// chapter runs one chapter to completion, abandonment, or a jump, and
// returns the exit code plus the chapter index to run next (-1 to stop).
//
// Two deliberate choices about the session it builds:
//
//   - Its stdout/stderr are teed. The student sees output exactly as
//     usual; the engine keeps the copy it grades against. This is why the
//     tutor owns session construction instead of taking one from main.
//   - ~/.grshrc is skipped and unit history stays in memory. A lesson
//     must be reproducible, and someone else's alias for `ls` breaking
//     step 1 would be a bad first impression of the language.
func (t *tutor) chapter(idx int, first, resumed bool) (code, next int) {
	l := t.all[idx]

	// The playground comes first: it chdir's the process, and every
	// later decision (what `ls` shows, where a `file` verifier looks) is
	// relative to it.
	box, err := newSandbox()
	if err != nil {
		fmt.Fprintf(t.errOut, "grsh tutor: could not build the playground: %v\n", err)
		return 2, -1
	}

	cap := newCapture(64 << 10)
	opts := runner.Options{
		ScriptName: "grsh",
		Stdout:     io.MultiWriter(os.Stdout, cap),
		Stderr:     io.MultiWriter(os.Stderr, cap),
	}
	if l.Explain {
		// io.Discard, not a real writer: --explain has two halves, a line
		// per classified chunk on the writer and the live verdict in the
		// prompt's hint lane, and only the second one belongs in a lesson.
		// The REPL reads Session.Explaining() (which is "the writer is
		// non-nil") for the hint lane, so discarding the stream turns on
		// exactly the half the chapter is teaching without printing a
		// classification dump over every panel.
		opts.Explain = io.Discard
	}
	sess := runner.NewSession(opts)

	e := newEngine(l, sess, cap, t.out, t.color)
	e.chapters, e.chIdx, e.dir, e.store = t.all, idx, box.dir, t.store

	rec, found := t.store.Load(l.ID)
	e.idx = resumeAt(l, rec, found)
	if e.idx > 0 {
		// Hint state resumes too: a student who quit stuck on step 4 comes
		// back to the hint they had already earned, not to silence.
		e.attempts, e.revealed = rec.Attempts, rec.Revealed
	}
	t.printIntro(e, box.dir, first, resumed)

	code = repl.RunOptions(sess, repl.Options{
		Version:     t.version,
		NoRC:        true,
		Quiet:       true,
		Ephemeral:   true,
		Interceptor: e,
	})
	// Teardown is explicit rather than deferred, so a panic leaves the
	// playground on disk: the fixtures plus whatever the student's last
	// command wrote are the whole reproduction for a crash report.
	box.cleanup()
	return code, e.jump
}

// resumeChapter picks the chapter that bare `grsh tutor` opens.
//
// It reads the records the way they were designed to be read: the
// FURTHEST chapter the student has touched, not the first one they have
// not finished. Those differ for anyone who jumped — a student who ran
// `grsh tutor 6` and stopped halfway wants chapter 6 back, not chapter 1
// because they never bothered with the shell basics.
//
// The scan runs backwards and stops at the first record it finds, so it
// costs one Get on a fresh install and a handful on a well-used one.
//
// load is passed in rather than reached for through a Store so the
// resume policy can be tested as the pure function it is.
func resumeChapter(all []Lesson, load func(string) (Record, bool)) (idx int, resumed bool) {
	for i := len(all) - 1; i >= 0; i-- {
		rec, ok := load(all[i].ID)
		if !ok {
			continue
		}
		if rec.Step != "" {
			return i, true // stopped mid-chapter: pick it back up
		}
		// An empty step means that chapter was finished (see save), so the
		// place to carry on is the next one...
		if i+1 < len(all) {
			return i + 1, true
		}
		// ...unless it was the last, in which case the student graduated
		// and a fresh run starts the curriculum over rather than dropping
		// them back onto the completion banner.
		return 0, false
	}
	return 0, false
}

// resumeAt maps a saved record onto a step index in the lesson as it
// exists NOW.
//
// The record stores a step *ID*, not an index, precisely so that editing
// the curriculum cannot teleport a returning student into the middle of a
// step they have never seen. An ID that no longer exists — a renamed or
// deleted step — and an empty ID — the lesson was finished — both mean
// the same safe thing: start at the top.
func resumeAt(l Lesson, r Record, found bool) int {
	if !found || r.Step == "" {
		return 0
	}
	for i, st := range l.Steps {
		if st.ID == r.Step {
			return i
		}
	}
	return 0
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

// printIntro sets the scene: a real prompt, a real (throwaway)
// directory, and the one meta-command a student needs to find all the
// others.
//
// Only the first chapter of a run gets the full header. A chapter
// reached with `:next` gets one line — the new playground path — because
// that is the only thing that changed, and re-reading the same six lines
// seven more times would train the student to skip them.
func (t *tutor) printIntro(e *engine, dir string, first, resumed bool) {
	st := e.st
	if !first {
		fmt.Fprintf(t.out, "%s\n", st.dim("Fresh playground: "+dir+" — the last chapter's files are gone."))
		return
	}
	fmt.Fprintf(t.out, "\n%s %s\n", st.bold("grsh tutor"), st.dim("· "+t.version))
	fmt.Fprintf(t.out, "%s\n", st.dim("You are at a real grsh prompt. Answer each step by running it."))
	fmt.Fprintf(t.out, "%s\n", st.dim("Playground: "+dir+" (deleted on exit — experiment freely)"))
	fmt.Fprintf(t.out, "%s\n", st.dim("Type :help for tutor commands; Ctrl+D or :quit leaves at any time."))
	if resumed || e.idx > 0 {
		// The chapter matters as much as the step: a returning student
		// dropped into chapter 6 with no explanation would reasonably
		// think the tutor had lost the first five.
		fmt.Fprintf(t.out, "%s\n", st.dim(fmt.Sprintf("Resuming at chapter %d (%s), step %d — `:menu` lists them all.",
			e.chIdx+1, e.lesson.Title, e.idx+1)))
	}
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

// inline renders a prose line's markdown emphasis: `backticked spans` in
// the code colour, **starred spans** in bold.
//
// Lesson prose is markdown-shaped because chapters are written as
// markdown, and these two marks are the only inline markup a chapter is
// allowed to use. The two are treated differently with color off, which
// is the whole reason this is a function rather than a regexp:
//
//   - backticks STAY. They are the only emphasis a plain terminal has,
//     and a NO_COLOR reader who lost them would be reading
//     `ls *.go | wc -l` as ordinary prose.
//   - stars GO. Emphasis has no plain-text convention worth preserving;
//     literal asterisks would just be noise around the word they meant to
//     lift.
//
// Spans do not nest, and an unclosed mark is left exactly as written
// rather than swallowing the rest of the line.
func (s style) inline(line string) string {
	var b strings.Builder
	for {
		tick, star := strings.Index(line, "`"), strings.Index(line, "**")
		switch {
		case tick < 0 && star < 0:
			b.WriteString(line)
			return b.String()

		case star < 0 || (tick >= 0 && tick < star):
			body, rest, ok := strings.Cut(line[tick+1:], "`")
			if !ok {
				b.WriteString(line)
				return b.String()
			}
			b.WriteString(line[:tick])
			if s.on {
				b.WriteString(s.code(body))
			} else {
				b.WriteString("`" + body + "`")
			}
			line = rest

		default:
			body, rest, ok := strings.Cut(line[star+2:], "**")
			if !ok {
				b.WriteString(line)
				return b.String()
			}
			b.WriteString(line[:star])
			b.WriteString(s.bold(body)) // identity with color off
			line = rest
		}
	}
}

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
