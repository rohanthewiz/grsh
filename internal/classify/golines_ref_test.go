package classify

import (
	"go/token"
	"strings"
)

// This file holds the pre-optimization consumeGo, verbatim, as the oracle
// the incremental version in golines.go is tested against.
//
// It is kept rather than deleted because the rewrite's whole claim is
// "same answers, less work". A comment asserting that is worth nothing; a
// reference implementation the tests can actually run is worth a lot, and
// it costs only a test file. TestConsumeGoMatchesRef sweeps a corpus
// through both and FuzzConsumeGoMatchesRef attacks the equivalence
// directly.
//
// Do not "fix" anything here. Its bugs are the specification: whatever
// this returns is what the classifier returned before the optimization,
// and any deliberate behavior change must land as its own commit that
// updates both sides at once.

// goLineInfo is the per-fragment analysis the reference version re-derives
// for every candidate end line. The live version carries the same three
// counters forward incrementally in goLineState instead.
type goLineInfo struct {
	parens, brackets, braceNet int
	toks                       []tokLit
}

func analyzeGo(frag string) goLineInfo {
	info := goLineInfo{toks: tokensOf(frag)}
	for _, t := range info.toks {
		switch t.tok {
		case token.LPAREN:
			info.parens++
		case token.RPAREN:
			info.parens--
		case token.LBRACK:
			info.brackets++
		case token.RBRACK:
			info.brackets--
		case token.LBRACE:
			info.braceNet++
		case token.RBRACE:
			info.braceNet--
		}
	}
	return info
}

func (i goLineInfo) last() token.Token {
	if len(i.toks) == 0 {
		return token.ILLEGAL
	}
	return i.toks[len(i.toks)-1].tok
}

// opensBlock reports whether a trailing '{' on this fragment starts a
// statement block (classification continues per-line inside) rather than a
// composite literal (which joins lines until braces balance).
func opensBlock(frag string, info goLineInfo) bool {
	t := strings.TrimSpace(frag)
	if strings.HasPrefix(t, "}") || t == "{" {
		return true
	}
	switch firstToken(t) {
	case "if", "for", "switch", "select", "else", "func":
		return true
	}
	// `f := func(...) {` — closure header: last two tokens are ') {'.
	n := len(info.toks)
	if n >= 2 && info.toks[n-1].tok == token.LBRACE && info.toks[n-2].tok == token.RPAREN {
		return true
	}
	return false
}

// consumeGoRef joins physical lines from index i until the Go logical line
// is complete, returning the verbatim text and the last line index.
func consumeGoRef(lines []string, i int) (string, int, error) {
	for j := i; j < len(lines); j++ {
		frag := strings.Join(lines[i:j+1], "\n")
		info := analyzeGo(frag)
		if info.parens > 0 || info.brackets > 0 {
			continue
		}
		last := info.last()
		// `case x:` / `default:` end in a colon but are complete.
		if last == token.COLON {
			switch firstToken(strings.TrimSpace(frag)) {
			case "case", "default":
				return frag, j, nil
			}
		}
		if last == token.LBRACE {
			if opensBlock(frag, info) {
				return frag, j, nil
			}
			continue // composite literal: join until braces balance
		}
		if info.braceNet > 0 && !opensBlock(frag, info) {
			continue // inside a multi-line composite literal
		}
		if semiInsertable(last) {
			return frag, j, nil
		}
	}
	return "", 0, ErrIncomplete
}
