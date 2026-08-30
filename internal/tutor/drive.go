package tutor

// Driving the curriculum without a terminal.
//
// The engine is a repl.Interceptor: four hooks that repl.loop calls at
// four points in an input unit's life. A REPL is not the only thing that
// can call them, and Driver is the proof — it reproduces exactly those
// four call sites, in exactly that order, with a line of input arriving
// from wherever the host gets its lines (an HTTP POST, in the web tour's
// case) instead of from a line editor:
//
//	repl.loop                          Driver
//	───────────────────────────────    ────────────────────────────────
//	drain job notifications            advance()
//	ic.Done() → leave the loop         advance()  → jump / finish
//	ic.BeforePrompt(w)                 advance()
//	rd.Readline()                      Submit(line)
//	sess.Pending → keep reading        Submit: accumulate, report Pending
//	ic.Command(src) → next unit        Submit
//	replCommand / hist / sess.Eval     Submit, via repl.UnitLog
//	ic.AfterEval(src, err)             Submit
//
// What it deliberately does NOT reproduce is the line editor: there is no
// highlighting, no ghost text and no hint lane here, because those are
// terminal rendering and a host that isn't a terminal has to draw its own.
// View exists for that — it is the engine's state as data, so a host can
// render the lesson in its own medium instead of scraping the transcript.
//
// One chapter at a time, same as the terminal tutor: openChapter is a
// teardown and a rebuild, never a lesson swapped under a live session.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"

	"github.com/rohanthewiz/grsh/internal/classify"
	"github.com/rohanthewiz/grsh/internal/repl"
	"github.com/rohanthewiz/grsh/internal/shellexec"
)

// evalGate serializes evaluation across every Driver in the process, and
// is the reason more than one of them can exist at once.
//
// A grsh session's working directory IS the process's working directory —
// `cd` calls os.Chdir, and that is deliberate (see internal/shellexec).
// Two drivers with two playgrounds therefore cannot both be "in" their own
// directory at the same time, so instead each one records where it was and
// re-enters it under this lock for the duration of an evaluation. Between
// evaluations the process cwd belongs to whoever ran last, which nothing
// reads: every path a lesson touches is resolved during an Eval.
//
// The cost is real and worth stating: one student's `sleep 30` blocks
// every other student's next command for thirty seconds. That is
// acceptable for what this is — a local tool serving a handful of tabs on
// one machine — and it is not fixable at this layer. Isolating students
// properly means a process each.
//
// The environment is NOT restored the same way. `export` also mutates
// process-global state, so a variable one student exports is visible to
// another; no chapter's steps export anything, and snapshotting the whole
// environment per evaluation would cost more than the leak does.
var evalGate sync.Mutex

// Driver runs the curriculum for one student outside the REPL loop.
//
// It owns the same three things the terminal tutor owns per chapter — a
// playground, a session and an engine — plus the continuation buffer that
// repl.loop keeps on its stack. Not safe for concurrent use: a host must
// serialize its calls, except for Interrupt, which is meant to be called
// while Submit is in flight and says so.
type Driver struct {
	all   []Lesson
	opts  DriverOptions
	out   io.Writer // the transcript: session output and engine chrome, interleaved
	store Store

	ch  *chapterRun
	log *repl.UnitLog

	// cwd is this driver's place in the filesystem, held across the gaps
	// when another driver owns the process cwd. See evalGate.
	cwd string

	// buf and pend are repl.loop's continuation state, which lives on its
	// stack there and has to live somewhere here: a host hands us one
	// physical line at a time and an input unit may span several.
	buf  []string
	pend classify.PendingInfo

	// done marks chapters the student has actually finished, which is not
	// derivable from where they are: jumping to chapter 6 says nothing
	// about chapters 1 to 5, and a table of contents that inferred it
	// would tick off work nobody did.
	done []bool

	ended bool // the curriculum is over: finished, :quit, or `exit`
	code  int  // the exit code the session ended with
}

// DriverOptions configures a headless run. The zero value is valid: no
// colour, no persisted progress, a 76-column panel.
type DriverOptions struct {
	// Color emits ANSI escapes in the engine's output. A host that renders
	// the transcript as text on a terminal-like surface wants them; one
	// that renders to plain HTML does not.
	Color bool
	// Width pins the panel rule. 0 means "ask the terminal", which is
	// wrong for a host that has no terminal, so a headless host should
	// always set it.
	Width int
	// Panels, when set, takes the step panel instead of the transcript —
	// for a host that renders the step itself from View.
	Panels io.Writer
	// Store persists the student's place. Nil disables resume.
	Store Store
	// Embedded runs the session in host-embedded mode: foreground
	// pipelines get their own process group so Interrupt can reach them,
	// and the session never reaches for a controlling terminal it does not
	// have. Any host that is not a terminal wants this on.
	Embedded bool
}

// NewDriver starts a student at the given chapter with the transcript
// going to out. It opens the first chapter, which builds a playground on
// disk — call Close to remove it.
func NewDriver(out io.Writer, chapter int, o DriverOptions) (*Driver, error) {
	all := lessons()
	if chapter < 0 || chapter >= len(all) {
		return nil, fmt.Errorf("no chapter %d (have 1..%d)", chapter+1, len(all))
	}
	d := &Driver{all: all, opts: o, out: out, store: o.Store, log: repl.NewUnitLog(), done: make([]bool, len(all))}
	if o.Store != nil {
		// A saved record with no step is a finished chapter (see the
		// engine's save), so a returning student's ticks survive the
		// restart that earned them.
		for i, l := range all {
			if rec, ok := o.Store.Load(l.ID); ok && rec.Step == "" {
				d.done[i] = true
			}
		}
	}
	if err := d.openChapter(chapter); err != nil {
		return nil, err
	}
	// The first panel is posted before any input arrives, exactly as the
	// REPL posts it before the first Readline.
	d.advance()
	return d, nil
}

// ResumeChapter is the chapter bare `grsh tutor` would open for this
// student — the furthest one they have touched. A host with a store
// should start there; one without gets 0 and a fresh start.
func ResumeChapter(store Store) int {
	if store == nil {
		return 0
	}
	idx, _ := resumeChapter(lessons(), store.Load)
	return idx
}

// Submit feeds one physical line of input.
//
// A line is not a unit: an unfinished `for` block leaves the driver
// pending (View reports it, so a host can show a continuation prompt) and
// the unit is not graded until it closes. Everything a completed unit does
// — meta-command, prompt affordance, or evaluation — happens before this
// returns, so a host can render View immediately afterwards.
func (d *Driver) Submit(line string) {
	if d.ended {
		return
	}
	d.buf = append(d.buf, line)
	src := strings.Join(d.buf, "\n")
	if strings.TrimSpace(src) == "" {
		d.buf, d.pend = d.buf[:0], classify.PendingInfo{}
		return
	}
	// One speculative classification decides whether to keep reading, the
	// same call and for the same reason as in the loop.
	if d.pend = d.ch.sess.Pending(src); d.pend.NeedsMore {
		return
	}
	d.buf, d.pend = d.buf[:0], classify.PendingInfo{}

	// The unit is echoed into the transcript because nothing else will: a
	// terminal echoes what the student types as a side effect of them
	// typing it, and a host reading lines off a socket has no such echo.
	// It goes through the same writer as the output it precedes, so the
	// two can never be reordered.
	d.echo(src)

	if d.ch.e.Command(src) {
		d.advance()
		return
	}

	var err error
	d.locked(func() {
		// Prompt-only conveniences (`?name`, `session save`) go first and
		// never reach Eval, then history, then evaluation — repl.UnitLog is
		// that dispatch, so the capstone's `session save` takes the path a
		// student's keystrokes take rather than a second implementation of
		// it that could drift.
		err = d.log.Submit(src, d.ch.sess, d.out, d.out)
	})
	if err != nil {
		if xe, ok := errors.AsType[shellexec.ExitErr](err); ok {
			// `exit` at the prompt, or errexit tripping. The student ended
			// the session themselves; there is nothing left to grade.
			d.finish(xe.Code)
			return
		}
		fmt.Fprintf(d.out, "grsh: %s\n", repl.UserMessage(src, err))
	}
	// Graded after the error is reported, so the tutor's feedback is the
	// last thing in the transcript before the next panel.
	d.ch.e.AfterEval(src, err)
	d.advance()
}

// Interrupt sends SIGINT to the student's running foreground pipeline —
// the browser's stop button.
//
// It deliberately takes neither the driver's own serialization nor the
// eval gate: the one moment it matters is while Submit holds both, and a
// stop button that waits for the command it is stopping is not a stop
// button. SignalForeground is documented safe from any goroutine.
func (d *Driver) Interrupt() bool {
	if d.ch == nil {
		return false
	}
	return d.ch.sess.SignalForeground(syscall.SIGINT)
}

// Kill is Interrupt's escalation, for a pipeline that ignored SIGINT.
func (d *Driver) Kill() bool {
	if d.ch == nil {
		return false
	}
	return d.ch.sess.SignalForeground(syscall.SIGKILL)
}

// Close tears the driver down and removes its playground. Idempotent, so
// a host can call it from both an explicit "reset" and a reaper without
// coordinating the two.
func (d *Driver) Close() {
	if d.ch == nil {
		return
	}
	d.locked(func() { d.ch.box.cleanup() })
	d.ch, d.ended = nil, true
}

// Classify is the classifier's verdict on a draft line — "shell ·
// rule=default" — which is what `--explain` shows in the prompt's hint
// lane as the student types.
//
// Chapter 2 grades the classification rules one at a time, and reading the
// verdict for the line you are typing is the whole lesson. A host with no
// prompt to decorate has to draw that lane itself, so the engine hands it
// the text rather than the rule machinery.
//
// Empty when the chapter did not ask for it (`explain: on` in the front
// matter), so a host can call it unconditionally.
func (d *Driver) Classify(src string) string {
	if d.ch == nil || !d.ch.sess.Explaining() || strings.TrimSpace(src) == "" {
		return ""
	}
	chunks := d.ch.sess.Preview(src)
	if len(chunks) == 0 {
		return ""
	}
	// The verdict for the LAST chunk: it is the one the cursor is in, and
	// the one still being typed.
	ch := chunks[len(chunks)-1]
	if ch.Kind == classify.Blank {
		return ""
	}
	return ch.Kind.String() + " · rule=" + ch.Rule
}

// Dir is the playground the student is working in — a real directory on
// the host's disk, which is worth showing, since a tour of a shell that
// would not say where it was standing would be teaching the wrong lesson.
func (d *Driver) Dir() string {
	if d.ch == nil {
		return ""
	}
	return d.ch.box.dir
}

// advance drives the driver to its next prompt: it drains job
// notifications, honours a chapter jump, and posts the panel. It is the
// top of a repl.loop iteration with the Readline taken out.
//
// The loop is for the jump case only — opening a chapter puts a fresh
// engine in place, and a fresh engine has nothing to report — so it runs
// at most twice.
func (d *Driver) advance() {
	for {
		for _, note := range d.ch.sess.Notifications() {
			fmt.Fprintln(d.out, note)
		}
		code, done := d.ch.e.Done()
		if !done {
			d.ch.e.BeforePrompt(d.out)
			return
		}
		if next := d.ch.e.jump; next >= 0 {
			if err := d.openChapter(next); err != nil {
				fmt.Fprintf(d.out, "grsh tutor: could not build the playground: %v\n", err)
				d.finish(2)
				return
			}
			continue
		}
		d.finish(code)
		return
	}
}

// openChapter tears down the current chapter, if any, and builds the next
// one. A chapter switch cannot happen in place: chapter 5 has its own
// fixtures, and a student arriving there holding chapter 2's variables,
// files and working directory would be debugging the tutor rather than
// learning the language.
func (d *Driver) openChapter(idx int) error {
	var ch *chapterRun
	var err error
	d.locked(func() {
		if d.ch != nil {
			if d.ch.e.finished {
				d.done[d.ch.e.chIdx] = true
			}
			d.ch.box.cleanup()
		}
		ch, err = newChapter(d.all, idx, chapterOpts{
			// Both streams land in the transcript, in the order they were
			// written. A host with one surface has nowhere else to put
			// stderr, and interleaving is what a terminal does anyway.
			Stdout:   d.out,
			Stderr:   d.out,
			Chrome:   d.out,
			Panels:   d.opts.Panels,
			Color:    d.opts.Color,
			Width:    d.opts.Width,
			Embedded: d.opts.Embedded,
			Store:    d.store,
		})
	})
	if err != nil {
		return err
	}
	// The new playground is where this driver stands from now on. It is
	// recorded from the sandbox rather than from os.Getwd, because another
	// driver may already have moved the process on.
	d.ch, d.cwd = ch, ch.box.dir
	d.buf, d.pend = d.buf[:0], classify.PendingInfo{}
	d.banner()
	return nil
}

// banner opens a chapter in the transcript.
//
// The terminal tutor has no need of it — its panel carries the chapter
// title, and its intro says where the playground is. A host that routes
// the panel elsewhere has neither, and would start a student in front of
// an empty box. It also marks, permanently and in place, where one
// chapter's playground ended and the next one's began: a sidebar can only
// ever show where the student is NOW, so the transcript is the only thing
// that can answer "when did my files go away?".
func (d *Driver) banner() {
	e, st := d.ch.e, newStyle(d.opts.Color)
	width := d.opts.Width
	if width <= 0 {
		width = 64
	}
	title := fmt.Sprintf("chapter %d · %s", e.chIdx+1, e.lesson.Title)
	fmt.Fprintf(d.out, "\n%s\n", st.rule(width, title, fmt.Sprintf("%d steps", len(e.lesson.Steps))))
	fmt.Fprintf(d.out, "%s\n\n", st.dim("playground: "+d.ch.box.dir+" — deleted when you leave; experiment freely."))
}

// finish marks the run over. The transcript keeps whatever the engine
// printed last — the completion banner, or :quit's farewell — so a host
// has something to show beside a dead prompt.
func (d *Driver) finish(code int) { d.ended, d.code = true, code }

// echo writes a submitted unit into the transcript under a prompt marker,
// continuation lines included, so the transcript reads as a session rather
// than as disembodied output.
func (d *Driver) echo(src string) {
	st := newStyle(d.opts.Color)
	for i, line := range strings.Split(src, "\n") {
		marker := "▸"
		if i > 0 {
			marker = "…"
		}
		fmt.Fprintf(d.out, "%s %s\n", st.dim(marker), line)
	}
}

// locked runs fn with the process serialized on this driver's working
// directory. See evalGate for why any of this is necessary.
//
// A failed Chdir is not reported: the only way it happens is a playground
// that vanished underneath the student (a stray `rm -rf`, a $TMPDIR
// reaper), and the command they typed will report that far better than a
// message from the tutor would.
func (d *Driver) locked(fn func()) {
	evalGate.Lock()
	defer evalGate.Unlock()
	if d.cwd != "" {
		_ = os.Chdir(d.cwd)
	}
	fn()
	// Whatever the student's `cd` did is where they now stand.
	if wd, err := os.Getwd(); err == nil {
		d.cwd = wd
	}
}
