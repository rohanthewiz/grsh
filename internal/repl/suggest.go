package repl

// Fish-style ghost text (inline autosuggestion) for the reeflective editor.
//
// While you type, the most recent history unit that STARTS WITH the current
// buffer is drawn dimmed after the cursor; → / ^F accepts all of it, and a
// forward-word key accepts one word. Nothing is ever inserted implicitly:
// the ghost lives in the display engine's inline-suggestion slot, not in the
// line buffer, so what `Readline` returns is only ever what was typed or
// explicitly accepted.
//
// Why a hand-rolled matcher instead of the library's `history-autosuggest`:
// that option suggests from the merged history sources with no filtering,
// and grsh's history is a UNIT store — a recalled entry can be a whole
// multi-line `func`. Ghost text containing a newline is unrenderable here
// (the display engine measures the suggestion as a single logical line, and
// the secondary-prompt gutter belongs between real buffer rows), so those
// entries must be skipped rather than shown. Owning the match also lets us
// pick when a suggestion is appropriate at all — see updateGhost.

import "strings"

// suggester finds ghost text for a buffer by prefix-matching the unit
// history, newest first.
//
// The result is memoized on the buffer string because the display engine
// refreshes on cursor-only movement too: without the memo every arrow key
// would rescan the whole history.
type suggester struct {
	hist    *historyStore
	lastSrc string
	lastSug string
}

func newSuggester(hist *historyStore) *suggester { return &suggester{hist: hist} }

// suggest returns the full suggested line (buffer prefix included, which is
// the shape the display engine expects), or "" when nothing matches.
//
// It runs on the editor's single read loop, so the memo needs no locking —
// same contract as the highlighter's.
func (s *suggester) suggest(src string) string {
	if src == s.lastSrc {
		return s.lastSug
	}
	sug := s.match(src)
	s.lastSrc, s.lastSug = src, sug
	return sug
}

func (s *suggester) match(src string) string {
	// An all-blank buffer matches nearly everything, which makes the ghost
	// flicker on the first keystroke of every prompt; wait for real input.
	// A buffer that already spans rows cannot carry ghost text at all.
	if strings.TrimSpace(src) == "" || strings.Contains(src, "\n") {
		return ""
	}
	units := s.hist.Units()
	for i := len(units) - 1; i >= 0; i-- { // newest first: most recent wins
		unit := units[i]
		// Strictly longer, or there is nothing to show; single-line, or the
		// suggestion cannot be rendered after the cursor (see file comment).
		if len(unit) <= len(src) || strings.Contains(unit, "\n") {
			continue
		}
		if strings.HasPrefix(unit, src) {
			return unit
		}
	}
	return ""
}
