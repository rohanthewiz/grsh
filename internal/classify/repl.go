package classify

import (
	"errors"
	"slices"
	"sort"
	"strings"
)

// Clone returns an independent copy of the classifier: same declared
// scopes, brace depth, block labels, and package set (shared — it is
// never mutated). Pending/NeedsMore classify speculatively on a clone so
// the real classifier only advances when the input is actually evaluated.
//
// The speculative memo is deliberately NOT copied. A memo is a claim about
// what THIS classifier's state produces, and the clone is about to be
// mutated by File; carrying it over would be the one way this cache could
// serve a stale answer.
func (c *Classifier) Clone() *Classifier {
	return &Classifier{scope: c.scope.clone(), pkgs: c.pkgs, depth: c.depth,
		blocks: slices.Clone(c.blocks)}
}

// specCache memoizes one speculative classify: the Clone+File that Pending
// and Preview each perform.
//
// The editor derives a whole frame from the buffer on every keystroke, and
// three of those derivations classify the SAME source — the highlighter's
// Preview (repl/highlight.go), and the hint lane's shellLineAt (another
// Preview) plus breadcrumb (a Pending). Before this cache each of the three
// cloned the scope chain and re-classified the entire buffer from scratch.
//
// One entry is enough because that is the access pattern: several reads of
// the current buffer, then the buffer changes and the old answer is dead.
// A larger cache would only pay off for input the user is not typing.
//
// This makes Pending and Preview WRITE to the classifier, where before they
// only cloned it. That is safe under the same contract the REPL's other
// per-frame memos already rely on — highlighter, hinter and suggester all
// run on the editor's single read loop, and evaluation happens on that same
// goroutine once Readline returns, so nothing here is concurrent. A host
// embedding a Session and driving Preview from another goroutine was
// already racing with Eval over the classifier pointer; it now races over
// this field too, and still needs its own serialization.
//
// The cache is deliberately NOT extended to runner.RunSource, which
// classifies the same source a fourth time on Enter. Reusing it there means
// handing out the mutated clone for the caller to commit as live session
// state — a much sharper edge than a read-only memo — and it buys 7.4us on
// a multi-line unit, once per command, against the ~36us this saves on
// every keystroke. Measured, not assumed: BenchmarkREPLUnit prints both.
type specCache struct {
	src    string
	chunks []Chunk
	err    error
	depth  int
	blocks []string
}

// speculate classifies src on a clone and remembers the result against c.
//
// Correctness rests on one invariant: the memo is dropped whenever c's own
// state could change. There are exactly three ways that happens, and each
// clears it — File (which mutates scope and depth as it classifies),
// Predeclare (which seeds the root scope), and Clone (which hands out a
// copy that never inherits it). A committing caller like runner.RunSource
// does not mutate the live classifier at all; it REPLACES it with a
// freshly cloned one, whose memo is empty by construction.
func (c *Classifier) speculate(src string) *specCache {
	if c.spec != nil && c.spec.src == src {
		return c.spec
	}
	cc := c.Clone()
	chunks, err := cc.File(src)
	c.spec = &specCache{src: src, chunks: chunks, err: err,
		depth: cc.depth, blocks: cc.blocks}
	return c.spec
}

func (s *Scope) clone() *Scope {
	if s == nil {
		return nil
	}
	names := make(map[string]bool, len(s.names))
	for n := range s.names {
		names[n] = true
	}
	return &Scope{parent: s.parent.clone(), names: names}
}

// PendingInfo describes the state of a (possibly incomplete) REPL input
// unit: whether more lines are needed, how deep the open blocks nest, and
// what those blocks are — innermost last, e.g. {"func greet", "for"}.
// The REPL renders Constructs as a continuation-prompt breadcrumb and
// Depth as auto-indent.
type PendingInfo struct {
	NeedsMore  bool
	Depth      int
	Constructs []string
	// Heredoc marks incompleteness caused by an unterminated heredoc.
	// Auto-indent must stay off in that state: seeded spaces would land in
	// the literal body, and an indented delimiter line would never match.
	Heredoc bool
}

// Pending speculatively classifies src on a clone (c is not mutated) and
// reports its continuation state. Incomplete means: a Go statement that
// ends mid-expression, an unclosed block, a pending heredoc body, or a
// shell line with a trailing continuation (\, |, &&, ||).
func (c *Classifier) Pending(src string) PendingInfo {
	sp := c.speculate(src)
	// Constructs is copied rather than aliased: PendingInfo is a value the
	// caller owns, and the memo behind it now outlives the call.
	info := PendingInfo{Depth: sp.depth, Constructs: slices.Clone(sp.blocks)}
	if err := sp.err; err != nil {
		// Mid-statement Go or an unterminated heredoc: incomplete. Any
		// other classify error is "complete" — Eval will report it.
		info.NeedsMore = errors.Is(err, ErrIncomplete)
		info.Heredoc = errors.Is(err, ErrHeredoc)
		return info
	}
	if sp.depth > 0 {
		info.NeedsMore = true
		return info
	}
	for i := len(sp.chunks) - 1; i >= 0; i-- {
		ch := sp.chunks[i]
		if ch.Kind == Blank {
			continue
		}
		if ch.Kind == Shell {
			_, cont := shellContinues(strings.TrimRight(ch.Text, " \t"))
			info.NeedsMore = cont
		}
		break
	}
	return info
}

// NeedsMore reports whether src is an incomplete REPL input unit.
func (c *Classifier) NeedsMore(src string) bool {
	return c.Pending(src).NeedsMore
}

// Preview chunk-maps src for display (syntax highlighting). It runs on a
// clone — c is never mutated — and never fails: an incomplete trailing
// unit (the norm while the user is typing) comes back as File's
// best-effort tail chunk, so every physical line has a Kind to render.
// The returned slice is shared with the speculative cache and with any
// other caller that previewed the same source this frame. Treat it as
// read-only; the display code that consumes it only reads.
func (c *Classifier) Preview(src string) []Chunk {
	return c.speculate(src).chunks
}

// Names lists every identifier visible in the current scope chain plus the
// registered package names, sorted, for REPL completion.
func (c *Classifier) Names() []string {
	seen := map[string]bool{}
	var out []string
	for sc := c.scope; sc != nil; sc = sc.parent {
		for n := range sc.names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	for n := range c.pkgs {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
