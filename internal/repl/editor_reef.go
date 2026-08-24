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
	"io"
	"strings"

	reef "github.com/reeflective/readline"
	"github.com/reeflective/readline/inputrc"
	"github.com/rohanthewiz/grsh/internal/runner"
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
}

func newReefReader(sess *runner.Session, comp *completer, hist *historyStore) *reefReader {
	// WithApp lets users scope ~/.inputrc directives with `$if grsh`.
	r := &reefReader{rl: reef.NewShell(inputrc.WithApp("grsh"))}

	r.rl.Prompt.Primary(func() string { return r.prompt })
	// Continuation lines get a plain gutter: with real multiline editing
	// the code is visible above, so the legacy per-line breadcrumb moved
	// to the hint lane below the buffer (see the hint provider).
	r.rl.Prompt.Secondary(func() string { return "  ... " })

	// The classifier decides when Enter submits: incomplete units insert a
	// newline and keep editing in-buffer instead of returning fragments.
	// (With the accept-line override below this is only consulted on the
	// delegated stock path — search modes and the like — where a bare
	// newline without indent is the acceptable degraded behavior.)
	r.rl.AcceptMultiline = func(line []rune) bool {
		return !sess.Pending(string(line)).NeedsMore
	}

	// Open-construct breadcrumb, recomputed from the live buffer on every
	// refresh by the display engine. This is the Round 1 "where am I"
	// feature relocated: `func hi() string {⏎` shows `… func hi` under
	// the input until the brace closes.
	r.rl.Hint.SetProvider(func(line []rune, _ int) []rune {
		pend := sess.Pending(string(line))
		if !pend.NeedsMore || len(pend.Constructs) == 0 {
			return nil
		}
		trail := "… " + strings.Join(pend.Constructs, " ▸ ")
		if colorEnabled() {
			trail = "\x1b[2m" + trail + "\x1b[0m" // dim; the hint is ambient, not content
		}
		return []rune(trail)
	})

	r.rl.Completer = comp.completeReef

	// Syntax highlighting, only when the session is color-worthy (same
	// gate as the prompt: NO_COLOR, dumb terminals, non-terminals opt out).
	if colorEnabled() {
		r.rl.SyntaxHighlighter = newHighlighter(sess, comp).highlight
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

	return r
}

// Readline maps reeflective's sentinels onto the loop's editor-neutral
// ones. reeflective returns ErrInterrupt for ^C and io.EOF for ^D on an
// empty line (^D with content is delete-char, as in bash — so unlike the
// legacy editor there is no ^D-mid-continuation case to handle).
func (r *reefReader) Readline() (string, error) {
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
