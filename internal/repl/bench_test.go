package repl

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/runner"
)

// Benchmarks for the per-keystroke display path.
//
// The plan's original perf targets were all interpreter-side (hot loops,
// interpolation). Those are the wrong thing to measure first for a shell:
// nobody runs a tight `for` at a prompt, but everybody types. Between two
// keystrokes the display engine re-derives the entire frame from the whole
// buffer — highlight, hint lane, ghost text — and each of those runs its
// own full classify pass. That is the latency budget a user actually feels,
// and until now nothing measured it.
//
// # One op = one keystroke
//
// Every benchmark here advances the buffer by one rune per iteration rather
// than re-rendering a fixed buffer. Two reasons, both load-bearing:
//
//  1. ns/op then reads directly as "how long the shell is busy after I press
//     a key" — comparable against the ~16ms frame budget without arithmetic.
//  2. The highlighter, hinter and suggester each memoize on the buffer
//     string (highlight.go:66, hint.go:78, suggest.go:42). Re-rendering a
//     fixed buffer would hit the memo every iteration and measure a map
//     lookup. Typing is precisely what defeats those memos, so the
//     benchmark must type.
//
// Calling reset() to defeat the memo would be the other option and is
// wrong: the editor never resets mid-line (only at a fresh prompt), so it
// would measure a state the REPL is never in.
//
// # The shapes
//
// short   — a one-line shell command: the overwhelmingly common case.
// pathcmd — a command word containing '/', which routes knownCommand
//           (completer.go:213) to an os.Stat instead of the cached $PATH
//           set: one syscall per keystroke, worth seeing separately.
// goline  — a single Go statement: the Go lexer lane.
// pending — typing the 21st line of an already-open 20-line Go block. This
//           is the case the classify benchmarks predict is quadratic, and
//           the one where the display engine's several classify passes per
//           frame compound.

// benchSession builds a session with $PATH pinned to a temp dir holding one
// executable, so knownCommand's cached scan is small and deterministic
// rather than however many thousand binaries the host happens to have.
func benchSession(b *testing.B) *runner.Session {
	b.Helper()
	dir := b.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "okcmd"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		b.Fatal(err)
	}
	b.Setenv("PATH", dir)
	var out, errB bytes.Buffer
	return runner.NewSession(runner.Options{Stdout: &out, Stderr: &errB})
}

// pendingBlockPrefix is 20 lines of an open Go func — the buffer already on
// screen before the keystrokes being measured are typed.
func pendingBlockPrefix() string {
	var b strings.Builder
	b.WriteString("func report(items []string) error {\n")
	for i := range 19 {
		fmt.Fprintf(&b, "\tstep%d := len(items) + %d\n", i, i)
	}
	return b.String()
}

// keystrokeShapes are the buffers whose typing gets measured. `typed` is
// the text typed one rune at a time; `base` is what already sits in the
// buffer (empty for a fresh line).
var keystrokeShapes = []struct {
	name  string
	base  string
	typed string
}{
	{"short", "", "okcmd -la --color=auto /tmp"},
	{"pathcmd", "", "./scripts/build.sh --verbose"},
	{"goline", "", `msg := strings.Join(parts, ", ")`},
	{"pending", pendingBlockPrefix(), "\tfmt.Println(step0, step1)"},
}

// keystrokePrefixes expands a shape into the successive buffer states a
// user types through: base+typed[:1], base+typed[:2], ...
//
// Rune-wise, not byte-wise — a byte-sliced prefix could split a multi-byte
// rune and hand the classifier invalid UTF-8, which is not a state the
// editor can produce.
func keystrokePrefixes(base, typed string) []string {
	rs := []rune(typed)
	out := make([]string, 0, len(rs))
	for i := 1; i <= len(rs); i++ {
		out = append(out, base+string(rs[:i]))
	}
	return out
}

// BenchmarkKeystrokeHighlight measures the syntax-highlight callback alone:
// one Preview (classify Clone+File over the whole buffer) plus the token
// paint.
func BenchmarkKeystrokeHighlight(b *testing.B) {
	for _, sh := range keystrokeShapes {
		b.Run(sh.name, func(b *testing.B) {
			sess := benchSession(b)
			h := newHighlighter(sess, newCompleter(sess.Idents))
			prefixes := keystrokePrefixes(sh.base, sh.typed)
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				h.highlight([]rune(prefixes[i%len(prefixes)]))
			}
		})
	}
}

// BenchmarkKeystrokeHint measures the hint lane alone. It is the most
// classify-hungry of the three: shellLineAt does a Preview (hint.go:210)
// and breadcrumb does a Pending (hint.go:220), so one hint call is already
// two full passes over the buffer.
func BenchmarkKeystrokeHint(b *testing.B) {
	for _, sh := range keystrokeShapes {
		b.Run(sh.name, func(b *testing.B) {
			h := newHinter(benchSession(b))
			prefixes := keystrokePrefixes(sh.base, sh.typed)
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				line := []rune(prefixes[i%len(prefixes)])
				h.hint(line, len(line)) // cursor at end: typing, not navigating
			}
		})
	}
}

// BenchmarkKeystrokeGhost measures the fish-style suggestion scan. It walks
// history newest-first (suggest.go:51) with no index, so its cost is in the
// history size rather than the buffer — hence the size sweep instead of the
// shape sweep the others use.
func BenchmarkKeystrokeGhost(b *testing.B) {
	for _, units := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("history/%d", units), func(b *testing.B) {
			hist := openHistory("") // empty path: in-memory, no file I/O
			for i := range units {
				hist.Append(fmt.Sprintf("okcmd run --job %d", i))
			}
			// A prefix that matches nothing forces the full scan — the worst
			// case, and the common one while the first few runes are typed.
			s := newSuggester(hist)
			prefixes := keystrokePrefixes("", "zzz-no-such-command")
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				s.suggest(prefixes[i%len(prefixes)])
			}
		})
	}
}

// BenchmarkKeystrokeFrame is the number that matters: everything the
// display engine derives from the buffer for one repaint, in the order it
// runs them. Compare it against a ~16ms frame budget, and against the sum
// of the three benchmarks above — they should add up, and any gap is
// duplicated work worth finding.
func BenchmarkKeystrokeFrame(b *testing.B) {
	for _, sh := range keystrokeShapes {
		b.Run(sh.name, func(b *testing.B) {
			sess := benchSession(b)
			h := newHighlighter(sess, newCompleter(sess.Idents))
			hint := newHinter(sess)
			hist := openHistory("")
			for i := range 500 {
				hist.Append(fmt.Sprintf("okcmd run --job %d", i))
			}
			sug := newSuggester(hist)

			prefixes := keystrokePrefixes(sh.base, sh.typed)
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				src := prefixes[i%len(prefixes)]
				line := []rune(src)
				h.highlight(line)
				hint.hint(line, len(line))
				sug.suggest(src)
			}
		})
	}
}

// BenchmarkKeystrokeMemoHit is the control. It renders ONE fixed buffer, so
// every call after the first hits the memo — the cost of a cursor-only
// refresh (arrow keys), and the floor the benchmarks above are measured
// against. A large gap between this and the frame benchmark is the memo
// earning its keep; a small one would mean the memo is not covering the
// expensive path.
func BenchmarkKeystrokeMemoHit(b *testing.B) {
	sess := benchSession(b)
	h := newHighlighter(sess, newCompleter(sess.Idents))
	hint := newHinter(sess)
	src := pendingBlockPrefix() + "\tfmt.Println(step0)"
	line := []rune(src)

	h.highlight(line) // prime both memos
	hint.hint(line, len(line))

	b.ReportAllocs()
	for b.Loop() {
		h.highlight(line)
		hint.hint(line, len(line))
	}
}
