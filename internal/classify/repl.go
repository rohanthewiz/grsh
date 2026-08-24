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
func (c *Classifier) Clone() *Classifier {
	return &Classifier{scope: c.scope.clone(), pkgs: c.pkgs, depth: c.depth,
		blocks: slices.Clone(c.blocks)}
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
	cc := c.Clone()
	chunks, err := cc.File(src)
	info := PendingInfo{Depth: cc.depth, Constructs: cc.blocks}
	if err != nil {
		// Mid-statement Go or an unterminated heredoc: incomplete. Any
		// other classify error is "complete" — Eval will report it.
		info.NeedsMore = errors.Is(err, ErrIncomplete)
		info.Heredoc = errors.Is(err, ErrHeredoc)
		return info
	}
	if cc.depth > 0 {
		info.NeedsMore = true
		return info
	}
	for i := len(chunks) - 1; i >= 0; i-- {
		ch := chunks[i]
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
func (c *Classifier) Preview(src string) []Chunk {
	chunks, _ := c.Clone().File(src)
	return chunks
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
