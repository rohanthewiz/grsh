package runner

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// Benchmarks for the evaluation path: the fixed cost of running one REPL
// unit, and the per-iteration cost of the tree-walking interpreter.
//
// # Separating fixed cost from per-iteration cost
//
// Every RunSource call pays a constant toll — classify, transform,
// go/parser — before a single statement is evaluated. Benchmarking one
// fixed loop size therefore measures the toll and the loop mixed together,
// and an interpreter optimization would show up diluted by however much
// parsing happened to cost.
//
// So the loop benchmarks sweep the trip count and report a ns/iter metric.
// The toll is constant across the sweep, so ns/iter converges to the true
// per-iteration interpreter cost as n grows; the residual at small n is the
// toll. Read the sweep, not any single row.
//
// allocs/op is the headline number for the loop benchmarks, not ns/op.
// NewEnv (env.go:15) allocates its map eagerly, evalFor wraps the body in a
// scope per iteration (interp.go:325), and the *ast.BlockStmt handler wraps
// it AGAIN (interp.go:232) — so the prediction under test is ~2 map
// allocations per iteration, and allocs/op divided by n is the direct
// reading of it. That is what the ReportMetric line below prints.

// benchLoopSizes: 10 is a hand-written loop, 10000 is a script doing real
// work. The spread is what makes the fixed toll separable.
var benchLoopSizes = []int{10, 100, 1000, 10000}

// benchSession returns a session with output discarded. Stderr matters too
// — a benchmark that silently errors every iteration would otherwise
// measure the error path and look impressively fast.
func benchSession(b *testing.B) *Session {
	b.Helper()
	return NewSession(Options{Stdout: io.Discard, Stderr: io.Discard})
}

// mustRun fails the benchmark on error. Called outside the timed region for
// setup, and inside it for the work under test — an unnoticed error means
// the benchmark is timing nothing.
func mustRun(b *testing.B, s *Session, src string) {
	b.Helper()
	if err := s.RunSource("bench", src); err != nil {
		b.Fatalf("%s: %v", strings.SplitN(src, "\n", 2)[0], err)
	}
}

// reportPerIter prints the two derived metrics the loop benchmarks are
// actually about: time and allocations per loop iteration, rather than per
// RunSource call.
func reportPerIter(b *testing.B, iters int) {
	b.Helper()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*iters), "ns/iter")
}

// BenchmarkForLoopInt is the classic three-clause loop — the shape whose
// body env is allocated twice per iteration.
func BenchmarkForLoopInt(b *testing.B) {
	for _, n := range benchLoopSizes {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			s := benchSession(b)
			// Declared once, called per op: the func body gives the loop a
			// scope to live in, so repeated calls neither redeclare at top
			// level nor accumulate state across iterations.
			mustRun(b, s, "func benchFor(n int) int {\n"+
				"\tsum := 0\n"+
				"\tfor i := 0; i < n; i++ { sum = sum + i }\n"+
				"\treturn sum\n}")
			call := fmt.Sprintf("benchFor(%d)", n)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				mustRun(b, s, call)
			}
			reportPerIter(b, n)
		})
	}
}

// BenchmarkRangeLoop covers the range form, which wraps in evalRange
// (interp.go:352) and then again in the block handler — the same double
// wrap by a different route, plus the per-iteration key/value binds.
func BenchmarkRangeLoop(b *testing.B) {
	for _, n := range benchLoopSizes {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			s := benchSession(b)
			mustRun(b, s, "func benchRange(xs []int) int {\n"+
				"\tsum := 0\n"+
				"\tfor _, v := range xs { sum = sum + v }\n"+
				"\treturn sum\n}")
			mustRun(b, s, fmt.Sprintf("benchXs := make([]int, %d)", n))

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				mustRun(b, s, "benchRange(benchXs)")
			}
			reportPerIter(b, n)
		})
	}
}

// BenchmarkNestedBlocks measures whether the env wrapping compounds with
// nesting: an `if` inside the loop body is another scope, itself wrapped
// twice (interp.go:225 plus the block handler). Compare allocs/iter against
// BenchmarkForLoopInt at the same n — the delta is the cost of one nested
// block, and it should be the thing a lazy-map or evalBlockIn change
// erases.
func BenchmarkNestedBlocks(b *testing.B) {
	for _, n := range benchLoopSizes {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			s := benchSession(b)
			mustRun(b, s, "func benchNested(n int) int {\n"+
				"\tsum := 0\n"+
				"\tfor i := 0; i < n; i++ {\n"+
				"\t\tif i > 0 { sum = sum + i }\n"+
				"\t}\n"+
				"\treturn sum\n}")
			call := fmt.Sprintf("benchNested(%d)", n)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				mustRun(b, s, call)
			}
			reportPerIter(b, n)
		})
	}
}

// BenchmarkEvalTrivial is the fixed per-unit toll with the interpreter work
// reduced to almost nothing: what the REPL pays to run `x := 1`. It is the
// floor under every interactive Enter, and the baseline the loop sweeps'
// small-n rows should be read against.
func BenchmarkEvalTrivial(b *testing.B) {
	units := []struct{ name, src string }{
		{"go-assign", "benchX := 1"},
		{"go-call", "benchNoop(1)"},
		{"comment", "// nothing at all"},
	}
	for _, u := range units {
		b.Run(u.name, func(b *testing.B) {
			s := benchSession(b)
			mustRun(b, s, "func benchNoop(n int) int { return n }")
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				mustRun(b, s, u.src)
			}
		})
	}
}

// BenchmarkInterpolationLoop covers `{expr}` word interpolation. The AST
// cache landed already (call.go:114), so this is not a bug hunt — it pins
// the cached cost so a later change to the fset/position handling cannot
// quietly reintroduce a parse per expansion without the number moving.
//
// `export` is a shell builtin (shellexec/builtins.go:41), so this exercises
// the interpolation path without forking a process — a fork would swamp the
// measurement and make it depend on the host's process-spawn cost.
func BenchmarkInterpolationLoop(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			s := benchSession(b)
			mustRun(b, s, "func benchInterp(n int) {\n"+
				"\tfor i := 0; i < n; i++ {\n"+
				"\t\texport BENCHVAR={i * 2}\n"+
				"\t}\n}")
			call := fmt.Sprintf("benchInterp(%d)", n)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				mustRun(b, s, call)
			}
			reportPerIter(b, n)
		})
	}
}

// BenchmarkREPLUnit models what the REPL actually does per Enter
// (repl.go:160 then :168): Pending to decide whether the unit is complete,
// then RunSource — which independently clones and classifies the SAME
// source again (session.go:241).
//
// The pair-vs-eval gap is the size of that duplication, measured rather
// than asserted. It is the number that says whether memoizing the last
// (src -> classify result) is worth doing, and on a multi-line unit it
// carries the quadratic consumeGo cost twice.
func BenchmarkREPLUnit(b *testing.B) {
	// A multi-line unit, since that is where the duplication hurts: the
	// composite literal is one logical line, so classifying it is quadratic
	// in its line count.
	var block strings.Builder
	block.WriteString("benchList := []int{\n")
	for i := range 40 {
		fmt.Fprintf(&block, "\t%d,\n", i)
	}
	block.WriteString("}")

	units := []struct{ name, src string }{
		{"one-line", "benchY := 1"},
		{"multi-line", block.String()},
	}
	for _, u := range units {
		// "eval" is RunSource alone; "pending+eval" adds the classify pass
		// the REPL does first. Same source, so the difference is purely the
		// duplicated work.
		b.Run(u.name+"/eval", func(b *testing.B) {
			s := benchSession(b)
			b.ReportAllocs()
			for b.Loop() {
				mustRun(b, s, u.src)
			}
		})
		b.Run(u.name+"/pending+eval", func(b *testing.B) {
			s := benchSession(b)
			b.ReportAllocs()
			for b.Loop() {
				s.Pending(u.src)
				mustRun(b, s, u.src)
			}
		})
	}
}
