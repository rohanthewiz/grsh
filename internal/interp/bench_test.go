package interp

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"reflect"
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
		//
		// The literal sits OUTSIDE the loop, which is why this shape did
		// not move when the clause variable became per-iteration: the
		// capture scan looks inside the ForStmt and finds nothing there.
		// The shape below is the one that pays.
		name: "closure-call",
		body: func(n int) string {
			return fmt.Sprintf("f := func(a int) int { return a + 1 }\ns := 0\nfor i := 0; i < %d; i++ {\n\ts = f(i)\n}", n)
		},
		allocs: 20,
	},
	{
		// The priced case for Go 1.22 clause variables: a func literal
		// INSIDE the loop means a closure could outlive the iteration, so
		// each iteration gets an Env of its own with the clause variable
		// copied in and back out.
		//
		// That is the cost of the semantics, and it is charged only to the
		// loops that could observe them -- which is the whole reason the
		// four shapes above are unmoved.
		name: "closure-in-body",
		body: func(n int) string {
			return fmt.Sprintf("s := 0\nfor i := 0; i < %d; i++ {\n\tf := func() int { return i + 1 }\n\ts = f()\n}", n)
		},
		allocs: 23,
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

// BenchmarkStructCopy prices the shape copyOnStore is EXPENSIVE at, as
// the loop shapes price the one it is free at.
//
// Value semantics are not free: every binding, parameter and slot that
// receives a struct now allocates a fresh Vals slice, and a struct-typed
// field allocates again one level down. The three shapes separate what
// that costs:
//
//	flat    one field, so one copy per store -- the floor
//	nested  a struct field, so the copy descends: two per store
//	scalar  the same loop over an int, as the baseline for what the
//	        loop itself costs before any copy
//
// The difference between scalar and flat is the price of correctness
// here. It is paid only by scripts that actually pass structs around,
// which is why the loop shapes above did not move at all.
func BenchmarkStructCopy(b *testing.B) {
	shapes := []struct{ name, body string }{
		{"scalar", `x := 0
for i := 0; i < %d; i++ {
	y := x
	x = y
}
_ = x`},
		{"flat", `type P struct {
	X int
}
p := P{1}
for i := 0; i < %d; i++ {
	q := p
	p = q
}
_ = p`},
		{"nested", `type In struct {
	X int
}
type Out struct {
	I In
}
p := Out{In{1}}
for i := 0; i < %d; i++ {
	q := p
	p = q
}
_ = p`},
	}
	for _, s := range shapes {
		b.Run(s.name, func(b *testing.B) {
			in, fset, f := prepScript(b, fmt.Sprintf(s.body, benchIters))
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

// BenchmarkStructZero prices what resolving a struct-TYPED field costs at
// CONSTRUCTION, which is the one place this change adds work.
//
// Before script struct types resolved in type position, `In Inner` had no
// resolvable type, so the field's zero was nil and a literal paid nothing
// for it. The zero is a real Inner now, and t.Zero is shared by the type,
// so newZero has to duplicate it -- every Outer literal allocates one
// level down, whether or not the literal then overwrites the field.
//
// That is Go's own order (zero the struct, then assign the fields), and
// it is what makes `var o Outer` have a usable o.In at all. The flat
// shape is the control: a struct of scalars still takes the bare copy.
func BenchmarkStructZero(b *testing.B) {
	shapes := []struct{ name, body string }{
		{"flat", `type P struct {
	X int
}
p := P{0}
for i := 0; i < %d; i++ {
	p = P{i}
}
_ = p`},
		{"nested", `type In struct {
	X int
}
type Out struct {
	I In
}
p := Out{In{0}}
for i := 0; i < %d; i++ {
	p = Out{In{i}}
}
_ = p`},
	}
	for _, s := range shapes {
		b.Run(s.name, func(b *testing.B) {
			in, fset, f := prepScript(b, fmt.Sprintf(s.body, benchIters))
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

// BenchmarkStructContainer prices the boundary the minted storage types
// added: every element read out of a slice or map of script structs goes
// through fromStore, which is a Kind check plus -- only for struct-kind
// values -- one lock-free map lookup.
//
// The native shapes are the control. They pay the Kind check and nothing
// else, so a gap between them and the struct shapes is the boundary's
// whole cost; a gap that moves is the map lookup getting expensive.
//
// The KEY shapes are priced against the interpreter before keys were
// minted, not against a native control, because their cost is a different
// one: a key is ENCODED into a comparable array on every crossing rather
// than wrapped like a pointer, so it can never match the string-keyed
// map above.
//
// Minting keys first cost +20% on a read, +21% on a write and +8% on a
// range, all at an unchanged allocation count. Encoding through a Go
// array literal instead of reflect, aliasing the wrap instead of filling
// it field by field, and decoding a map's keys once for both the
// ordering and the loop put all three back where they were -- +2%, +2%
// and -1.5%, against a control band of the same width -- and one
// allocation cheaper per read or write, 1755 cheaper over the range
// shape. BenchmarkKeyCrossing prices those three pieces directly; this
// measures what a script actually feels.
//
// Raising keyArrFanout from 4 to 8 afterwards moved one shape and only
// one: map-key-struct-hit-6 by -19.7% and one allocation fewer per
// crossing, with every other shape here inside the control band. That is
// the whole visible effect of widening the fast path, and the reason the
// wide shape is benchmarked beside the narrow ones rather than instead
// of them -- the narrow shapes are what pays for the extra cases, so a
// tax that showed up would show up in THEM.
//
// Rebuilding the general path -- the one a key WIDER than the fanout
// still takes -- then moved map-key-struct-hit-10 by -23.8% and one
// allocation per crossing, 499KB of a thousand crossings down to 339KB,
// with every other shape here inside +/-0.8%. The two wide shapes are
// there to keep those two changes separable: 6 is on the literal path
// and 10 is on the general one, so a change to either shows in exactly
// one of them.
//
// Both figures are the MINIMUM of a dozen interleaved runs with the
// binaries rotated. Machine noise on these shapes is wider than several
// of the numbers above, and a baseline taken in another session is not a
// baseline -- the native controls are here to say how wide it was.
func BenchmarkStructContainer(b *testing.B) {
	shapes := []struct{ name, body string }{
		{"slice-index-native", `xs := []int{0, 0, 0, 0}
n := 0
for i := 0; i < %d; i++ {
	n = xs[i%%4]
}
_ = n`},
		{"slice-index-struct", `type P struct {
	X int
}
xs := []P{{1}, {2}, {3}, {4}}
n := 0
for i := 0; i < %d; i++ {
	n = xs[i%%4].X
}
_ = n`},
		{"map-hit-native", `m := map[string]int{"k": 1}
n := 0
for i := 0; i < %d; i++ {
	n = m["k"]
}
_ = n`},
		{"map-hit-struct", `type P struct {
	X int
}
m := map[string]P{"k": {1}}
n := 0
for i := 0; i < %d; i++ {
	n = m["k"].X
}
_ = n`},
		// The miss is the case that changed BEHAVIOUR, not just storage:
		// it used to hand back nil and now builds a zero struct, so it
		// allocates where it did not. Priced so the cost is on the record.
		{"map-miss-struct", `type P struct {
	X int
}
m := map[string]P{"k": {1}}
n := 0
for i := 0; i < %d; i++ {
	n = m["zz"].X
}
_ = n`},
		{"range-slice-struct", `type P struct {
	X int
}
xs := []P{{1}, {2}, {3}, {4}}
n := 0
for i := 0; i < %d/4; i++ {
	for _, p := range xs {
		n = p.X
	}
}
_ = n`},
		// The KEY boundary, which is a different and dearer one: every
		// crossing ENCODES the struct into a comparable array rather than
		// wrapping a pointer, so this is priced against the string-keyed
		// map above rather than expected to match it.
		{"map-key-struct-hit", `type P struct {
	X int
}
m := map[P]int{{1}: 1}
k := P{1}
n := 0
for i := 0; i < %d; i++ {
	n = m[k]
}
_ = n`},
		{"map-key-struct-write", `type P struct {
	X int
}
m := map[P]int{}
k := P{1}
for i := 0; i < %d; i++ {
	m[k] = i
}
_ = m`},
		// A key WIDE enough to have missed the fast path before the
		// fanout was measured, kept beside the one-field shapes so the
		// two halves of that trade are priced together: this shape is
		// what the extra cases bought, and map-key-struct-hit above is
		// what every key pays for them.
		{"map-key-struct-hit-6", `type P struct {
	A int
	B int
	C int
	D int
	E int
	F int
}
m := map[P]int{{1, 2, 3, 4, 5, 6}: 1}
k := P{1, 2, 3, 4, 5, 6}
n := 0
for i := 0; i < %d; i++ {
	n = m[k]
}
_ = n`},
		// Wider than keyArrFanout, so this is the only shape here that
		// crosses on the general path: reflect allocates the array, a
		// slice aliasing it takes the fields, and the box aliases it
		// again rather than copying.
		{"map-key-struct-hit-10", `type P struct {
	A int
	B int
	C int
	D int
	E int
	F int
	G int
	H int
	I int
	J int
}
m := map[P]int{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}: 1}
k := P{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
n := 0
for i := 0; i < %d; i++ {
	n = m[k]
}
_ = n`},
		{"range-map-key-struct", `type P struct {
	X int
}
m := map[P]int{{1}: 1, {2}: 2, {3}: 3, {4}: 4}
n := 0
for i := 0; i < %d/4; i++ {
	for k := range m {
		n = k.X
	}
}
_ = n`},
		// The DECODE counterpart to map-key-struct-hit-10, and the only
		// shape in this file whose cost is dominated by reading a key
		// back rather than by building one. Decoding is per-field work
		// and the one-field shape above cannot show that: a change to
		// the per-field cost moves this by several times what it moves
		// its narrow neighbour, which is the difference between a slope
		// and a number.
		{"range-map-key-struct-10", `type P struct {
	A int
	B int
	C int
	D int
	E int
	F int
	G int
	H int
	I int
	J int
}
m := map[P]int{
	{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}: 1,
	{2, 2, 3, 4, 5, 6, 7, 8, 9, 10}: 2,
	{3, 2, 3, 4, 5, 6, 7, 8, 9, 10}: 3,
	{4, 2, 3, 4, 5, 6, 7, 8, 9, 10}: 4,
}
n := 0
for i := 0; i < %d/4; i++ {
	for k := range m {
		n = k.A
	}
}
_ = n`},
	}
	for _, s := range shapes {
		b.Run(s.name, func(b *testing.B) {
			in, fset, f := prepScript(b, fmt.Sprintf(s.body, benchIters))
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

// BenchmarkKeyCrossing prices the three pieces of a struct-keyed map
// crossing on their own, because BenchmarkStructContainer measures them
// buried under an interpreted loop that costs several times as much --
// which is enough to tell whether a change helped, and not enough to say
// WHERE.
//
// encode   structKeyOf: the script's struct into a comparable [N]any
// wrap     intoKeyStore: that encoding into the map's minted key type
// decode   decodeMintedKey: a key in the map back into the script's struct
//
// Four field counts, because encode and decode are per-field work and
// wrap is not, so a change that helps one and not the others shows here
// as a different slope rather than as one number moving. 6 is past the
// fanout keyArrFanout used to hold and inside the one it holds now,
// which is the arity where raising it showed: ~117ns of encode became
// ~34ns there while 1 and 3 barely moved. 10 is past the fanout
// altogether, so it is the only one of the four that prices the GENERAL
// path -- the one that fills through a slice and boxes by aliasing.
//
// THE DECODE INPUT COMES FROM MapKeys, and that is not incidental.
// reflect boxes a struct out of an ADDRESSABLE value by copying it to the
// heap first and out of a non-addressable one for free, so a decode
// benchmarked on a key straight from intoKeyStore -- which is addressable
// -- reports an allocation the real path never pays. A decode measured
// that way once made a change look like it saved an allocation per
// ranged key when it saved none.
//
// decodeMintedKey now TRADES on that difference rather than merely being
// measured under it: it lifts the whole carrier out with one Interface(),
// which is free only for a non-addressable key. So an input from
// intoKeyStore would not just misreport this benchmark, it would price a
// path the interpreter does not take. TestDecodingAMapKeyDoesNotCopyIt
// holds the real one to a single allocation.
func BenchmarkKeyCrossing(b *testing.B) {
	names := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	for _, nf := range []int{1, 3, 6, 10} {
		src := "type P struct {\n"
		for i := 0; i < nf; i++ {
			src += "\t" + names[i] + " int\n"
		}
		// The map literal is what forces the key type to be minted.
		src += "}\n_ = map[P]int{}\n"
		in, fset, f := prepScript(b, src)
		if err := in.Run(fset, f); err != nil {
			b.Fatalf("declaring P: %v", err)
		}
		v, ok := in.globals.Get("P")
		if !ok {
			b.Fatal("P was not defined")
		}
		st := v.(*StructType)

		vals := make([]Value, nf)
		for i := range vals {
			vals[i] = i
		}
		sv := &StructVal{Type: st, Vals: vals}
		k, err := structKeyOf(sv)
		if err != nil {
			b.Fatalf("encoding a key: %v", err)
		}
		mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
		mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(7))
		inMap := mp.MapKeys()[0]

		b.Run(fmt.Sprintf("encode/%d", nf), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				keySink, _ = structKeyOf(sv)
			}
		})
		b.Run(fmt.Sprintf("wrap/%d", nf), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				wrapSink = intoKeyStore(k, st.keyT)
			}
		})
		b.Run(fmt.Sprintf("decode/%d", nf), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				svSink = decodeMintedKey(inMap, nil)
			}
		})
	}
}

// BenchmarkMapKeyArena prices a WHOLE MAP's keys, which is the unit the
// range path actually decodes in and the one BenchmarkKeyCrossing
// structurally cannot show: it decodes a single key, so a change that
// amortises across a map reads there as no change at all.
//
// The two rows are the same decode differing only in where the StructVals
// come from -- one fused block each, against two slabs for the map -- so
// the gap is the keyArena, and ns/key rather than ns/op is what makes the
// rows comparable across key counts.
//
// KEY COUNTS 1 AND 2 STRADDLE THE THRESHOLD sortMapKeys applies, and they
// are here to keep it honest: the arena LOSES at one key and is a wash at
// two, so this benchmark is the thing to re-run before moving that bound.
// 64 is well past any map a script is likely to range and shows where the
// curve flattens -- the saving is bounded by the allocator, not the map.
// 256 CROSSES CHUNK BOUNDARIES: past keyChunk keys an arena cuts a fresh
// pair of slabs, so this is the row that prices the retention cap, and
// the one to re-run before moving keyChunk.
//
// Field counts 1 and 10 bracket the same way BenchmarkKeyCrossing's do: a
// decode is per-field work sitting on a per-KEY floor, and the arena only
// touches the floor, so the saving has to shrink as a fraction when the
// fields grow around it. Two arities are enough to see that it does.
func BenchmarkMapKeyArena(b *testing.B) {
	names := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	for _, nf := range []int{1, 10} {
		src := "type P struct {\n"
		for i := 0; i < nf; i++ {
			src += "\t" + names[i] + " int\n"
		}
		src += "}\n_ = map[P]int{}\n"
		in, fset, f := prepScript(b, src)
		if err := in.Run(fset, f); err != nil {
			b.Fatalf("declaring P: %v", err)
		}
		v, ok := in.globals.Get("P")
		if !ok {
			b.Fatal("P was not defined")
		}
		st := v.(*StructType)

		for _, nk := range []int{1, 2, 3, 4, 16, 64, 256} {
			mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
			for j := 0; j < nk; j++ {
				vals := make([]Value, nf)
				for i := range vals {
					vals[i] = j*100 + i
				}
				k, err := structKeyOf(&StructVal{Type: st, Vals: vals})
				if err != nil {
					b.Fatalf("encoding a key: %v", err)
				}
				mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(j))
			}
			// From MapKeys, for the reason given on BenchmarkKeyCrossing:
			// an addressable key would price a path the interpreter does
			// not take.
			keys := mp.MapKeys()

			b.Run(fmt.Sprintf("arena/f%d/k%d", nf, nk), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					a := newKeyArena(st, len(keys))
					for _, k := range keys {
						svSink = decodeMintedKey(k, a)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*nk), "ns/key")
			})
			b.Run(fmt.Sprintf("perkey/f%d/k%d", nf, nk), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					for _, k := range keys {
						svSink = decodeMintedKey(k, nil)
					}
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*nk), "ns/key")
			})
		}
	}
}

// Sinks for the three above: each result is otherwise dead and the
// compiler is free to delete the call that made it.
var (
	keySink  StructKey
	wrapSink reflect.Value
	svSink   *StructVal
)

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

// BenchmarkSortMapKeys prices the ORDERING of a whole map's keys, which
// is what a range over a map pays before its body runs once.
//
// It is a different unit again from BenchmarkMapKeyArena. That one prices
// the DECODE of n keys, which is linear in n; this one prices decode plus
// render plus sort, and the sort is the part whose shape is not linear.
// The key counts run to 1024 for exactly that reason: a quadratic term
// hiding under a linear one is invisible until the counts are far enough
// apart to separate them, and ns/key is what makes that visible -- a
// linear cost holds its ns/key flat as n grows, a quadratic one does not.
//
// EACH ITERATION RE-SCRAMBLES, by copying a fixed unsorted permutation
// over the working slice. sortMapKeys sorts in place, so without the copy
// every iteration after the first would sort an already-sorted slice,
// which is insertion sort's BEST case -- the benchmark would report O(n)
// for an O(n^2) loop. The copyonly row prices that copy so it can be
// subtracted rather than assumed small.
//
// The string row is the other half of the function: no decode and no
// render, just the sort, so it isolates the sort's own shape from the
// per-key work the struct row stacks on top of it.
func BenchmarkSortMapKeys(b *testing.B) {
	names := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	counts := []int{4, 16, 64, 256, 1024}

	for _, nf := range []int{1, 10} {
		src := "type P struct {\n"
		for i := 0; i < nf; i++ {
			src += "\t" + names[i] + " int\n"
		}
		src += "}\n_ = map[P]int{}\n"
		in, fset, f := prepScript(b, src)
		if err := in.Run(fset, f); err != nil {
			b.Fatalf("declaring P: %v", err)
		}
		v, ok := in.globals.Get("P")
		if !ok {
			b.Fatal("P was not defined")
		}
		st := v.(*StructType)

		for _, nk := range counts {
			mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
			for j := 0; j < nk; j++ {
				vals := make([]Value, nf)
				for i := range vals {
					vals[i] = j*100 + i
				}
				k, err := structKeyOf(&StructVal{Type: st, Vals: vals})
				if err != nil {
					b.Fatalf("encoding a key: %v", err)
				}
				mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(j))
			}
			keys := mp.MapKeys()
			work := make([]reflect.Value, len(keys))

			b.Run(fmt.Sprintf("struct/f%d/k%d", nf, nk), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					copy(work, keys)
					sortSink = sortMapKeys(work)
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*nk), "ns/key")
			})
			b.Run(fmt.Sprintf("copyonly/f%d/k%d", nf, nk), func(b *testing.B) {
				for b.Loop() {
					copy(work, keys)
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*nk), "ns/key")
			})
		}
	}

	for _, nk := range counts {
		sm := make(map[string]int, nk)
		for j := 0; j < nk; j++ {
			sm[fmt.Sprintf("key-%06d", j*7919%nk)] = j
		}
		keys := reflect.ValueOf(sm).MapKeys()
		work := make([]reflect.Value, len(keys))
		b.Run(fmt.Sprintf("string/k%d", nk), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				copy(work, keys)
				sortSink = sortMapKeys(work)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*nk), "ns/key")
		})
	}
}

// sortSink keeps sortMapKeys' result live; it is nil for the string row,
// which is itself part of what that row asserts.
var sortSink []Value
