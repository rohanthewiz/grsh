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
	})
	for _, km := range []string{"emacs", "vi-insert", "vi-command"} {
		_ = r.rl.Config.Bind(km, "\x03", "grsh-interrupt", false)
		_ = r.rl.Config.Bind(km, "\x1a", "grsh-noop", false)
	}

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
