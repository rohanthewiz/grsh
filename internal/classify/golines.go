package classify

import (
	"errors"
	"go/scanner"
	"go/token"
	"strings"
)

// ErrIncomplete marks source that ends mid-statement (unclosed paren,
// composite literal, or trailing binary operator). A script run reports it
// as a parse error; the REPL treats it as "read another line".
var ErrIncomplete = errors.New("incomplete Go statement at end of script")

// ErrHeredoc marks the specific incompleteness of an unterminated heredoc.
// It always travels together with ErrIncomplete (errors.Is matches both);
// the distinction exists because the REPL's auto-indent must stay OFF while
// a heredoc body is being read — seeded spaces would become literal body
// content and an indented delimiter line would never match.
var ErrHeredoc = errors.New("unterminated heredoc")

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

// goSrc is the byte view of the source File is classifying, plus the byte
// offset of each physical line, so a Go logical line can be lexed straight
// out of the original bytes instead of from a freshly joined fragment.
//
// It exists because consumeGo used to be quadratic. The old shape was
// "join lines[i:j+1], lex the whole thing, ask whether it is complete;
// if not, j++ and do it all again" — so an n-line composite literal or a
// pasted multi-line call cost n joins and n full lexes, O(n^2) in both
// time and allocation. Measured before the change: 619 ns/line at 8
// lines, 16,256 ns/line at 512, where one 512-line pass burned 8.4ms and
// 22MB (ai_docs/perf/round3-baseline.txt).
//
// Sharing the bytes is what keeps the fix from moving the cost rather
// than removing it. Sub-slicing gs.b is free, so lexing from line i is
// O(1) to start and O(bytes actually consumed) to run; summed over every
// logical line in the file that is O(n) total. Handing consumeGo a
// `lines[i:]` join instead would have made the composite-literal case
// linear while making a file of many short Go lines quadratic — the
// go-block shape in BenchmarkFile exists to catch exactly that regression.
//
// Both fields are built on first use rather than up front: a buffer with
// no Go in it — a pipeline, a heredoc, most of what gets typed at a shell
// prompt — would otherwise pay a full copy of the source plus an int per
// line for an index it never reads.
type goSrc struct {
	src  string
	b    []byte // src as bytes, for scanner.Init
	offs []int  // byte offset at which each line of b starts
}

// newGoSrc prepares to index src by line. lines must be
// strings.Split(src, "\n"); nothing is allocated until the first Go
// logical line asks for it.
func newGoSrc(src string) *goSrc { return &goSrc{src: src} }

// index materializes the byte view and the line table. The +1 per line
// accounts for the separator Split removed, which is what makes offs[k] an
// exact index into b.
func (gs *goSrc) index(lines []string) {
	if gs.offs != nil {
		return
	}
	gs.b = []byte(gs.src)
	gs.offs = make([]int, len(lines))
	off := 0
	for k, ln := range lines {
		gs.offs[k] = off
		off += len(ln) + 1
	}
}

// goLineState is the running token state the completion rules need: the
// three nesting counters, plus the last two significant tokens (comments
// and auto-inserted semicolons never enter it, matching tokensOf).
//
// It replaces re-deriving a goLineInfo from a re-lex per candidate line —
// every rule below reads only these fields, so carrying them forward
// across lines gives the same answers for a fraction of the work.
type goLineState struct {
	parens, brackets, braceNet int
	last, prev                 token.Token
	n                          int // significant tokens seen
}

func (st *goLineState) add(tok token.Token) {
	switch tok {
	case token.LPAREN:
		st.parens++
	case token.RPAREN:
		st.parens--
	case token.LBRACK:
		st.brackets++
	case token.RBRACK:
		st.brackets--
	case token.LBRACE:
		st.braceNet++
	case token.RBRACE:
		st.braceNet--
	}
	st.prev, st.last = st.last, tok
	st.n++
}

// consumeGo joins physical lines from index i until the Go logical line is
// complete, returning the verbatim text and the last line index.
//
// The source is lexed ONCE, forward, and completion is tested at each line
// boundary against the state accumulated so far. The equivalence with the
// original re-lex-per-prefix version is not argued, it is tested:
// golines_ref_test.go keeps that version verbatim as an oracle and
// TestConsumeGoMatchesRef / FuzzConsumeGoMatchesRef require identical
// (text, end, err) on every input.
//
// # Why tokens are attributed to their START line
//
// Only two Go tokens can span physical lines: a raw string and a general
// /*...*/ comment. Attributing by start line is what preserves the old
// behavior for both.
//
// The old code lexed a TRUNCATED fragment, so a raw string opened on line
// j came back as an (unterminated, error-ignored) STRING token belonging
// to line j — and STRING is semicolon-insertable, so the logical line
// ended there. Lexing the whole source instead yields one COMPLETE STRING
// token, still starting on line j: same token kind, same line, same
// verdict. A general comment is dropped by both. So the two disagree only
// about the literal text of a token neither one reads.
//
// The upshot is that a multi-line raw string still terminates the logical
// line at its opening line, exactly as before. That is arguably wrong Go,
// but it is pre-existing behavior and this change is a performance change:
// fixing it belongs in its own commit, with its own test.
func (gs *goSrc) consumeGo(lines []string, i int) (string, int, error) {
	gs.index(lines)

	// Facts about the head of the fragment that the block-vs-composite rule
	// needs. They are constant across every candidate end line, which is
	// what lets the loop below never rebuild the joined fragment:
	//
	//   - File never calls consumeGo on a blank line, so the first
	//     non-space character of the joined fragment is always the first
	//     non-space character of lines[i].
	//   - firstToken stops at the newline Join inserts, so it too can only
	//     see lines[i].
	head := strings.TrimSpace(lines[i])
	headCloses := strings.HasPrefix(head, "}")
	headTok := firstToken(head)
	// bareBrace tracks the original's `t == "{"` test: true while the whole
	// fragment still trims to a single open brace. It is cleared the moment
	// a later line contributes anything — including a comment, which is why
	// this is checked against the raw line rather than against tokens.
	bareBrace := head == "{"

	start := gs.offs[i]
	fset := token.NewFileSet()
	f := fset.AddFile("", -1, len(gs.b)-start)
	var s scanner.Scanner
	// Errors are ignored, as before: a fragment mid-statement is expected to
	// be unlexable, and that is the signal to read another line.
	s.Init(f, gs.b[start:], func(token.Position, string) {}, 0)

	var st goLineState
	st.last, st.prev = token.ILLEGAL, token.ILLEGAL

	// opens mirrors opensBlock: does a trailing '{' start a statement block
	// (keep classifying per line) or a composite literal (join until the
	// braces balance)?
	opens := func() bool {
		if headCloses || bareBrace {
			return true
		}
		switch headTok {
		case "if", "for", "switch", "select", "else", "func":
			return true
		}
		// `f := func(...) {` — closure header: last two tokens are ') {'.
		return st.n >= 2 && st.last == token.LBRACE && st.prev == token.RPAREN
	}

	// complete applies the original rule chain to the state accumulated
	// through some candidate end line. Order matters and is preserved.
	complete := func() bool {
		if st.parens > 0 || st.brackets > 0 {
			return false
		}
		// `case x:` / `default:` end in a colon but are complete.
		if st.last == token.COLON {
			switch headTok {
			case "case", "default":
				return true
			}
		}
		if st.last == token.LBRACE {
			return opens() // otherwise a composite literal: keep joining
		}
		if st.braceNet > 0 && !opens() {
			return false // inside a multi-line composite literal
		}
		return semiInsertable(st.last)
	}

	// cur is the line whose tokens are still being accumulated; it is only
	// safe to test completion for a line once a token belonging to a LATER
	// line has been seen (or the source has run out).
	cur := i
	advance := func(to int) (string, int, bool) {
		for cur < to {
			if complete() {
				return strings.Join(lines[i:cur+1], "\n"), cur, true
			}
			cur++
			if strings.TrimSpace(lines[cur]) != "" {
				bareBrace = false
			}
		}
		return "", 0, false
	}

	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue // auto-inserted, never part of the token stream
		}
		if tok == token.COMMENT {
			continue
		}
		// f.Line is 1-based within the sub-source starting at line i.
		if ln := i + f.Line(pos) - 1; ln > cur {
			if text, end, ok := advance(ln); ok {
				return text, end, nil
			}
		}
		st.add(tok)
	}
	// Source exhausted: every remaining line is a candidate end line, and
	// the state can no longer change.
	if text, end, ok := advance(len(lines) - 1); ok {
		return text, end, nil
	}
	if complete() {
		return strings.Join(lines[i:], "\n"), len(lines) - 1, nil
	}
	return "", 0, ErrIncomplete
}

// constructLabel names the block a Go logical line opens, for the REPL's
// continuation-prompt breadcrumb: `func greet(...) {` → "func greet",
// `for i := range xs {` → "for", `f := func() {` → "func f",
// `} else {` → "else", anything else → "{".
func constructLabel(text string) string {
	t := strings.TrimSpace(text)
	tok := firstToken(t)
	switch tok {
	case "func":
		if name := firstToken(strings.TrimSpace(t[len("func"):])); name != "" {
			return "func " + name
		}
		return "func"
	case "if", "for", "switch", "select":
		return tok
	}
	if strings.HasPrefix(t, "}") {
		// `} else {` / `} else if ... {` reopen a branch.
		if strings.Contains(t, "else if") {
			return "else if"
		}
		if strings.Contains(t, "else") {
			return "else"
		}
	}
	// Closure bound to a name: `handler := func(...) {`.
	if strings.Contains(t, "func") && tok != "" {
		return "func " + tok
	}
	return "{"
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

	// The first brace a line opens gets a label derived from its leading
	// construct ("func greet", "for", ...); any further braces on the
	// same logical line (composite literals inside a block header) are
	// anonymous. Balanced lines push and pop in one pass, so only truly
	// open blocks remain on the stack.
	labeled := false

	// Push/pop scopes as braces open/close. We fold declaration recording
	// into the same pass so `for i := range` vars land in a live scope.
	for k, t := range toks {
		switch t.tok {
		case token.LBRACE:
			c.depth++
			c.scope = NewScope(c.scope)
			label := "{"
			if !labeled {
				label = constructLabel(text)
				labeled = true
			}
			c.blocks = append(c.blocks, label)
		case token.RBRACE:
			if c.depth > 0 {
				c.depth--
				if c.scope.parent != nil {
					c.scope = c.scope.parent
				}
				if len(c.blocks) > 0 {
					c.blocks = c.blocks[:len(c.blocks)-1]
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
