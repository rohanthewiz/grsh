package repl

// Hint-lane content for the reeflective editor: the one dim line printed
// under the input buffer on every display refresh.
//
// Three sources feed it, composed left to right, never clobbering one
// another:
//
//	signature   fmt.Printf(string, ...any) (int, error)
//	            the registry symbol at the cursor, or the one whose call the
//	            cursor is inside — Go signature help, from reflection.
//	alias       ll → ls -la
//	            what the shell command being built expands to; it stays up
//	            while the arguments are typed.
//	breadcrumb  … func greet ▸ for
//	            the open constructs above the cursor (Round 1's "where am I",
//	            relocated here when the editor gained real multiline editing).
//
// Signature and alias are cursor-local and mutually exclusive (a word is
// being read as one language or the other), so at most two segments show at
// once: the cursor-local one, then the breadcrumb.
//
// Cost matters here — this runs on every keystroke AND on every cursor-only
// move, alongside the highlighter and the ghost-text scan. So the lanes are
// ordered cheapest-first and the expensive classifier calls are paid only
// once a cheap string test has already matched:
//
//	trailing selector / open-call scan  →  map lookup in the registry
//	command word of the cursor's segment →  map lookup in the alias table
//	                                       → Session.Preview (clone) ONLY
//	                                         after the name matched an alias
//	Session.Pending (clone)             →  unconditional, but it is the same
//	                                       call the breadcrumb has always made

import (
	"strings"

	"github.com/rohanthewiz/grsh/internal/classify"
	"github.com/rohanthewiz/grsh/internal/runner"
	"github.com/rohanthewiz/grsh/internal/stdlibreg"
)

const (
	// hintSep divides composed segments. A thin vertical bar reads as a
	// gutter rather than as content at dim intensity.
	hintSep = "  ▏ "
	// maxAliasRunes caps an alias expansion; alias values are arbitrary user
	// text and the hint is one line under the input.
	maxAliasRunes = 64
)

// hinter builds the hint text. It memoizes on (buffer, cursor) because the
// display engine refreshes on cursor-only movement too — and unlike the
// highlighter's memo this one must key on the cursor as well, since both
// cursor-local lanes move with it.
//
// Like the highlighter and the suggester it runs on the editor's single read
// loop, so the memo needs no locking. The memo is dropped at each new prompt
// (reefReader.Readline) because it is keyed only on the buffer: session state
// it depends on — the alias table, declared identifiers — can change between
// prompts but never within one.
type hinter struct {
	sess *runner.Session

	memoed  bool
	lastSrc string
	lastPos int
	lastOut string
}

func newHinter(sess *runner.Session) *hinter { return &hinter{sess: sess} }

// reset drops the memo. Called at each fresh prompt.
func (h *hinter) reset() { h.memoed = false }

// hint returns the hint-lane text for the frame being rendered, already
// colored. Empty means "no hint" — the lane collapses.
//
// This is the rune-buffer shape the editor's callback signature has. The
// display path calls hintSrc directly with the frame's already-converted
// buffer (see runeIntern); this wrapper serves callers holding only runes.
func (h *hinter) hint(line []rune, pos int) string {
	return h.hintSrc(string(line), pos)
}

// hintSrc is hint over the buffer as a string. pos stays a RUNE index --
// it is the editor's cursor, and the hint lanes are described in terms of
// where the cursor sits, not which byte it lands on.
func (h *hinter) hintSrc(src string, pos int) string {
	if h.memoed && pos == h.lastPos && src == h.lastSrc {
		return h.lastOut
	}
	out := h.render(src, pos)
	h.memoed, h.lastSrc, h.lastPos, h.lastOut = true, src, pos, out
	return out
}

func (h *hinter) render(src string, pos int) string {
	// The cursor-local lanes read the text BEFORE the cursor. Slicing src
	// rather than re-encoding line[:pos] keeps that free: the prefix shares
	// src's backing array. A cursor outside the buffer is treated as
	// end-of-line -- defensive: a bad cursor must not panic the display.
	prefix := src
	if pos >= 0 {
		prefix = runePrefix(src, pos)
	}

	var parts []string
	if s := h.cursorHint(src, prefix); s != "" {
		parts = append(parts, s)
	}
	if b := h.breadcrumb(src); b != "" {
		parts = append(parts, b)
	}
	if len(parts) == 0 {
		return ""
	}
	out := strings.Join(parts, hintSep)
	if colorEnabled() {
		// Dim as a whole: the hint is ambient context, not content, and one
		// span keeps it from competing with the highlighted buffer above.
		out = "\x1b[2m" + out + "\x1b[0m"
	}
	return out
}

// runePrefix returns src truncated at rune index pos, or all of src when
// pos is past the end. The result is a slice of src, not a copy -- which is
// the point: this runs on every frame the hint lane is not memoized for.
func runePrefix(src string, pos int) string {
	if pos <= 0 {
		return ""
	}
	n := 0
	for i := range src { // ranging a string steps by rune, i is the byte offset
		if n == pos {
			return src[:i]
		}
		n++
	}
	return src
}

// cursorHint is the cursor-local segment: Go signature help when a registry
// symbol is in play, otherwise a shell alias expansion.
func (h *hinter) cursorHint(src, prefix string) string {
	if sig, ok := goSignature(prefix); ok {
		return sig
	}
	return h.aliasHint(src, prefix)
}

// goSignature describes the registry symbol the cursor is working on:
//
//	strings.Spli|              nothing — the symbol does not resolve yet
//	strings.Split|             the word at the cursor
//	strings.Split(x, |         the call the cursor is inside
//	fmt.Println(strings.Split| the word wins over the enclosing call: it is
//	                           what the user is typing right now
//
// A `pkg.Sym` that resolves in the registry is itself sufficient evidence
// that this is Go — no classifier call needed. (A shell line containing a
// literal `strings.Split` would be classified Go anyway.)
func goSignature(prefix string) (string, bool) {
	// Ordered so the cheap test runs first: trailingSelector walks back a few
	// bytes, callee scans the whole prefix.
	if sig, ok := registrySignature(trailingSelector(prefix)); ok {
		return sig, true
	}
	return registrySignature(callee(prefix))
}

// registrySignature resolves a dotted name against the registry.
func registrySignature(name string) (string, bool) {
	pkg, sym, ok := strings.Cut(name, ".")
	if !ok {
		return "", false
	}
	return stdlibreg.Signature(pkg, sym)
}

// aliasHint shows what a shell alias expands to while the command that uses
// it is being built: `ll → ls -la`. It follows the COMMAND word, not the word
// under the cursor, so the expansion stays visible while the arguments are
// typed — the same way signature help stays up inside a call.
func (h *hinter) aliasHint(src, prefix string) string {
	// The scan below is line-local: a command word belongs to its physical
	// line, and the segment scanner treats a newline as ordinary text.
	linePrefix := prefix
	if i := strings.LastIndexByte(linePrefix, '\n'); i >= 0 {
		linePrefix = linePrefix[i+1:]
	}
	word := commandWord(linePrefix)
	if word == "" {
		return ""
	}
	exp, ok := h.sess.Alias(word)
	if !ok {
		return ""
	}
	// Only now — after a name has actually matched the alias table — is it
	// worth a classifier clone to confirm the line really is shell. Without
	// this, an alias named like a Go identifier would hint over Go code.
	if !h.shellLineAt(src, prefix) {
		return ""
	}
	return word + " → " + oneLine(exp, maxAliasRunes)
}

// commandWord returns the first word of the shell command segment the cursor
// is in — the name of the command being built. `echo hi | ll -x` yields "ll",
// `echo ll` yields "echo" (there, ll is an argument). Quoted runs are skipped
// so a separator inside a string does not start a phantom segment.
//
// Following the segment rather than the cursor's own word means a partially
// typed name ("l") simply does not match the alias table, and no hint shows
// until the name is complete.
func commandWord(linePrefix string) string {
	start := 0
	for i := 0; i < len(linePrefix); i++ {
		switch linePrefix[i] {
		case '\'', '"', '`':
			i = closeQuote(linePrefix, i) - 1 // shell quoting rules: highlight.go
		case '|', '&', ';', '(':
			// Pipes, logical operators, separators, and a subshell/command
			// substitution opener all begin a new command. Redirections do not.
			start = i + 1
		}
	}
	fields := strings.Fields(linePrefix[start:])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// shellLineAt reports whether the classifier reads the cursor's physical
// line as shell.
func (h *hinter) shellLineAt(src, prefix string) bool {
	ln := strings.Count(prefix, "\n") + 1 // 1-based, like Chunk.StartLine
	for _, ch := range h.sess.Preview(src) {
		if ln >= ch.StartLine && ln <= ch.EndLine {
			return ch.Kind == classify.Shell
		}
	}
	return false
}

// breadcrumb names the constructs still open above the cursor.
func (h *hinter) breadcrumb(src string) string {
	pend := h.sess.Pending(src)
	if !pend.NeedsMore || len(pend.Constructs) == 0 {
		return ""
	}
	return "… " + strings.Join(pend.Constructs, " ▸ ")
}

// trailingSelector returns the dotted identifier ending exactly at the end of
// prefix — "strings.Split" for `x := strings.Split`, "" for `strings.Split(`
// (the paren ends the word) or for anything not identifier-shaped.
func trailingSelector(prefix string) string {
	i := len(prefix)
	for i > 0 {
		c := prefix[i-1]
		if c == '.' || c == '_' ||
			c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			i--
			continue
		}
		break
	}
	return prefix[i:]
}

// callee returns the name being called by the innermost call still open at
// the end of prefix — `fmt.Println(strings.Join(parts, ` yields
// "strings.Join". Parens inside string/char literals and comments are
// skipped, so an unbalanced one in text cannot wedge the stack.
//
// This is a bracket matcher, not a parser: it has no idea whether the name
// before a `(` is a func, a conversion, or a grouping paren with a bare
// expression in front. It does not need to — an unresolvable name simply
// produces no hint.
func callee(prefix string) string {
	var stack []string
	for i := 0; i < len(prefix); i++ {
		switch c := prefix[i]; c {
		case '"', '\'':
			i = skipGoQuote(prefix, i) - 1 // -1: the loop's i++ lands past it
		case '`':
			if j := strings.IndexByte(prefix[i+1:], '`'); j >= 0 {
				i += j + 1
			} else {
				i = len(prefix) // unterminated raw string: the rest is text
			}
		case '/':
			if i+1 < len(prefix) {
				switch prefix[i+1] {
				case '/':
					if j := strings.IndexByte(prefix[i:], '\n'); j >= 0 {
						i += j
					} else {
						i = len(prefix)
					}
				case '*':
					if j := strings.Index(prefix[i+2:], "*/"); j >= 0 {
						i += j + 3
					} else {
						i = len(prefix)
					}
				}
			}
		case '(':
			stack = append(stack, trailingSelector(prefix[:i]))
		case ')':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if len(stack) == 0 {
		return ""
	}
	return stack[len(stack)-1]
}

// skipGoQuote returns the index just past the interpreted literal opened at
// i, honoring backslash escapes (so `'\”` is one literal, not two). An
// unterminated literal runs to the end — while typing, that is the norm.
// Distinct from the highlighter's closeQuote, which follows SHELL rules
// where a single-quoted run has no escapes at all.
func skipGoQuote(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case q:
			return j + 1
		case '\n':
			return j // interpreted literals do not span lines
		}
	}
	return len(s)
}

// oneLine flattens text to a single row and caps its length. Control
// characters are dropped rather than escaped: the hint is measured for
// cursor math with the escape sequences this file adds, and a stray ESC or
// newline from user-defined text would corrupt that measurement.
func oneLine(s string, maxRunes int) string {
	var b strings.Builder
	b.Grow(len(s))
	n := 0
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			r = ' '
		}
		if n == maxRunes {
			b.WriteRune('…')
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
