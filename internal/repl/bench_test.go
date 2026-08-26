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
//     string (highlightSrc, hintSrc, suggest). Re-rendering a fixed buffer
//     would hit the memo every iteration and measure a map lookup. Typing
//     is precisely what defeats those memos, so the benchmark must type.
//
// Every buffer goes through a runeIntern first, because that is what the
// reader does (editor_reef.go's hintProvider) -- one []rune->string
// conversion per frame, shared by all three consumers. Calling the
// rune-taking wrappers instead would measure a conversion per lane that
// the display path does not pay, and would hide the memo-hit floor
// entirely.
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
// pending — typing the 21st line of an already-open 20-line Go block, where
//           every existing line is its own complete logical line. The
//           display engine's several classify passes per frame compound
//           here, but each pass is linear.
// pending-literal — the same height of buffer, but all of it is one
//           unfinished logical line (an open composite literal). This is
//           the shape consumeGo was quadratic on, so it is the shape that
//           says whether that fix reached the interactive path.

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
// screen before the keystrokes being measured are typed. Each line is a
// COMPLETE logical line, so the classifier finishes every one of them in a
// single pass.
func pendingBlockPrefix() string {
	var b strings.Builder
	b.WriteString("func report(items []string) error {\n")
	for i := range 19 {
		fmt.Fprintf(&b, "\tstep%d := len(items) + %d\n", i, i)
	}
	return b.String()
}

// pendingLiteralPrefix is the other kind of open buffer, and the one that
// matters for consumeGo: 20 lines that are all ONE unfinished logical line,
// because the composite literal's braces have not balanced yet. Typing a
// long slice or map literal at the prompt puts a user here.
//
// The distinction is the whole point of having both shapes. A block of
// complete statements costs the same per keystroke however tall it gets;
// an open literal used to cost more with every line added, because the
// classifier re-lexed the whole thing per candidate end line.
func pendingLiteralPrefix() string {
	var b strings.Builder
	b.WriteString("hosts := map[string]int{\n")
	for i := range 19 {
		fmt.Fprintf(&b, "\t\"host%02d\": %d,\n", i, i)
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
	{"pending-literal", pendingLiteralPrefix(), "\t\"host19\": 19,"},
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
			var buf runeIntern
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				h.highlightSrc(buf.str([]rune(prefixes[i%len(prefixes)])))
			}
		})
	}
}

// BenchmarkKeystrokeHint measures the hint lane alone: shellLineAt does a
// Preview (hint.go:210) and breadcrumb does a Pending (hint.go:220).
//
// In isolation this understates what the classifier cache does for the
// hint lane, because the two calls share their result with the
// highlighter's Preview rather than only with each other — and on a buffer
// where signature help fires, cursorHint returns before shellLineAt runs at
// all. BenchmarkKeystrokeFrame is where that sharing shows up.
func BenchmarkKeystrokeHint(b *testing.B) {
	for _, sh := range keystrokeShapes {
		b.Run(sh.name, func(b *testing.B) {
			h := newHinter(benchSession(b))
			prefixes := keystrokePrefixes(sh.base, sh.typed)
			var buf runeIntern
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				line := []rune(prefixes[i%len(prefixes)])
				// Cursor at end: typing, not navigating.
				h.hintSrc(buf.str(line), len(line))
			}
		})
	}
}

// BenchmarkKeystrokeGhost measures the fish-style suggestion scan. Its cost
// is in the history size rather than the buffer, so it sweeps history sizes
// instead of the buffer shapes the others use.
//
// Two prefixes per size, because the index (suggest.go) makes them different
// problems where the original linear scan made them the same one:
//
//	miss — matches nothing. Every keystroke that is not retracing a
//	       remembered command lands here, and it is the case the binary
//	       search answers without touching the store.
//	hit  — matches EVERY unit, which is the index's worst case: the prefix
//	       run it has to walk for the newest entry is the whole store. Typing
//	       the first rune of a command you run constantly is this shape.
func BenchmarkKeystrokeGhost(b *testing.B) {
	for _, units := range []int{100, 1000, 10000} {
		for _, pfx := range []struct{ name, typed string }{
			{"miss", "zzz-no-such-command"},
			{"hit", "okcmd run --job"},
		} {
			b.Run(fmt.Sprintf("history/%d/%s", units, pfx.name), func(b *testing.B) {
				hist := openHistory("") // empty path: in-memory, no file I/O
				for i := range units {
					hist.Append(fmt.Sprintf("okcmd run --job %d", i))
				}
				s := newSuggester(hist)
				prefixes := keystrokePrefixes("", pfx.typed)
				s.suggest(prefixes[0]) // absorb the history outside the timer
				b.ReportAllocs()
				for i := 0; b.Loop(); i++ {
					s.suggest(prefixes[i%len(prefixes)])
				}
			})
		}
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
			var buf runeIntern
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				line := []rune(prefixes[i%len(prefixes)])
				// The order the display engine runs them in, over one
				// shared conversion of the buffer -- see hintProvider.
				src := buf.str(line)
				sug.suggest(src)
				hint.hintSrc(src, len(line))
				h.highlightSrc(src)
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
//
// This is also where the frame intern shows up. The floor used to be a
// conversion of the whole buffer per consumer plus a memcmp per memo —
// 2.6us and 1.2KB on this 20-line buffer, for an answer already in hand.
// With one shared conversion the runes are compared once and the memo
// compares are pointer-equal, so an unchanged buffer allocates nothing.
func BenchmarkKeystrokeMemoHit(b *testing.B) {
	sess := benchSession(b)
	h := newHighlighter(sess, newCompleter(sess.Idents))
	hint := newHinter(sess)
	line := []rune(pendingBlockPrefix() + "\tfmt.Println(step0)")
	var buf runeIntern

	h.highlightSrc(buf.str(line)) // prime both memos
	hint.hintSrc(buf.str(line), len(line))

	b.ReportAllocs()
	for b.Loop() {
		src := buf.str(line)
		hint.hintSrc(src, len(line))
		h.highlightSrc(src)
	}
}

// BenchmarkGhostAbsorb measures indexing itself, which is the cost the index
// added to a path that previously had none. Two shapes, and they are paid at
// different moments:
//
//	load   — the whole history file at the first prompt's first keystroke.
//	         Once per session.
//	append — one accepted command merged into an already-built index. This
//	         one lands on a keystroke, so it is the one with a frame budget:
//	         the merge rebuilds the slices, so it is linear in the history
//	         even though only one unit arrived.
func BenchmarkGhostAbsorb(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		units := make([]string, 0, n+1)
		for i := range n + 1 {
			units = append(units, fmt.Sprintf("okcmd run --job %d", i))
		}
		b.Run(fmt.Sprintf("load/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var g ghostIndex
				g.absorb(units[:n], 0)
			}
		})
		b.Run(fmt.Sprintf("append/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				var g ghostIndex
				g.absorb(units[:n], 0)
				b.StartTimer()
				g.absorb(units, n)
			}
		})
	}
}
