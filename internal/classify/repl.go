package classify

import (
	"errors"
	"sort"
	"strings"
)

// Clone returns an independent copy of the classifier: same declared
// scopes, brace depth, and package set (shared — it is never mutated).
// NeedsMore classifies speculatively on a clone so the real classifier
// only advances when the input is actually evaluated.
func (c *Classifier) Clone() *Classifier {
	return &Classifier{scope: c.scope.clone(), pkgs: c.pkgs, depth: c.depth}
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

// NeedsMore reports whether src is an incomplete REPL input unit: a Go
// statement that ends mid-expression, an unclosed block (brace depth still
// positive after classification), or a shell line with a trailing
// continuation (\, |, &&, ||). c is not mutated.
func (c *Classifier) NeedsMore(src string) bool {
	cc := c.Clone()
	chunks, err := cc.File(src)
	if err != nil {
		return errors.Is(err, ErrIncomplete)
	}
	if cc.depth > 0 {
		return true
	}
	for i := len(chunks) - 1; i >= 0; i-- {
		ch := chunks[i]
		if ch.Kind == Blank {
			continue
		}
		if ch.Kind == Shell {
			_, cont := shellContinues(strings.TrimRight(ch.Text, " \t"))
			return cont
		}
		break
	}
	return false
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
