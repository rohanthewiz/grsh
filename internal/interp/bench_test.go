package interp

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"testing"

	"github.com/rohanthewiz/grsh/internal/shellexec"
)

// ---- benchmarks and the allocation-shape guard ----
//
// The evaluator's cost is dominated by scope construction: every
// construct that introduces a scope allocates an *Env AND the map inside
// it, and several allocate more than once for the same scope (evalFor
// builds one per iteration, then the *ast.BlockStmt handler builds
// another for the same body). These benchmarks price that, and the guard
// below pins the per-iteration allocation COUNT so a change to the Env
// layout shows up as a number rather than as a hunch.
//
// A note for whoever writes the next one: Run is not memoized -- it
// re-executes the whole body every call -- but it DOES mutate globals, so
// a benchmark whose script accumulates into a global slice grows its own
// working set across iterations and measures the growth rather than the
// evaluation. Keep bench scripts idempotent.

// benchIters is the trip count the benchmarks use. The guard uses two
// different counts; see perIterationAllocs.
const benchIters = 1000

// prepScript parses a bench/guard script once, outside the measured
// region, and returns everything Run needs. Output is discarded: these
// scripts print nothing, and a real writer would price the io instead.
func prepScript(tb testing.TB, body string) (*Interp, *token.FileSet, *ast.File) {
	tb.Helper()
	st := shellexec.NewState()
	stdio := shellexec.Stdio{In: bytes.NewReader(nil), Out: io.Discard, Err: io.Discard}
	in := New(st, stdio, nil)

	src := "package main\n\nfunc __main() {\n" + body + "\n}\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "b.grsh", src, parser.SkipObjectResolution)
	if err != nil {
		tb.Fatalf("bench script is not valid Go: %v\n%s", err, src)
	}
	return in, fset, f
}

// loopShapes are the scripts the guard and the benchmarks share. body
// takes a trip count so the guard can difference two sizes.
var loopShapes = []struct {
	name string
	body func(n int) string

	// allocs is the expected allocation count for ONE loop iteration.
	// It is a whole number by construction -- see perIterationAllocs --
	// so the tolerance below only absorbs measurement noise, not fixed
	// setup cost.
	allocs float64
}{
	{
		// The floor case: one scope per iteration, opened by the
		// BlockStmt handler, plus the arithmetic's boxing. It costs a
		// single allocation because nothing is declared into it and the
		// map is deferred.
		name:   "plain",
		body:   func(n int) string { return fmt.Sprintf("s := 0\nfor i := 0; i < %d; i++ {\n\ts = s + i\n}", n) },
		allocs: 11,
	},
	{
		// A nested if now costs only its own body scope. Before the
		// scopes were collapsed it added three -- an init scope with no
		// init, a wrap around the body, and the body block itself -- and
		// this shape was the one that showed it, at 23 against plain's 14.
		name: "nested-if",
		body: func(n int) string {
			return fmt.Sprintf("s := 0\nfor i := 0; i < %d; i++ {\n\tif i > 0 {\n\t\ts = s + i\n\t}\n}", n)
		},
		allocs: 15,
	},
	{
		// range keeps its per-iteration scope: `:= v` declares into it,
		// and that scope is what gives each closure its own v. So this
		// shape moved least -- one allocation, from the body block's
		// deferred map.
		name: "range",
		body: func(n int) string {
			return fmt.Sprintf("xs := make([]int, %d)\ns := 0\nfor _, v := range xs {\n\ts = s + v\n}", n)
		},
		allocs: 10,
	},
	{
		// A closure call adds a call scope for the parameters, a frame,
		// and the argument slice on top of the loop's own scopes. The
		// parameter scope is a real one -- it holds a binding -- so what
		// came off here is the loop's redundant wrap and two deferred
		// maps.
		name: "closure-call",
		body: func(n int) string {
			return fmt.Sprintf("f := func(a int) int { return a + 1 }\ns := 0\nfor i := 0; i < %d; i++ {\n\ts = f(i)\n}", n)
		},
		allocs: 20,
	},
}

// allocTolerance absorbs run-to-run noise only. The measured quantity is
// a difference of two whole-loop measurements, so a correct reading lands
// on an integer; anything beyond half an allocation is a real change.
const allocTolerance = 0.5

// perIterationAllocs measures the MARGINAL allocation cost of one loop
// iteration by running the same shape at two trip counts and taking the
// difference. Everything that does not scale with the loop -- parsing,
// hoisting, the frame, the script's own setup statements -- appears in
// both measurements and cancels, which is what makes the result a clean
// integer that a tight bound can be written against.
func perIterationAllocs(t *testing.T, body func(n int) string) float64 {
	t.Helper()
	//
	// Both trip counts sit ABOVE 255 for the same reason: the interpreter
	// boxes every int into a Value, and Go's runtime serves 0..255 from a
	// shared table without allocating. A low trip count would spend most
	// of its iterations in that free range, so the difference would report
	// the boxing cliff rather than the scope cost. With lo already past
	// the cliff, the 256 cheap iterations appear in both runs and cancel.
	const (
		lo = 1000
		hi = 2000
	)
	measure := func(n int) float64 {
		in, fset, f := prepScript(t, body(n))
		// A warm run first: the first pass through a body grows the frame
		// slice and touches go/ast internals, and that one-off cost does
		// NOT cancel in the difference (it scales with nothing).
		if err := in.Run(fset, f); err != nil {
			t.Fatalf("run: %v", err)
		}
		return testing.AllocsPerRun(10, func() {
			if err := in.Run(fset, f); err != nil {
				t.Fatalf("run: %v", err)
			}
		})
	}
	return (measure(hi) - measure(lo)) / (hi - lo)
}

// TestLoopAllocationShape pins per-iteration allocation counts.
//
// The bound is TWO-SIDED on purpose, which is unusual and worth
// explaining. The upper side is the ordinary regression guard. The lower
// side is there so an improvement cannot land silently: it forces the new
// baseline to be written down in the commit that earns it, next to the
// cases in scope_test.go that say the semantics did not move with it.
//
// It has already done that job once. The counts below were 14 / 23 / 11 /
// 24 before the redundant Env wraps came out of evalFor, evalRange, the
// if handler and evalSwitch, and before Env deferred its map to the first
// Define. All four floors tripped; scope_test.go passed unchanged.
//
// These counts are marginal, and are higher than a naive
// allocs-per-run divided by the trip count reports: the first 256
// iterations box their ints for free (see perIterationAllocs), which
// drags the average down by about one allocation per iteration on a
// thousand-iteration script. The marginal figure is the one an
// optimization actually moves.
//
// So: a failure here is not necessarily a bug. Read which side broke.
func TestLoopAllocationShape(t *testing.T) {
	for _, shape := range loopShapes {
		t.Run(shape.name, func(t *testing.T) {
			got := perIterationAllocs(t, shape.body)
			if math.Abs(got-shape.allocs) <= allocTolerance {
				return
			}
			if got > shape.allocs {
				t.Errorf("allocations per iteration rose to %.2f from %.0f -- a regression, "+
					"or a scope was added", got, shape.allocs)
				return
			}
			t.Errorf("allocations per iteration fell to %.2f from %.0f -- if that was the intent, "+
				"re-baseline the count here and confirm scope_test.go still passes unchanged",
				got, shape.allocs)
		})
	}
}

// BenchmarkLoop prices each loop shape end to end; ns/iter is the
// per-iteration figure the Env work moves.
func BenchmarkLoop(b *testing.B) {
	for _, shape := range loopShapes {
		b.Run(shape.name, func(b *testing.B) {
			in, fset, f := prepScript(b, shape.body(benchIters))
			b.ReportAllocs()
			for b.Loop() {
				if err := in.Run(fset, f); err != nil {
					b.Fatalf("run: %v", err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*benchIters), "ns/iter")
		})
	}
}

// BenchmarkNewEnv isolates the cost the loop shapes pay repeatedly: an
// Env plus its eagerly allocated map. If NewEnv ever grows a lazy map,
// this is where the saving shows up first.
func BenchmarkNewEnv(b *testing.B) {
	parent := NewEnv(nil)
	b.ReportAllocs()
	for b.Loop() {
		envSink = NewEnv(parent)
	}
}

// BenchmarkEnvDefine prices a scope that is actually used -- the case a
// lazy map would NOT make cheaper, and the reason the two are benchmarked
// separately.
func BenchmarkEnvDefine(b *testing.B) {
	parent := NewEnv(nil)
	b.ReportAllocs()
	for b.Loop() {
		e := NewEnv(parent)
		e.Define("x", 1)
		envSink = e
	}
}

// BenchmarkEnvLookupDepth prices the outward walk. Scope nesting is deep
// in real scripts -- a loop body inside an if inside a function is five
// or six links -- so the cost of reaching a global is paid on every
// identifier reference.
func BenchmarkEnvLookupDepth(b *testing.B) {
	for _, depth := range []int{1, 4, 16} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			root := NewEnv(nil)
			root.Define("target", 1)
			e := root
			for i := 0; i < depth; i++ {
				e = NewEnv(e)
			}
			b.ReportAllocs()
			for b.Loop() {
				valSink, _ = e.Get("target")
			}
		})
	}
}

// Sinks keep the benchmarked results live so the stores are not
// eliminated.
var (
	envSink *Env
	valSink Value
)
