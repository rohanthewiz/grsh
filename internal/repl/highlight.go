package repl

// Syntax highlighting for the reeflective editor.
//
// The library calls SyntaxHighlighter with the FULL multiline buffer on
// every display refresh and repaints whatever string comes back, so the
// one hard rule here is: the visible characters must be exactly the input
// — only SGR color sequences (\x1b[...m, the form the display engine
// knows how to skip when doing cursor math) may be added, and no color
// may span a physical newline (each buffer row is painted independently,
// and the secondary-prompt gutter sits between rows).
//
// Coloring is language-aware via the same classifier that drives
// evaluation: Session.Preview maps buffer lines to Shell/Go/Blank chunks
// (clone-based, tolerant of the half-typed input that is the norm here),
// then each chunk is lexed with the matching scheme:
//
//	Go     go/scanner over the whole chunk (so raw strings and general
//	       comments spanning lines color correctly): keywords, strings,
//	       numbers, comments.
//	Shell  a small per-line lexer: the command-position word green when
//	       it resolves (builtin, $PATH, alias, explicit path) and red
//	       when it does not — fish-style typo radar — plus flags,
//	       $variables, quoted strings, comments. {go-interpolations}
//	       are skipped (left default) rather than guessed at.
//	Blank  whole-line #/// comments dimmed.

import (
	"go/scanner"
	"go/token"
	"strings"

	"github.com/rohanthewiz/grsh/internal/classify"
	"github.com/rohanthewiz/grsh/internal/runner"
)

// The palette sticks to the basic 8 ANSI colors so it inherits the user's
// terminal theme instead of fighting it.
const (
	hlReset   = "\x1b[0m"
	hlKeyword = "\x1b[35m" // magenta: Go keywords
	hlString  = "\x1b[33m" // yellow: quoted strings, both languages
	hlNumber  = "\x1b[36m" // cyan: Go numeric literals
	hlComment = "\x1b[2m"  // dim: comments
	hlKnown   = "\x1b[32m" // green: command that resolves
	hlUnknown = "\x1b[31m" // red: command that does not
	hlFlag    = "\x1b[36m" // cyan: -f / --flags
	hlVar     = "\x1b[35m" // magenta: $variables
)

// highlighter memoizes the last render: the display engine refreshes on
// cursor-only movement too, and the buffer is unchanged for those.
type highlighter struct {
	sess    *runner.Session
	comp    *completer
	lastSrc string
	lastOut string
}

func newHighlighter(sess *runner.Session, comp *completer) *highlighter {
	return &highlighter{sess: sess, comp: comp}
}

// highlight is the reeflective SyntaxHighlighter hook. It runs on the
// editor's single read loop, so the memo needs no locking.
func (h *highlighter) highlight(line []rune) string {
	src := string(line)
	if src == h.lastSrc {
		return h.lastOut
	}
	out := h.render(src)
	h.lastSrc, h.lastOut = src, out
	return out
}

// render colorizes src line by line according to its chunk map. Any line
// the chunk map somehow misses stays uncolored — plain text is always a
// safe fallback.
func (h *highlighter) render(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, len(lines))
	copy(out, lines)

	for _, ch := range h.sess.Preview(src) {
		lo, hi := ch.StartLine-1, ch.EndLine-1
		if lo < 0 || hi >= len(lines) || lo > hi {
			continue // defensive: never let a bad range garble the paint
		}
		switch ch.Kind {
		case classify.Go:
			// Scan the chunk as one fragment — verbatim per consumeGo, so
			// scanner byte offsets index straight into it.
			frag := strings.Join(lines[lo:hi+1], "\n")
			for k, cl := range strings.Split(highlightGo(frag), "\n") {
				if lo+k <= hi {
					out[lo+k] = cl
				}
			}
		case classify.Shell:
			// Physical lines, not the joined chunk text (joinShell rewrites
			// continuations). Command position carries across lines only
			// through an explicit trailing pipe/logical operator; heredoc
			// body lines therefore drop out of it naturally.
			cmdPos := true
			for k := lo; k <= hi; k++ {
				out[k], cmdPos = h.shellLine(lines[k], cmdPos)
			}
		case classify.Blank:
			if t := strings.TrimSpace(lines[lo]); strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
				out[lo] = hlComment + lines[lo] + hlReset
			}
		}
	}
	return strings.Join(out, "\n")
}

// highlightGo rewrites a Go fragment with token colors. Only tokens whose
// literal text is present verbatim in the source (keywords, basic
// literals, comments) are colored, so byte offsets stay exact; scan
// errors are ignored — fragments are usually incomplete while typing.
func highlightGo(frag string) string {
	fset := token.NewFileSet()
	file := fset.AddFile("", -1, len(frag))
	var s scanner.Scanner
	s.Init(file, []byte(frag), func(token.Position, string) {}, scanner.ScanComments)

	var b strings.Builder
	prev := 0
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		color := goTokenColor(tok)
		if color == "" || lit == "" {
			continue
		}
		off := file.Offset(pos)
		if off < prev || off+len(lit) > len(frag) || frag[off:off+len(lit)] != lit {
			continue // offset/literal mismatch (e.g. auto-inserted semicolon): skip
		}
		b.WriteString(frag[prev:off])
		writeColored(&b, color, lit)
		prev = off + len(lit)
	}
	b.WriteString(frag[prev:])
	return b.String()
}

func goTokenColor(tok token.Token) string {
	switch {
	case tok.IsKeyword():
		return hlKeyword
	case tok == token.STRING || tok == token.CHAR:
		return hlString
	case tok == token.INT || tok == token.FLOAT || tok == token.IMAG:
		return hlNumber
	case tok == token.COMMENT:
		return hlComment
	}
	return ""
}

// writeColored emits text wrapped in color, restarting the color after
// every newline so no SGR span crosses a buffer row (raw strings and
// general comments are the multi-line tokens this matters for).
func writeColored(b *strings.Builder, color, text string) {
	for i, seg := range strings.Split(text, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		if seg != "" {
			b.WriteString(color)
			b.WriteString(seg)
			b.WriteString(hlReset)
		}
	}
}

// shellLine colorizes one physical shell line. cmdPos says whether the
// first word sits at a command position; the returned bool says whether
// the NEXT physical line will (i.e. this one ends with |, && or ||
// — a backslash continuation continues the argument list instead).
func (h *highlighter) shellLine(s string, cmdPos bool) (string, bool) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			b.WriteByte(c)
			i++

		case c == '\\': // escape: the pair is plain text
			end := min(i+2, len(s))
			b.WriteString(s[i:end])
			i = end

		case c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			// Word-start comment: rest of line. (Also pre-empts the
			// library's own comment-begin regex, which would otherwise
			// re-wrap it in its hardcoded gray.)
			writeSpan(&b, hlComment, s[i:])
			i = len(s)

		case c == '\'' || c == '"' || c == '`':
			j := closeQuote(s, i)
			writeSpan(&b, hlString, s[i:j])
			i = j

		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			// Command substitution: the operator stays plain, and what
			// follows is a command position again.
			b.WriteString("$(")
			i += 2
			cmdPos = true

		case c == '$':
			j := varEnd(s, i)
			if j > i+1 {
				writeSpan(&b, hlVar, s[i:j])
			} else {
				b.WriteByte(c) // lone $: plain
			}
			i = j

		case c == '{':
			// {go expression} interpolation: leave it default rather than
			// half-guess Go colors with a shell lexer.
			j := braceEnd(s, i)
			b.WriteString(s[i:j])
			i = j

		case c == '(' || c == ')' || c == '<' || c == '>':
			// Grouping and redirection punctuation stays plain; a bare (
			// opens a subshell, so a command follows it.
			b.WriteByte(c)
			i++
			if c == '(' {
				cmdPos = true
			}

		case c == '|' || c == '&' || c == ';':
			// Pipes, logical operators, separators, background &: plain
			// text, and the next word is a command again.
			j := i
			for j < len(s) && (s[j] == '|' || s[j] == '&' || s[j] == ';') {
				j++
			}
			b.WriteString(s[i:j])
			i = j
			cmdPos = true

		default:
			j := wordEnd(s, i)
			word := s[i:j]
			switch {
			case cmdPos && strings.Contains(word, "="):
				// FOO=bar prefix assignment: plain, and the command is
				// still to come.
				b.WriteString(word)
			case cmdPos:
				if h.comp.knownCommand(word) || h.sess.IsAlias(word) {
					writeSpan(&b, hlKnown, word)
				} else {
					writeSpan(&b, hlUnknown, word)
				}
				cmdPos = false
			case strings.HasPrefix(word, "-"):
				writeSpan(&b, hlFlag, word)
			default:
				b.WriteString(word)
			}
			i = j
		}
	}

	trimmed := strings.TrimRight(s, " \t")
	next := strings.HasSuffix(trimmed, "|") || strings.HasSuffix(trimmed, "&&") ||
		strings.HasSuffix(trimmed, "||")
	return b.String(), next
}

// writeSpan wraps text in one color. Callers only pass single-line spans.
func writeSpan(b *strings.Builder, color, text string) {
	if text == "" {
		return
	}
	b.WriteString(color)
	b.WriteString(text)
	b.WriteString(hlReset)
}

// closeQuote returns the index just past the quote opened at i, or
// len(s) when it never closes (an unterminated string colors to end of
// line — which is what it is). Backslash escapes count inside " only.
func closeQuote(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' && q == '"' {
			j++
			continue
		}
		if s[j] == q {
			return j + 1
		}
	}
	return len(s)
}

// varEnd returns the index just past a $variable reference at i:
// $name, ${...}, or a special like $?, $#, $$, $!, $*, $@.
func varEnd(s string, i int) int {
	j := i + 1
	if j >= len(s) {
		return j
	}
	if s[j] == '{' {
		if end := strings.IndexByte(s[j:], '}'); end >= 0 {
			return j + end + 1
		}
		return len(s)
	}
	if strings.ContainsRune("?#$!*@", rune(s[j])) {
		return j + 1
	}
	for j < len(s) && (s[j] == '_' ||
		s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' ||
		s[j] >= '0' && s[j] <= '9') {
		j++
	}
	return j
}

// braceEnd returns the index just past the {expr} region opened at i,
// respecting nesting and Go string literals — the display twin of
// classify's skipGoBrace. Unclosed regions run to end of line.
func braceEnd(s string, i int) int {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '"', '\'', '`':
			q := s[j]
			j++
			for j < len(s) && s[j] != q {
				if s[j] == '\\' && q != '`' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return len(s)
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(s)
}

// wordEnd returns the index just past the plain word starting at i — it
// breaks on whitespace and on every character another lexer case owns.
func wordEnd(s string, i int) int {
	j := i
	for j < len(s) && !strings.ContainsRune(" \t\\'\"`${|&;()<>", rune(s[j])) {
		j++
	}
	if j == i {
		j++ // never stall: an unclaimed byte passes through as itself
	}
	return j
}
