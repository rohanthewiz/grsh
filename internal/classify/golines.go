package classify

import (
	"go/scanner"
	"go/token"
	"strings"

	"github.com/rohanthewiz/serr"
)

type tokLit struct {
	tok token.Token
	lit string
}

// tokensOf scans a Go fragment, dropping auto-inserted semicolons and
// comments. Scan errors are ignored — fragments are often incomplete.
func tokensOf(frag string) []tokLit {
	fset := token.NewFileSet()
	f := fset.AddFile("", -1, len(frag))
	var s scanner.Scanner
	s.Init(f, []byte(frag), func(token.Position, string) {}, 0)
	var out []tokLit
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		if tok == token.COMMENT {
			continue
		}
		out = append(out, tokLit{tok, lit})
	}
}

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

// semiInsertable implements Go's semicolon-insertion rule: a logical line
// can end after these tokens.
func semiInsertable(tok token.Token) bool {
	switch tok {
	case token.IDENT, token.INT, token.FLOAT, token.IMAG, token.CHAR,
		token.STRING, token.BREAK, token.CONTINUE, token.FALLTHROUGH,
		token.RETURN, token.INC, token.DEC, token.RPAREN, token.RBRACK,
		token.RBRACE:
		return true
	}
	return false
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

// consumeGo joins physical lines from index i until the Go logical line is
// complete, returning the verbatim text and the last line index.
func consumeGo(lines []string, i int) (string, int, error) {
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
	return "", 0, serr.New("incomplete Go statement at end of script")
}

// predeclare records top-level-looking func/var/const/type names so
// forward references classify correctly (pass 0). Over-approximation of
// nesting is deliberate and harmless.
func (c *Classifier) predeclare(lines []string) {
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		for _, kw := range []string{"func ", "var ", "const ", "type "} {
			if rest, ok := strings.CutPrefix(t, kw); ok {
				if name := firstToken(strings.TrimSpace(rest)); name != "" {
					c.scope.Add(name)
				}
			}
		}
	}
}

// trackGoLine records declarations from a completed Go logical line and
// applies brace depth / scope push-pop.
func (c *Classifier) trackGoLine(text string) {
	toks := tokensOf(text)

	// Push/pop scopes as braces open/close. We fold declaration recording
	// into the same pass so `for i := range` vars land in a live scope.
	for k, t := range toks {
		switch t.tok {
		case token.LBRACE:
			c.depth++
			c.scope = NewScope(c.scope)
		case token.RBRACE:
			if c.depth > 0 {
				c.depth--
				if c.scope.parent != nil {
					c.scope = c.scope.parent
				}
			}
		case token.DEFINE:
			// Walk back over `ident, ident :=`.
			for b := k - 1; b >= 0; b-- {
				if toks[b].tok == token.IDENT {
					c.scope.Add(toks[b].lit)
					if b == 0 || toks[b-1].tok != token.COMMA {
						break
					}
					b-- // skip the comma
					continue
				}
				break
			}
		case token.VAR, token.CONST, token.TYPE:
			for b := k + 1; b < len(toks); b++ {
				if toks[b].tok == token.IDENT {
					c.scope.Add(toks[b].lit)
					if b+1 < len(toks) && toks[b+1].tok == token.COMMA {
						b++
						continue
					}
				}
				break
			}
		case token.FUNC:
			// `func name(` → add name; add all idents in the param list
			// (over-approx: includes type names, harmless for rule 6a).
			b := k + 1
			if b < len(toks) && toks[b].tok == token.IDENT {
				c.scope.Add(toks[b].lit)
				b++
			}
			if b < len(toks) && toks[b].tok == token.LPAREN {
				depth := 1
				for b++; b < len(toks) && depth > 0; b++ {
					switch toks[b].tok {
					case token.LPAREN:
						depth++
					case token.RPAREN:
						depth--
					case token.IDENT:
						c.scope.Add(toks[b].lit)
					}
				}
			}
		}
	}
}
