package repl

// reeflective/readline adapter — the default editor.
//
// Why a second editor exists at all: chzyer's RuneBuffer owns its cursor
// math in a way that makes ANSI escapes inside the line buffer impossible
// (so no syntax highlighting or ghost-text there), and its multiline story
// is simulated — the loop re-reads physical lines and joins them.
// reeflective is multiline-native and exposes exactly the hooks the
// roadmap needs: AcceptMultiline (classifier-driven continuation),
// SyntaxHighlighter, inline suggestions, hint lanes, and full inputrc
// support (vi mode for free). chzyer stays available behind
// GRSH_EDITOR=legacy as the escape hatch.
//
// Division of labor with the loop:
//
//	loop (repl.go)                    reefReader
//	──────────────                    ──────────
//	buf accumulation ── never fires ─ AcceptMultiline consults
//	(Pending on a returned            sess.Pending per Enter, so an
//	unit is complete by               unfinished func/if/heredoc stays
//	construction)                     in ONE editable buffer and
//	                                  Readline returns the whole unit
//
// The loop's continuation machinery is kept (unchanged) because the
// legacy editor still needs it; with reeflective it is simply dormant.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	reef "github.com/reeflective/readline"
	"github.com/reeflective/readline/inputrc"
	"github.com/rohanthewiz/grsh/internal/runner"
	"golang.org/x/term"
)

// indentUnit is one auto-indent level: two spaces per open block, the
// same visual step the legacy continuation prompt used. Spaces, not tabs
// — a fed tab rune would dispatch as the completion key.
const indentUnit = "  "

// reefReader adapts reeflective/readline to the loop's lineReader seam.
type reefReader struct {
	rl *reef.Shell
	// prompt holds the latest SetPrompt value; the Primary callback closes
	// over the reader so the loop's per-iteration SetPrompt keeps working
	// against reeflective's callback-based prompt engine.
	prompt string
	// sess backs the per-refresh callbacks (breadcrumb hint, auto-indent
	// depth, electric brace): they all ask the classifier about the live
	// buffer.
	sess *runner.Session
	// hints builds the line under the buffer: signature help, alias
	// expansion, and the open-construct breadcrumb (see hint.go).
	hints *hinter
	// ghost is the inline-autosuggestion matcher, or nil when ghost text is
	// disabled (see the colorEnabled gate in newReefReader).
	ghost *suggester
	// ghostHold suppresses the ghost while a line is being accepted; see
	// AcceptMultiline for why that moment needs a clear screen.
	ghostHold bool
	// color caches colorEnabled() for the callbacks that run on every
	// refresh (the gutter): the check stats the terminal, and a keystroke
	// already pays for a classifier pass and a history scan.
	color bool
}

func newReefReader(sess *runner.Session, comp *completer, hist *historyStore) *reefReader {
	// WithApp lets users scope ~/.inputrc directives with `$if grsh`.
	r := &reefReader{rl: reef.NewShell(inputrc.WithApp("grsh")), sess: sess, color: colorEnabled()}
	r.hints = newHinter(sess)

	r.rl.Prompt.Primary(func() string { return r.prompt })
	// Continuation lines get a plain gutter: with real multiline editing
	// the code is visible above, so the legacy per-line breadcrumb moved
	// to the hint lane below the buffer (see the hint provider).
	r.rl.Prompt.Secondary(r.secondary)

	// ...and the gutter only gets painted at all when a multiline column is
	// enabled. The display engine's indicator pass (renderMultilineIndicators)
	// returns early unless one of the multiline-column* options is set OR the
	// primary prompt is empty — the secondary prompt rides along with the
	// column, it is not independent of it. The library means to default this
	// to on (its own builtin option table says true), but the inputrc defaults
	// it parses first already define the key as false, and builtin options are
	// only applied to keys that are still unset — so the intended default
	// never lands and continuation rows come up bare. Set it explicitly.
	_ = r.rl.Config.Set("multiline-column", true)

	// The classifier decides when Enter submits: incomplete units insert a
	// newline and keep editing in-buffer instead of returning fragments.
	// (With the accept-line override below this is only consulted on the
	// delegated stock path — search modes and the like — where a bare
	// newline without indent is the acceptable degraded behavior.)
	r.rl.AcceptMultiline = func(line []rune) bool {
		accept := !sess.Pending(string(line)).NeedsMore
		// Take the ghost down on the way out. This callback is the library's
		// last step before Display.AcceptLine on every acceptance path, and
		// AcceptLine's coordinate pass measures the line INCLUDING the inline
		// suggestion: a ghost still set here makes the engine walk past it and
		// leave it printed, as if the suggestion had been typed. The hold is
		// lifted at the next Readline call.
		r.ghostHold = accept
		return accept
	}

	// The hint lane — signature help, alias expansion, and the
	// open-construct breadcrumb — recomputed from the live buffer on every
	// refresh by the display engine. See hint.go for what each lane shows.
	//
	// The provider doubles as grsh's per-refresh hook, because the display
	// engine calls it from computeCoordinates -- after the effective buffer
	// has been resolved and BEFORE the inline suggestion is measured and
	// painted. Updating the ghost here is therefore consistent within one
	// frame; doing it from the SyntaxHighlighter (which runs later, during
	// the line render) would leave the coordinate math a frame stale and
	// mis-place the cursor whenever the ghost wraps.
	r.rl.Hint.SetProvider(r.hintProvider)

	r.rl.Completer = comp.completeReef

	// Syntax highlighting, only when the session is color-worthy (same
	// gate as the prompt: NO_COLOR, dumb terminals, non-terminals opt out).
	if r.color {
		r.rl.SyntaxHighlighter = newHighlighter(sess, comp).highlight
		// Ghost text rides the same gate: the library paints the suggestion
		// with a hardcoded dim/gray SGR run, so with color suppressed it
		// would be indistinguishable from text the user actually typed.
		r.ghost = newSuggester(hist)
	}

	// Recall is backed by the unit store: up-arrow restores a whole
	// multi-line func as one editable buffer — the legacy editor could
	// only replay it line by line.
	r.rl.History.Add("units", unitSource{hist})

	// Multiline pastes arrive as one buffer edit instead of a stream of
	// keystrokes — no per-line evaluation, and literal tabs in pasted
	// code don't trigger completion.
	_ = r.rl.Config.Set("enable-bracketed-paste", true)

	// UTF-8 sanity. GNU readline flips both of these automatically in
	// UTF-8 locales; this library keeps byte-era defaults, which corrupt
	// non-ASCII input — grsh assumes UTF-8 terminals outright (an
	// ~/.inputrc can still override):
	//   convert-meta on  → high-bit input bytes rewritten as ESC+char
	//   output-meta  off → self-insert of a rune ≥ U+0080 goes through
	//                      strutil.Quote, so é lands in the LINE BUFFER
	//                      as a literal ESC+i meta chord
	_ = r.rl.Config.Set("convert-meta", false)
	_ = r.rl.Config.Set("output-meta", true)

	// Raw mode clears ISIG, so ^C and ^Z arrive as plain bytes and both
	// default to self-insert — which would put literal ETX/SUB bytes into
	// the eval source. Give them shell semantics instead:
	//
	//   ^C → grsh-interrupt: accept the line with ErrInterrupt, exactly
	//        what the library's own abort does for its ^G terminator.
	//        The loop then drops the input and reprompts, like bash.
	//   ^Z → grsh-noop: truly inert. No SIGTSTP can reach the parent (the
	//        chzyer footgun this replaces); ^Z during a foreground
	//        command still suspends that job — the terminal is cooked
	//        (ISIG on) while commands run. Not the library's "abort":
	//        that probes InputIsTerminator, which dispatches against
	//        pending keys and eats the next typed-ahead keystroke.
	r.rl.Keymap.Register(map[string]func(){
		"grsh-interrupt": func() {
			r.ghostHold = true // same reason as in AcceptMultiline
			r.rl.Display.AcceptLine()
			r.rl.History.Accept(false, false, reef.ErrInterrupt)
		},
		"grsh-noop": func() {},
		// Electric closing brace: when } is the first non-space character
		// on its line, drop one indent level first — Enter's auto-indent
		// seeds body depth, so the brace that CLOSES the block belongs one
		// level back (gofmt style). Anywhere else it is a plain insert.
		// Off inside heredoc bodies, where leading spaces are content.
		"grsh-electric-brace": func() {
			line, cur := r.rl.Line(), r.rl.Cursor()
			pos := cur.Pos()
			start := pos
			for start > 0 && (*line)[start-1] != '\n' {
				start--
			}
			prefix := string((*line)[start:pos])
			if len(prefix) >= len(indentUnit) && strings.Trim(prefix, " ") == "" &&
				!sess.Pending(string((*line)[:pos])).Heredoc {
				line.Cut(pos-len(indentUnit), pos)
				cur.Set(pos - len(indentUnit))
			}
			cur.InsertAt('}')
		},
	})
	for _, km := range []string{"emacs", "vi-insert", "vi-command"} {
		_ = r.rl.Config.Bind(km, "\x03", "grsh-interrupt", false)
		_ = r.rl.Config.Bind(km, "\x1a", "grsh-noop", false)
	}
	// } dedents only where it would self-insert; in vi-command it stays a
	// paragraph motion.
	for _, km := range []string{"emacs", "vi-insert"} {
		_ = r.rl.Config.Bind(km, "}", "grsh-electric-brace", false)
	}

	// Auto-indent, via an accept-line override rather than the tempting
	// Keys.Feed of spaces: fed macro keys lose the race against type-ahead
	// already sitting in the input buffer (the key stack serves buffered
	// bytes before macro keys), so a fast `{`⏎`}`⏎ leaked the indent into
	// a LATER prompt. Overriding the command makes the newline and its
	// indent one synchronous buffer edit during the Enter dispatch — no
	// ordering to lose, and it works in every keymap, vi-command included.
	//
	// The stock command (captured before the override lands in the same
	// registry) still runs for everything this doesn't own: complete units,
	// and any active local mode (isearch, completion menu), where the line
	// buffer isn't plainly editable. On those delegated paths the
	// AcceptMultiline callback above provides the stock bare-newline
	// continuation. Known blind spot, accepted: non-incremental history
	// search sets no local keymap and is invisible from the public API, so
	// an Enter there on an incomplete buffer indents instead of accepting
	// the search — obscure mode, recoverable outcome.
	acceptLine := r.rl.Keymap.Commands()["accept-line"]
	r.rl.Keymap.Register(map[string]func(){
		"accept-line": func() {
			if string(r.rl.Keymap.Local()) == "" {
				line, cur := r.rl.Line(), r.rl.Cursor()
				if pend := sess.Pending(string(*line)); pend.NeedsMore {
					ind := pend
					if pos := cur.Pos(); pos < line.Len() {
						// Enter mid-buffer: indent for the depth at the
						// cursor, not at the end of the buffer.
						ind = sess.Pending(string((*line)[:pos]))
					}
					depth := 0
					if !ind.Heredoc { // heredoc bodies are literal: never indent
						depth = ind.Depth
					}
					cur.InsertAt(append([]rune{'\n'},
						[]rune(strings.Repeat(indentUnit, depth))...)...)
					return
				}
			}
			acceptLine()
		},
	})

	// Partial ghost-text acceptance, fish style: a forward-word key takes the
	// next word of the suggestion instead of moving into empty space. The
	// whole-suggestion case needs no wiring — the library's forward-char
	// already falls back to the inline suggestion when the cursor is at the
	// end of the line, so → and ^F accept it out of the box.
	//
	// Overriding the COMMAND rather than binding keys means every sequence
	// that already resolves to forward-word (\ef, \e[1;5C, ...) inherits the
	// behavior, in whichever keymap the user has it. inline-suggest-accept-word
	// is a no-op unless a suggestion applies at the cursor, so "did the buffer
	// grow?" is a sufficient (and library-version-proof) test for whether to
	// fall through to the plain motion.
	acceptSuggestWord := r.rl.Keymap.Commands()["inline-suggest-accept-word"]
	forwardWord := r.rl.Keymap.Commands()["forward-word"]
	r.rl.Keymap.Register(map[string]func(){
		"forward-word": func() {
			before := r.rl.Line().Len()
			acceptSuggestWord()
			if r.rl.Line().Len() == before {
				forwardWord()
			}
		},
	})

	return r
}

// gutterMark is the continuation marker painted on every row of a pending
// multiline unit — the same ellipsis the legacy editor uses for its
// continuation prompt, so both editors read the same way.
const gutterMark = "... "

// secondary renders the continuation gutter for the whole input buffer.
//
// The display engine calls this once per refresh and only for the LAST
// continuation row: its indicator pass walks the rows top-down painting
// ui.DefaultMultilineColumn ("│ ") on each one, and substitutes the secondary
// prompt on the final row. grsh wants one uniform gutter rather than that
// two-glyph tree, and the engine exposes no hook for the other rows — so this
// callback repaints them itself and then returns the mark for its own row.
//
// Reaching up the screen from a prompt callback is safe here because of where
// in the frame it runs and what the engine does right afterwards:
//
//   - Only the ROW has to be preserved. The engine has just moved to column 0
//     of the last row, and once the pass finishes it CRs and re-forwards to
//     lineCol — the column we leave behind is discarded. So: up N-1, paint
//     downwards, land back where we started.
//   - Mistakes cannot accumulate. displayLine emits ClearLineBefore at the
//     start of every continuation row, so the whole gutter area is wiped and
//     repainted from scratch on each keystroke.
//   - The mark cannot collide with the code: it is sized to the prompt width
//     (see gutter), which is exactly where the code begins, and the rows above
//     hold nothing but the engine's own indicator at column 0.
func (r *reefReader) secondary() string {
	mark := r.gutter()

	// Continuation rows == newlines in the buffer, the same count the engine
	// walked to get here (display.Engine uses core.Line.Lines()).
	rows := strings.Count(string(*r.rl.Line()), "\n")
	if rows < 2 || r.wrapped() {
		return mark // nothing above to repaint, or not safe to try
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\x1b[%dA", rows-1) // up to the first continuation row
	for range rows - 1 {
		b.WriteString(mark)
		// Column 0, one row down — as cursor motion, not as a newline. The
		// engine's own pass writes a bare "\n" here and so leans on the tty
		// still doing ONLCR (raw mode leaves OPOST alone, but only just); a
		// CUD cannot scroll the screen either, and every row we step down to
		// is one we climbed up from a moment ago.
		b.WriteString("\r\x1b[1B")
	}
	b.WriteString(mark)
	return b.String()
}

// gutter builds one row of gutter: gutterMark right-aligned in a field as wide
// as the primary prompt. Right-aligned, because the engine indents
// continuation rows to the prompt width — a mark parked at column 0 would sit
// stranded far to the left of the code under a wide prompt:
//
//	grsh ~/projs/go/grsh> func greet(name string) {
//	                 ...    if name == "" {
//	                 ...      return
//
// (The legacy editor prints the same mark at column 0 only because there the
// gutter IS the prompt, with the code starting right after it.)
//
// A prompt too narrow to hold the mark truncates it rather than letting it
// run into the buffer text.
func (r *reefReader) gutter() string {
	cols := promptCols(r.prompt)
	if cols == 0 {
		cols = 2 // empty prompt: the engine reserves its own 2-column indicator
	}
	mark := gutterMark
	if cols < len(mark) {
		mark = mark[:cols]
	}
	pad := strings.Repeat(" ", cols-len(mark))
	if r.color {
		mark = "\x1b[2m" + mark + "\x1b[0m" // dim, like the engine's own column
	}
	return pad + mark
}

// wrapped reports whether any buffer row is long enough to wrap at the
// terminal width. It gates the multi-row repaint above: the engine's indicator
// pass counts LOGICAL lines while it travels by VISUAL rows, so wrapped input
// already lands its indicators on the wrong rows — and a gutter drawn against
// those rows would smear the mark over code instead of over an indicator.
// Wrapped buffers therefore keep the mark on the single row the engine
// positioned us on, which is the stock behavior.
func (r *reefReader) wrapped() bool {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		width = 80 // the engine's own fallback when it cannot size the terminal
	}
	start := promptCols(r.prompt)
	for _, ln := range strings.Split(string(*r.rl.Line()), "\n") {
		if start+utf8.RuneCountInString(ln) >= width {
			return true
		}
	}
	return false
}

// promptCols is the printed width of the prompt: SGR escapes carry no width,
// and only the last line of a multi-line prompt shares the input row. This
// mirrors the start column the engine computes for the code (which measures
// grapheme clusters — near enough for a gutter that only pads).
func promptCols(prompt string) int {
	if i := strings.LastIndexByte(prompt, '\n'); i >= 0 {
		prompt = prompt[i+1:]
	}
	n := 0
	for i := 0; i < len(prompt); {
		if prompt[i] == '\x1b' {
			// Skip the escape: CSI (ESC [ ... final byte in @..~) covers every
			// sequence the prompt renderer and the color tags can emit.
			j := i + 1
			if j < len(prompt) && prompt[j] == '[' {
				for j++; j < len(prompt) && (prompt[j] < '@' || prompt[j] > '~'); j++ {
				}
			}
			if j < len(prompt) {
				j++
			}
			i = j
			continue
		}
		_, size := utf8.DecodeRuneInString(prompt[i:])
		n++
		i += size
	}
	return n
}

// hintProvider is the display engine's per-refresh callback. It returns the
// hint-lane text and, on the way, refreshes the ghost text — see
// newReefReader for why the two share this hook.
func (r *reefReader) hintProvider(line []rune, pos int) []rune {
	r.updateGhost(line, pos)

	if h := r.hints.hint(line, pos); h != "" {
		return []rune(h)
	}
	return nil // nil, not an empty slice: the lane collapses either way
}

// updateGhost recomputes the inline autosuggestion for the frame being
// rendered. Called from the hint provider (see newReefReader for why that
// is the right moment), so it runs on the editor's read loop only.
func (r *reefReader) updateGhost(line []rune, pos int) {
	if r.ghost == nil {
		return
	}
	// Only offer ghost text in the plain editing state:
	//   - ghostHold: the line is on its way out; the display engine must see
	//     an unadorned buffer while it walks past it.
	//   - a local keymap active means the buffer handed to us is a minibuffer
	//     (incremental search) or carries a virtually inserted completion
	//     candidate — extending either is nonsense.
	//   - away from the end of the buffer the library refuses to paint or
	//     accept a suggestion anyway, so skip the history scan entirely.
	sug := ""
	if !r.ghostHold && string(r.rl.Keymap.Local()) == "" && pos == len(line) {
		sug = r.ghost.suggest(string(line))
	}
	r.rl.SetInlineSuggestion(sug) // "" clears any previous ghost
}

// Readline maps reeflective's sentinels onto the loop's editor-neutral
// ones. reeflective returns ErrInterrupt for ^C and io.EOF for ^D on an
// empty line (^D with content is delete-char, as in bash — so unlike the
// legacy editor there is no ^D-mid-continuation case to handle).
func (r *reefReader) Readline() (string, error) {
	r.ghostHold = false // fresh prompt: the ghost is welcome again
	r.hints.reset()     // ...and the hint memo may be stale (aliases, idents)
	line, err := r.rl.Readline()
	if errors.Is(err, reef.ErrInterrupt) {
		return line, errInterrupt
	}
	return line, err
}

func (r *reefReader) SetPrompt(p string) { r.prompt = p }

// unitSource exposes the unit history store to readline's recall engine.
// Write is deliberately a no-op: the loop persists units via hist.Append
// after command dispatch (so `?x` and friends stay out of `session save`
// scripts), and readline's accept-time write would double-record. The
// store is shared, so a unit the loop appends is recallable at the very
// next prompt.
type unitSource struct{ store *historyStore }

func (u unitSource) Write(string) (int, error) { return u.store.LenUnits(), nil }

func (u unitSource) GetLine(i int) (string, error) {
	units := u.store.Units()
	if i < 0 || i >= len(units) {
		return "", io.EOF
	}
	return units[i], nil
}

func (u unitSource) Len() int  { return u.store.LenUnits() }
func (u unitSource) Dump() any { return u.store.Units() }

// completeReef shapes the shared candidate set for reeflective: whole-word
// values that replace the typed word (PREFIX), with directories chaining
// slash-to-slash instead of getting the usual accepted-candidate space.
func (c *completer) completeReef(line []rune, cursor int) reef.Completions {
	word, matches := c.matches(string(line[:cursor]))
	if len(matches) == 0 {
		return reef.Completions{}
	}
	return reef.CompleteValues(matches...).Prefix(word).NoSpace('/')
}
