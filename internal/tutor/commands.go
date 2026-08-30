package tutor

import (
	"fmt"
	"io"
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
//	:menu      list the chapters
//	:progress  where you are, and what it has cost
//	:help      this list
//	:quit      leave the tutor, keeping your place
//
// Each returns whether the engine should keep running; only :quit says no.
type metaCommand struct {
	name string
	help string
	run  func(e *engine, w io.Writer)
}

// metaCommands is the dispatch table, ordered as :help prints it.
//
// A function rather than a package var because :help renders the table
// itself — as a var, the table and cmdHelp would reference each other and
// Go rejects the initialization cycle. Rebuilding eight structs on a
// keystroke costs nothing, and the alternative (populating in init) hides
// the table's contents from the file that documents them.
func metaCommands() []metaCommand {
	return []metaCommand{
		{":hint", "reveal the next hint", (*engine).cmdHint},
		{":sol", "show the answer for this step", (*engine).cmdSolution},
		{":skip", "move on without solving this one", (*engine).cmdSkip},
		{":back", "return to the previous step", (*engine).cmdBack},
		{":menu", "list the chapters", (*engine).cmdMenu},
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
	name, _, _ := strings.Cut(t, " ")
	for _, c := range metaCommands() {
		if c.name == name {
			c.run(e, e.out)
			// Persist unconditionally rather than threading a dirty flag
			// through eight handlers: the write is one row keyed by lesson,
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
func (e *engine) cmdHint(w io.Writer) {
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
func (e *engine) cmdSolution(w io.Writer) {
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
func (e *engine) cmdSkip(w io.Writer) {
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
func (e *engine) cmdBack(w io.Writer) {
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

// cmdMenu lists the chapters and how to jump to one. Jumping happens by
// relaunching (`grsh tutor 4`) rather than in-place: a chapter switch
// would need a fresh sandbox and a fresh session, and restarting the
// process is the honest way to get both.
func (e *engine) cmdMenu(w io.Writer) {
	fmt.Fprintf(w, "\n%s\n", e.st.bold("chapters"))
	for i, l := range e.chapters {
		marker := "  "
		if l.ID == e.lesson.ID {
			marker = e.st.ok("▸ ")
		}
		fmt.Fprintf(w, "%s%d. %s %s\n", marker, i+1, l.Title, e.st.dim(fmt.Sprintf("(%d steps)", len(l.Steps))))
	}
	fmt.Fprintf(w, "\n%s\n", e.st.dim("jump with: grsh tutor N"))
}

// cmdProgress reports the student's place, including what it has cost.
// Attempts and skips are shown because they are the honest picture; the
// tutor never hides a miss, since a miss is not a failure state here.
func (e *engine) cmdProgress(w io.Writer) {
	done := min(e.idx, len(e.lesson.Steps))
	fmt.Fprintf(w, "\n%s  %s\n", e.st.bold(e.lesson.Title),
		e.st.dim(fmt.Sprintf("step %d of %d", min(e.idx+1, len(e.lesson.Steps)), len(e.lesson.Steps))))
	fmt.Fprintf(w, "  %s\n", e.st.dim(fmt.Sprintf("%d passed, %d skipped", done-e.skipped, e.skipped)))
	if step := e.current(); step != nil && e.attempts > 0 {
		fmt.Fprintf(w, "  %s\n", e.st.dim(fmt.Sprintf("%d attempt(s) at this one", e.attempts)))
	}
}

func (e *engine) cmdHelp(w io.Writer) {
	fmt.Fprintf(w, "\n%s\n", e.st.bold("tutor commands"))
	for _, c := range metaCommands() {
		fmt.Fprintf(w, "  %-10s %s\n", e.st.code(c.name), e.st.dim(c.help))
	}
	fmt.Fprintf(w, "\n  %s\n", e.st.dim("everything else is a normal grsh prompt."))
}

// cmdQuit ends the session. Done reports exit code 0: leaving a tutorial
// early is a choice, not a failure, and a nonzero code would break the
// perfectly reasonable `grsh tutor && something-else` without teaching
// anyone anything. The place is already saved — every step transition
// writes it — so there is nothing to flush here.
func (e *engine) cmdQuit(w io.Writer) {
	fmt.Fprintf(w, "\n%s %s\n", e.st.dim("←"), e.st.dim("leaving the tutor — your place is saved. `grsh tutor` resumes."))
	e.quit = true
}
