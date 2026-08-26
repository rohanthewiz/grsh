package classify

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/stdlibreg"
)

// Benchmarks for the classifier — the hot path of the interactive editor.
//
// Every keystroke on a pending unit re-runs this package several times over
// the WHOLE buffer (highlighter Preview, hinter Preview + Pending, the
// accept-line Pending). So the question these benchmarks have to answer is
// not "how fast is one classify" but "how does one classify scale with the
// buffer the user is sitting in" — a cost that is merely large is fine at
// 60fps; a cost that is quadratic in buffer size is what makes a paste feel
// like the shell hung.
//
// That is why the size-parameterized benchmarks below report a ns/line
// metric alongside ns/op. ns/op rising with n proves nothing on its own
// (more input, more work). ns/line rising with n is the signature of
// super-linear behavior, and it reads straight off the benchmark output
// without anyone having to divide two numbers in their head.

// benchSizes spans the range that matters interactively: a few lines is a
// hand-typed block, 512 is a paste. Powers of four keep the output short
// while still making a quadratic trend unmistakable (each step should
// roughly quadruple ns/line if the cost is O(n^2)).
var benchSizes = []int{8, 32, 128, 512}

// newBenchClassifier mirrors runner.NewSession's construction (session.go:96)
// so the package set and predeclared scope match what the REPL actually
// classifies against — the scope chain is consulted per identifier, so a
// toy classifier would understate the real cost.
func newBenchClassifier() *Classifier {
	c := New(stdlibreg.Names())
	c.Predeclare("len", "cap", "append", "delete", "copy", "make", "min", "max", "iff")
	return c
}

// goCompositeSrc builds ONE Go logical line spanning n+2 physical lines: a
// composite literal whose braces stay unbalanced until the last line.
//
//	xs := []int{
//	    0,
//	    1,
//	    ...
//	}
//
// This is the shape consumeGo used to be quadratic on: it cannot know the
// line is complete until the closing brace, so the pre-optimization version
// re-joined and re-lexed the entire accumulated fragment once per physical
// line.
func goCompositeSrc(n int) string {
	var b strings.Builder
	b.WriteString("xs := []int{\n")
	for i := range n {
		fmt.Fprintf(&b, "\t%d,\n", i)
	}
	b.WriteString("}")
	return b.String()
}

// goBlockSrc builds n statements inside a func — n SEPARATE logical lines,
// each complete on its own. The contrast with goCompositeSrc isolates the
// quadratic from a plain "more input costs more" curve: same line count,
// but every line here completes on its own. It is also the shape that
// catches the tempting wrong fix — joining lines[i:] per logical line would
// leave the composite case linear while making THIS one quadratic.
func goBlockSrc(n int) string {
	var b strings.Builder
	b.WriteString("func bench() {\n")
	for i := range n {
		fmt.Fprintf(&b, "\tv%d := %d + %d\n", i, i, i*2)
	}
	b.WriteString("}")
	return b.String()
}

// shellSrc builds n independent shell lines — the everyday case, and the
// baseline every Go-side number should be read against.
func shellSrc(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "echo line %d | grep -c %d\n", i, i)
	}
	return strings.TrimRight(b.String(), "\n")
}

// heredocSrc builds a heredoc with an n-line body. joinShell accumulates
// that body with `text += "\n" + lines[i]` (classify.go:298), reallocating
// the whole chunk per line.
func heredocSrc(n int) string {
	var b strings.Builder
	b.WriteString("cat <<EOF\n")
	for i := range n {
		fmt.Fprintf(&b, "body line %d with some filler text\n", i)
	}
	b.WriteString("EOF")
	return b.String()
}

// reportPerLine emits the ns/line metric described in the file comment.
// b.Elapsed() covers only the timed region, so this stays correct even
// when a benchmark stops the timer to rebuild state.
func reportPerLine(b *testing.B, lines int) {
	b.Helper()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*lines), "ns/line")
}

// BenchmarkConsumeGo isolates consumeGo on one unbalanced logical line,
// with no classify or scope work around it. This is where the O(n^2) was
// found, and the ns/line column is now the regression guard: it must stay
// flat across the sweep. If it starts rising with n again, the incremental
// lexer has been broken back into a re-lex-per-line loop.
//
// The goSrc is rebuilt inside the timed region on purpose. Indexing the
// source is per-File work, and a future change could easily make it
// per-logical-line instead; that mistake would show up here as exactly the
// curve this benchmark exists to catch.
func BenchmarkConsumeGo(b *testing.B) {
	for _, n := range benchSizes {
		src := goCompositeSrc(n)
		lines := strings.Split(src, "\n")
		b.Run(fmt.Sprintf("composite/%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := newGoSrc(src).consumeGo(lines, 0); err != nil {
					b.Fatal(err)
				}
			}
			reportPerLine(b, len(lines))
		})
	}
}

// BenchmarkFile is the end-to-end classify pass, one call per whole buffer.
// The three shapes separate "cost of more input" (shell, block) from "cost
// of one long logical line" (composite).
func BenchmarkFile(b *testing.B) {
	shapes := []struct {
		name string
		src  func(int) string
	}{
		{"shell", shellSrc},
		{"go-block", goBlockSrc},
		{"go-composite", goCompositeSrc},
		{"heredoc", heredocSrc},
	}
	for _, sh := range shapes {
		for _, n := range benchSizes {
			src := sh.src(n)
			lines := strings.Count(src, "\n") + 1
			b.Run(fmt.Sprintf("%s/%d", sh.name, n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					// A fresh clone per iteration: File mutates the
					// classifier (scope, brace depth), so reusing one would
					// measure a classifier that drifts further from the
					// REPL's state on every iteration.
					if _, err := newBenchClassifier().File(src); err != nil {
						// Incomplete input is expected for the pending
						// shapes; only a real failure should stop the run.
						if !strings.Contains(err.Error(), "incomplete") {
							b.Fatal(err)
						}
					}
				}
				reportPerLine(b, lines)
			})
		}
	}
}

// BenchmarkPending measures what the editor calls on Enter and on every
// electric `}` — including the Clone, which copies the whole scope chain.
func BenchmarkPending(b *testing.B) {
	c := newBenchClassifier()
	for _, n := range benchSizes {
		src := goCompositeSrc(n) // still open: the pending state the REPL sits in
		lines := strings.Count(src, "\n") + 1
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				c.Pending(src)
			}
			reportPerLine(b, lines)
		})
	}
}

// BenchmarkPreview measures the highlighter's per-frame classify. It runs
// the same shape as BenchmarkPending on purpose: both are Clone+File, so
// the two should track each other closely, and a divergence would mean one
// is doing work the other is not. (The flat, linear go-block shape is
// already covered by BenchmarkFile.)
func BenchmarkPreview(b *testing.B) {
	c := newBenchClassifier()
	for _, n := range benchSizes {
		src := goCompositeSrc(n)
		lines := strings.Count(src, "\n") + 1
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				c.Preview(src)
			}
			reportPerLine(b, lines)
		})
	}
}

// BenchmarkClone isolates the Clone half of Pending/Preview. Every
// speculative classify deep-copies the scope chain's name maps
// (repl.go:22), and the REPL runs several per keystroke — so if this is a
// meaningful fraction of Pending, memoizing the clone is worth more than
// optimizing the lexer.
func BenchmarkClone(b *testing.B) {
	// A scope loaded the way a working session's is: registry packages plus
	// a realistic number of user-declared identifiers.
	c := newBenchClassifier()
	for i := range 100 {
		c.Predeclare(fmt.Sprintf("userVar%d", i))
	}
	b.ReportAllocs()
	for b.Loop() {
		c.Clone()
	}
}
