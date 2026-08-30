package tutor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Meta-commands are the tutor's own vocabulary, taken with a colon
// prefix. The prefix is what makes them free: no shell command and no Go
// statement begins with ':', so `:hint` can never shadow something the
// student meant to run — and the engine claims these before the
// classifier, Eval, or unit history ever see them (see repl.Interceptor's
// Command hook).
//
//	:hint      reveal the next hint for this step
//	:sol       show the canonical solution (and stay on the step)
//	:skip      give up on this step and move on
//	:back      return to the previous step
//	:next      start the next chapter
//	:menu      list the chapters, or jump to one (`:menu 4`)
//	:keep      save this chapter's file before the playground goes
//	:progress  where you are, and what it has cost
//	:help      this list
//	:quit      leave the tutor, keeping your place
//
// A handler is given whatever followed the command name, trimmed — the
// chapter number for `:menu 4`, the destination path for `:keep ~/x`.
// Most ignore it; the two that take an argument treat an empty one as
// "no argument given" rather than as an error, so `:menu` still lists
// and `:keep` still picks its own destination.
type metaCommand struct {
	name string
	help string
	run  func(e *engine, w io.Writer, arg string)
}

// metaCommands is the dispatch table, ordered as :help prints it.
//
// A function rather than a package var because :help renders the table
// itself — as a var, the table and cmdHelp would reference each other and
// Go rejects the initialization cycle. Rebuilding the table on a
// keystroke costs nothing, and the alternative (populating in init) hides
// the table's contents from the file that documents them.
func metaCommands() []metaCommand {
	return []metaCommand{
		{":hint", "reveal the next hint", (*engine).cmdHint},
		{":sol", "show the answer for this step", (*engine).cmdSolution},
		{":skip", "move on without solving this one", (*engine).cmdSkip},
		{":back", "return to the previous step", (*engine).cmdBack},
		{":next", "start the next chapter", (*engine).cmdNext},
		{":menu", "list the chapters (`:menu 4` jumps to one)", (*engine).cmdMenu},
		{":keep", "save this chapter's file out of the playground", (*engine).cmdKeep},
		{":progress", "where you are in this chapter", (*engine).cmdProgress},
		{":help", "this list", (*engine).cmdHelp},
		{":quit", "leave the tutor (your place is saved)", (*engine).cmdQuit},
	}
}

// Command implements repl.Interceptor. It claims any unit whose first
// word starts with ':', so a mistyped `:hnt` is answered by the tutor
// rather than handed to the shell as a command that does not exist —
// a student experimenting with the meta-commands should never be told
// "command not found" by the thing that invented them.
func (e *engine) Command(src string) bool {
	t := strings.TrimSpace(src)
	if !strings.HasPrefix(t, ":") {
		return false
	}
	name, arg, _ := strings.Cut(t, " ")
	for _, c := range metaCommands() {
		if c.name == name {
			c.run(e, e.out, strings.TrimSpace(arg))
			// Persist unconditionally rather than threading a dirty flag
			// through every handler: the write is one row keyed by lesson,
			// and :help costing an upsert is cheaper than a bug where
			// :hint's escalation quietly fails to survive a restart.
			e.save()
			return true
		}
	}
	fmt.Fprintf(e.out, "\n%s %s\n", e.st.warn("?"), e.st.dim("no such tutor command: "+name+" — try :help"))
	return true
}

// cmdHint reveals the next unrevealed hint, and says so when there are
// none left rather than silently doing nothing.
func (e *engine) cmdHint(w io.Writer, _ string) {
	step := e.current()
	if step == nil {
		return
	}
	if e.revealed >= len(step.Hints) {
		if step.Solution != "" {
			fmt.Fprintf(w, "\n%s %s\n", e.st.dim("   no hints left —"), e.st.dim("try :sol for the answer."))
		} else {
			fmt.Fprintf(w, "\n%s\n", e.st.dim("   no hints for this one."))
		}
		return
	}
	fmt.Fprintf(w, "\n   %s %s\n", e.st.label("hint:"), step.Hints[e.revealed])
	e.revealed++
}

// cmdSolution shows the answer but does NOT advance: the student still
// has to run it. Reading a command and typing it are different acts, and
// the second one is the one the muscle memory comes from.
func (e *engine) cmdSolution(w io.Writer, _ string) {
	step := e.current()
	if step == nil {
		return
	}
	if step.Solution == "" {
		fmt.Fprintf(w, "\n%s\n", e.st.dim("   this step has no single answer — anything that works counts."))
		return
	}
	fmt.Fprintf(w, "\n   %s %s\n", e.st.label("answer:"), e.st.code(step.Solution))
	fmt.Fprintf(w, "   %s\n", e.st.dim("run it to move on, or :skip past this step."))
	// Every hint is now moot; marking them revealed keeps :hint honest
	// about having nothing further to offer.
	e.revealed = len(step.Hints)
}

// cmdSkip abandons the current step. The step is not marked passed —
// :progress and the outro count skips separately, so a student can see
// what they left behind.
func (e *engine) cmdSkip(w io.Writer, _ string) {
	if e.current() == nil {
		return
	}
	fmt.Fprintf(w, "\n%s %s\n", e.st.warn("→"), e.st.dim("skipped."))
	e.skipped++
	e.advance(w)
}

// cmdBack returns to the previous step, re-posting its panel with its
// hint state reset — going back is for re-reading, and a student who
// returns to a step deserves the same silence they had the first time.
func (e *engine) cmdBack(w io.Writer, _ string) {
	if e.idx == 0 {
		fmt.Fprintf(w, "\n%s\n", e.st.dim("   already at the first step."))
		return
	}
	e.idx--
	e.attempts, e.revealed, e.posted = 0, 0, false
	// Stepping back out of the outro un-finishes the lesson, or Done
	// would end the session at the next prompt.
	e.finished = false
}

// cmdMenu lists the chapters, and with an argument jumps to one.
//
// The jump is not performed here: the engine records it and Done ends
// the loop so tutor.chapter can rebuild the playground and the session
// around the new chapter. Switching in place would leave the student in
// chapter 5 holding chapter 2's variables and files, which is precisely
// the confusion a tutorial cannot afford.
func (e *engine) cmdMenu(w io.Writer, arg string) {
	if arg != "" {
		e.jumpTo(w, arg)
		return
	}
	fmt.Fprintf(w, "\n%s\n", e.st.bold("chapters"))
	for i, l := range e.chapters {
		marker := "  "
		if i == e.chIdx {
			marker = e.st.ok("▸ ")
		}
		fmt.Fprintf(w, "%s%d. %s %s\n", marker, i+1, l.Title, e.st.dim(fmt.Sprintf("(%d steps)", len(l.Steps))))
	}
	fmt.Fprintf(w, "\n%s\n", e.st.dim("jump with `:menu N` — each chapter starts in a fresh playground."))
}

// cmdNext starts the following chapter. It is the action the outro
// offers, and it works mid-chapter too: a student who has had enough of
// jobs can move on, and the place they left is already saved.
func (e *engine) cmdNext(w io.Writer, _ string) {
	if !e.hasNext() {
		fmt.Fprintf(w, "\n%s\n", e.st.dim("   that was the last chapter — `:menu` lists them all."))
		return
	}
	e.startChapter(w, e.chIdx+1)
}

// jumpTo parses `:menu N` and validates it against the curriculum, so a
// fat-fingered `:menu 40` is answered rather than obeyed.
func (e *engine) jumpTo(w io.Writer, arg string) {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(e.chapters) {
		fmt.Fprintf(w, "\n%s %s\n", e.st.warn("?"),
			e.st.dim(fmt.Sprintf("no chapter %q — have 1..%d", arg, len(e.chapters))))
		return
	}
	if n-1 == e.chIdx {
		fmt.Fprintf(w, "\n%s\n", e.st.dim("   you are already in that chapter."))
		return
	}
	e.startChapter(w, n-1)
}

// startChapter records the jump. The announcement is printed here rather
// than after the rebuild because this is the last moment the OLD panel is
// still on screen — the student's "where did my files go?" is answered
// before it can be asked.
func (e *engine) startChapter(w io.Writer, idx int) {
	e.jump = idx
	fmt.Fprintf(w, "\n%s %s\n", e.st.ok("→"),
		e.st.bold(fmt.Sprintf("chapter %d: %s", idx+1, e.chapters[idx].Title)))
	fmt.Fprintf(w, "  %s\n", e.st.dim("a fresh playground and a fresh session — this chapter's variables and files stay behind."))
}

// cmdKeep copies the chapter's keepsake file out of the playground,
// which is deleted on exit.
//
// Never overwrites. A tutorial writing into someone's home is already
// pushing its luck; doing it over an existing file would be indefensible,
// so a collision is reported and the student names another path.
func (e *engine) cmdKeep(w io.Writer, arg string) {
	name := e.lesson.Keep
	if name == "" {
		fmt.Fprintf(w, "\n%s\n", e.st.dim("   this chapter has no file to keep."))
		return
	}
	body, err := os.ReadFile(filepath.Join(e.dir, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The likely case by far, and not an error from where the student
		// sits: they reached :keep before the step that writes the file.
		fmt.Fprintf(w, "\n%s %s\n", e.st.warn("?"),
			e.st.dim(fmt.Sprintf("no %s in the playground yet — the step that writes it comes first.", name)))
		return
	case err != nil:
		fmt.Fprintf(w, "\n%s %s\n", e.st.warn("?"), e.st.dim("could not read it: "+err.Error()))
		return
	}
	dest, err := keepDest(arg, name)
	if err != nil {
		fmt.Fprintf(w, "\n%s %s\n", e.st.warn("?"), e.st.dim(err.Error()))
		return
	}
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(w, "\n%s %s\n", e.st.warn("?"),
			e.st.dim(dest+" already exists — `:keep <path>` picks somewhere else."))
		return
	}
	// 0o755: `session save` writes a shebang script, and a saved script
	// the student cannot run is a souvenir rather than a tool.
	if err := os.WriteFile(dest, body, 0o755); err != nil {
		fmt.Fprintf(w, "\n%s %s\n", e.st.warn("?"), e.st.dim("could not save it: "+err.Error()))
		return
	}
	e.keepDone = true
	fmt.Fprintf(w, "\n%s %s\n", e.st.ok("✓"), e.st.dim("saved to "+dest))
	fmt.Fprintf(w, "  %s\n", e.st.dim("run it any time with `grsh "+dest+"`."))
}

// keepDest resolves the destination for :keep: the home directory by
// default, an explicit path when given (with ~ expanded and a directory
// argument taking the file's own name).
func keepDest(arg, name string) (string, error) {
	if arg == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("no home directory to save into — `:keep <path>` names one")
		}
		return filepath.Join(home, name), nil
	}
	if arg == "~" || strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("cannot expand ~ — no home directory")
		}
		arg = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(arg, "~"), "/"))
	}
	// A directory argument is an invitation to put the file inside it,
	// which is what `cp` would do and therefore what a shell user expects.
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		return filepath.Join(arg, name), nil
	}
	return arg, nil
}

// cmdProgress reports the student's place, including what it has cost.
// Attempts and skips are shown because they are the honest picture; the
// tutor never hides a miss, since a miss is not a failure state here.
func (e *engine) cmdProgress(w io.Writer, _ string) {
	done := min(e.idx, len(e.lesson.Steps))
	fmt.Fprintf(w, "\n%s  %s\n", e.st.bold(e.lesson.Title),
		e.st.dim(fmt.Sprintf("step %d of %d", min(e.idx+1, len(e.lesson.Steps)), len(e.lesson.Steps))))
	fmt.Fprintf(w, "  %s\n", e.st.dim(fmt.Sprintf("%d passed, %d skipped", done-e.skipped, e.skipped)))
	if step := e.current(); step != nil && e.attempts > 0 {
		fmt.Fprintf(w, "  %s\n", e.st.dim(fmt.Sprintf("%d attempt(s) at this one", e.attempts)))
	}
}

func (e *engine) cmdHelp(w io.Writer, _ string) {
	fmt.Fprintf(w, "\n%s\n", e.st.bold("tutor commands"))
	for _, c := range metaCommands() {
		// The name is padded BEFORE it is coloured: padding a string that
		// already carries escape bytes counts them as visible columns, and
		// every row would lose its alignment by exactly the width of an
		// SGR sequence.
		fmt.Fprintf(w, "  %s %s\n", e.st.code(fmt.Sprintf("%-10s", c.name)), e.st.dim(c.help))
	}
	fmt.Fprintf(w, "\n  %s\n", e.st.dim("everything else is a normal grsh prompt."))
}

// cmdQuit ends the session. Done reports exit code 0: leaving a tutorial
// early is a choice, not a failure, and a nonzero code would break the
// perfectly reasonable `grsh tutor && something-else` without teaching
// anyone anything. The place is already saved — every step transition
// writes it — so there is nothing to flush here.
func (e *engine) cmdQuit(w io.Writer, _ string) {
	fmt.Fprintf(w, "\n%s %s\n", e.st.dim("←"), e.st.dim("leaving the tutor — your place is saved. `grsh tutor` resumes."))
	e.quit = true
}
