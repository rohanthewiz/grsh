package tutor

// View is the engine's state as data.
//
// The terminal tutor needs nothing like this: it renders the lesson by
// printing it, and the panel IS the state. A host with a second surface —
// a sidebar next to a transcript — needs the same facts as values, because
// the alternative is scraping them back out of the text the engine just
// printed, and a UI built on parsing its own output breaks the first time
// someone rewrites a sentence.
//
// It is a snapshot, taken between units. Nothing in it is a live reference
// into the engine, so a host may hold one, serialize it, and compare it
// with the last.

// View is everything a host needs to draw the lesson. Fields are ordered
// as a renderer reads them: where the student is, what the step asks, what
// help they have earned, and how the run ends.
type View struct {
	// Chapters is the whole curriculum, for a table of contents. It does
	// not change during a run, but it ships with every snapshot so a host
	// that reconnects mid-lesson needs only one message to draw.
	Chapters []ChapterView `json:"chapters"`
	Chapter  int           `json:"chapter"` // 0-based index into Chapters
	Title    string        `json:"title"`

	Step  int `json:"step"`  // 1-based; equal to Steps+1 once the chapter is finished
	Steps int `json:"steps"` // steps in this chapter

	// Prose is the current step's text, one entry per line, carrying the
	// two inline marks the format allows: `backticks` and **stars**. It is
	// handed over unrendered on purpose — a terminal turns those into
	// escape codes and an HTML host into elements, and the engine has no
	// business guessing which.
	Prose []string `json:"prose"`
	// Try is the literal the step offers as a starting point, if any. May
	// be several lines.
	Try string `json:"try"`

	// Hints holds only the hints the student has actually earned or asked
	// for. The unrevealed ones are deliberately absent rather than hidden
	// behind a flag: a hint that reaches the browser is a hint that can be
	// read in the page source, which would make :hint theatre.
	Hints []string `json:"hints"`
	// MoreHints reports that :hint still has something to give.
	MoreHints bool `json:"moreHints"`
	// Solution is the canonical answer, and is empty until the student
	// asks with :sol or misses often enough to be shown it — same reason.
	Solution string `json:"solution"`
	// HasSolution says a solution exists to ask for, without giving it
	// away, so a host can grey out the button rather than offer nothing.
	HasSolution bool `json:"hasSolution"`

	Attempts int `json:"attempts"` // misses at the current step
	Skipped  int `json:"skipped"`  // steps left behind in this chapter

	// Pending reports a half-typed input unit: an unclosed block or a
	// trailing shell continuation. A host shows a continuation prompt and
	// keeps reading, exactly as the REPL does.
	Pending bool `json:"pending"`
	// Explain is the chapter's `explain: on` directive — the host should
	// show the classifier's live verdict (see Driver.Classify).
	Explain bool `json:"explain"`

	// Keep names a file this chapter created that outlives the playground,
	// once it exists and until it has been saved out; empty otherwise. It
	// is the offer, so a host can put a button where the terminal puts a
	// sentence about `:keep`.
	Keep string `json:"keep"`

	// Finished is the chapter's steps all passed or skipped. Ended is the
	// whole run over — the last chapter finished, :quit, or `exit` — after
	// which Submit does nothing.
	Finished bool `json:"finished"`
	Ended    bool `json:"ended"`
	Code     int  `json:"code"`

	// Dir is the playground on disk. Worth showing: this is a real
	// directory that a tour of a shell should not be coy about.
	Dir string `json:"dir"`
}

// ChapterView is one entry in the table of contents.
type ChapterView struct {
	Title string `json:"title"`
	Steps int    `json:"steps"`
	// Done is "this student finished this chapter", not "this chapter
	// comes before the one they are in". The two differ for anyone who
	// jumped, and only the first is true.
	Done bool `json:"done"`
}

// View snapshots the driver. Safe to call whenever the host is not inside
// a Submit, which is the same rule as everything else here.
func (d *Driver) View() View {
	v := View{
		Chapters: make([]ChapterView, len(d.all)),
		Ended:    d.ended,
		Code:     d.code,
	}
	for i, l := range d.all {
		v.Chapters[i] = ChapterView{Title: l.Title, Steps: len(l.Steps), Done: d.done[i]}
	}
	if d.ch != nil && d.ch.e.finished {
		// The chapter in progress ticks over the moment it is finished,
		// rather than when the student happens to leave it.
		v.Chapters[d.ch.e.chIdx].Done = true
	}
	if d.ch == nil {
		// Closed, or torn down after `exit`. The table of contents still
		// answers "what was this?", which is what a host showing a finished
		// run needs.
		return v
	}

	e := d.ch.e
	v.Chapter, v.Title = e.chIdx, e.lesson.Title
	v.Steps, v.Finished = len(e.lesson.Steps), e.finished
	v.Attempts, v.Skipped = e.attempts, e.skipped
	v.Pending, v.Explain = d.pend.NeedsMore, e.lesson.Explain
	v.Dir = d.ch.box.dir
	// Step counts from 1 and runs one PAST the last step when the chapter
	// is done, which is how the outro's "8 of 8" reads as complete rather
	// than as still standing on the last one.
	v.Step = e.idx + 1

	if step := e.current(); step != nil {
		v.Prose, v.Try = step.Prose, step.Try
		v.HasSolution = step.Solution != ""
		// Only what has been revealed: see the field comments above.
		v.Hints = step.Hints[:min(e.revealed, len(step.Hints))]
		v.MoreHints = e.revealed < len(step.Hints)
		if e.answered {
			// Not "the hints have run out": a step with no hints at all
			// satisfies that on its very first prompt, and would ship its
			// answer to the browser before the student had typed anything.
			// The engine records the moment it actually shows the answer.
			v.Solution = step.Solution
		}
	}
	if e.keepPending() {
		v.Keep = e.lesson.Keep
	}
	return v
}
