package interp

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/rohanthewiz/serr"
)

// ---- script-declared struct types ----
//
// Method declarations are written here in the form the transform stage
// emits: `func (p Point) Sum() int` at the top of a file becomes a global
// closure named __m_Point_Sum whose first parameter is the receiver.
// Spelling that out rather than routing through transform is what keeps
// these tests about the interpreter -- callStructMethod's dispatch, the
// pointer-vs-value receiver rule, and the error messages -- and nothing
// else. internal/runner/methods_test.go covers the source-level spelling.

func TestStructDeclarationAndZeroValues(t *testing.T) {
	wantOut(t, `type P struct {
	Name string
	N    int
	F    float64
	OK   bool
}
var p P
fmt.Printf("%q %d %v %v\n", p.Name, p.N, p.F, p.OK)`, "\"\" 0 0 false\n")
}

func TestStructPositionalLiteral(t *testing.T) {
	wantOut(t, `type P struct {
	X, Y int
}
p := P{1, 2}
fmt.Println(p.X, p.Y)`, "1 2\n")
}

// A keyed literal may name any subset of the fields; the rest keep their
// zero values.
func TestStructKeyedLiteral(t *testing.T) {
	wantOut(t, `type P struct {
	X, Y int
}
p := P{Y: 5}
fmt.Println(p.X, p.Y)`, "0 5\n")
}

func TestStructFieldAssignment(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
p := P{}
p.X = 7
p.X += 3
fmt.Println(p.X)`, "10\n")
}

// A struct assigned to a second name is a second struct. StructVal is a
// pointer, so this only holds because the value is copied on the way IN
// to the new binding.
func TestStructAssignmentCopies(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
a := P{1}
b := a
b.X = 99
fmt.Println(a.X, b.X)`, "1 99\n")
}

// Every spelling that stores copies, not just `:=`. These are the sites
// copyOnStore is wired into, one test apiece, because each is a separate
// line that could be dropped without the others noticing.
func TestStructCopiesAtEveryStoreSite(t *testing.T) {
	decl := "type P struct {\n\tX int\n}\n"

	t.Run("plain assignment", func(t *testing.T) {
		wantOut(t, decl+`a := P{1}
var b P
b = a
b.X = 99
fmt.Println(a.X, b.X)`, "1 99\n")
	})

	t.Run("var declaration", func(t *testing.T) {
		wantOut(t, decl+`a := P{1}
var b = a
b.X = 99
fmt.Println(a.X, b.X)`, "1 99\n")
	})

	t.Run("function argument", func(t *testing.T) {
		wantOut(t, decl+`f := func(p P) {
	p.X = 99
}
a := P{1}
f(a)
fmt.Println(a.X)`, "1\n")
	})

	t.Run("slice element", func(t *testing.T) {
		wantOut(t, decl+`a := P{1}
xs := []any{a}
a.X = 99
fmt.Println(xs[0].X, a.X)`, "1 99\n")
	})

	t.Run("map value", func(t *testing.T) {
		wantOut(t, decl+`a := P{1}
m := map[string]any{"k": a}
a.X = 99
fmt.Println(m["k"].X, a.X)`, "1 99\n")
	})

	t.Run("container slot write", func(t *testing.T) {
		wantOut(t, decl+`a := P{1}
xs := []any{P{0}}
xs[0] = a
a.X = 99
fmt.Println(xs[0].X, a.X)`, "1 99\n")
	})

	t.Run("append", func(t *testing.T) {
		wantOut(t, decl+`a := P{1}
xs := []any{}
xs = append(xs, a)
a.X = 99
fmt.Println(xs[0].X, a.X)`, "1 99\n")
	})

	t.Run("struct field", func(t *testing.T) {
		wantOut(t, "type In struct {\n\tX int\n}\ntype Out struct {\n\tI In\n}\n"+`i := In{1}
o := Out{}
o.I = i
i.X = 99
fmt.Println(o.I.X, i.X)`, "1 99\n")
	})

	t.Run("struct literal field", func(t *testing.T) {
		wantOut(t, "type In struct {\n\tX int\n}\ntype Out struct {\n\tI In\n}\n"+`i := In{1}
o := Out{I: i}
i.X = 99
fmt.Println(o.I.X, i.X)`, "1 99\n")
	})

	t.Run("range variable", func(t *testing.T) {
		wantOut(t, decl+`xs := []any{P{1}, P{2}}
for _, v := range xs {
	v.X = 99
}
fmt.Println(xs[0].X, xs[1].X)`, "1 2\n")
	})
}

// The other half of the range rule, and the reason it matters: indexing
// still reaches the element itself, so `for i := range xs` is the
// spelling that mutates. A copy on READ would break this.
func TestStructIndexWriteStillMutatesInPlace(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
xs := []any{P{1}, P{2}}
for i := range xs {
	xs[i].X = 99
}
fmt.Println(xs[0].X, xs[1].X)`, "99 99\n")
}

// A struct-typed FIELD is part of the value, so it copies with it. This
// is where a flat copy of the field slice is not enough: b.In would be
// the SAME Inner as a.In, and writing through it would reach back.
func TestStructCopyDescendsIntoStructFields(t *testing.T) {
	wantOut(t, `type In struct {
	X int
}
type Out struct {
	I In
}
a := Out{In{1}}
b := a
b.I.X = 99
fmt.Println(a.I.X, b.I.X)`, "1 99\n")
}

// ...but only into struct fields. A slice, map or closure field is a
// reference, and a Go struct copy shares those too.
func TestStructCopyDoesNotDescendIntoReferenceFields(t *testing.T) {
	wantOut(t, `type P struct {
	Xs []int
	M  map[string]int
}
a := P{[]int{1}, map[string]int{"k": 1}}
b := a
b.Xs[0] = 99
b.M["k"] = 99
fmt.Println(a.Xs[0], a.M["k"])`, "99 99\n")
}

func TestStructUnknownFieldErrors(t *testing.T) {
	decl := `type P struct {
	X int
}
`
	wantErr(t, decl+`p := P{}
fmt.Println(p.Nope)`, "unknown field Nope in P")
	wantErr(t, decl+`p := P{}
p.Nope = 1`, "unknown field Nope in P")
	wantErr(t, decl+`p := P{Nope: 1}
_ = p`, "unknown field Nope in P")
}

func TestStructTooManyPositionalValues(t *testing.T) {
	wantErr(t, `type P struct {
	X int
}
p := P{1, 2}
_ = p`, "too many values in P literal")
}

// A keyed literal's key has to be a bare field name.
func TestStructLiteralKeyMustBeAFieldName(t *testing.T) {
	wantErr(t, `type P struct {
	X int
}
p := P{"X": 1}
_ = p`, "struct literal key must be a field name")
}

// The interpreter's String() renders every field in declaration order,
// which is what fmt.Println and the inspector both lean on.
func TestStructStringRendersAllFields(t *testing.T) {
	wantOut(t, `type P struct {
	X int
	S string
}
p := P{1, "hi"}
fmt.Println(p)`, "P{X: 1, S: hi}\n")
}

// ---- methods ----

// A value receiver gets a shallow copy: writes to its fields do not
// reach the caller's instance.
func TestValueReceiverSeesACopy(t *testing.T) {
	wantOut(t, `type P struct {
	N int
}
__m_P_Bump := func(p P) int {
	p.N = 99
	return p.N
}
v := P{1}
fmt.Println(v.Bump(), v.N)`, "99 1\n")
}

// A pointer receiver shares the instance, so its writes stick. The only
// thing distinguishing the two is the *T on the rewritten first
// parameter, which methodHasPtrRecv reads off the AST.
func TestPointerReceiverMutatesTheInstance(t *testing.T) {
	wantOut(t, `type P struct {
	N int
}
__m_P_Bump := func(p *P) {
	p.N = 99
}
v := P{1}
v.Bump()
fmt.Println(v.N)`, "99\n")
}

// "Shallow" is the operative word: a copy shares its reference-typed
// fields with the original, exactly as a Go struct copy does.
func TestValueReceiverCopyIsShallow(t *testing.T) {
	wantOut(t, `type P struct {
	Xs []int
}
__m_P_Poke := func(p P) {
	p.Xs[0] = 99
}
v := P{[]int{1}}
v.Poke()
fmt.Println(v.Xs[0])`, "99\n")
}

func TestMethodArgumentsFollowTheReceiver(t *testing.T) {
	wantOut(t, `type P struct {
	N int
}
__m_P_Add := func(p P, a int, b int) int {
	return p.N + a + b
}
v := P{10}
fmt.Println(v.Add(1, 2))`, "13\n")
}

// An unknown method names the type and shows how to declare one -- the
// hint matters because the method must be written at top level, which is
// not obvious from the call site.
func TestUnknownMethodErrorCarriesAHint(t *testing.T) {
	_, err := eval(t, `type P struct {
	N int
}
v := P{1}
v.Missing()`, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unknown method Missing on P") {
		t.Errorf("err = %v; want it to name the method and the type", err)
	}
}

// Method values are not supported, and reading a method as if it were a
// field says so specifically rather than reporting an unknown field --
// the two mistakes need different fixes.
func TestReadingAMethodAsAFieldIsDistinguished(t *testing.T) {
	wantErr(t, `type P struct {
	N int
}
__m_P_Get := func(p P) int { return p.N }
v := P{1}
fmt.Println(v.Get)`, "is a method of P")
}

// A method the script has not declared can still resolve to a native
// method on StructVal itself -- String being the one that matters, since
// it is what fmt reaches for.
func TestNativeMethodOnStructValStillResolves(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
v := P{3}
fmt.Println(v.String())`, "P{X: 3}\n")
}

func TestMethodSpreadCallIsNotSupported(t *testing.T) {
	wantErr(t, `type P struct {
	N int
}
__m_P_Add := func(p P, a int, b int) int { return a + b }
v := P{1}
xs := []int{1, 2}
fmt.Println(v.Add(xs...))`, "spread calls")
}

// ---- declarations the type system declines ----

func TestEmbeddedFieldsAreNotSupported(t *testing.T) {
	wantErr(t, `type Inner struct {
	X int
}
type Outer struct {
	Inner
}
var o Outer
_ = o`, "embedded fields are not supported")
}

func TestNonStructTypeDeclarationIsNotSupported(t *testing.T) {
	wantErr(t, `type MyInt int
var v MyInt
_ = v`, "only struct type declarations are supported")
}

// A field whose type the interpreter cannot resolve still declares --
// declareType leaves its zero nil rather than failing the whole type, so
// one exotic field does not sink a struct the script mostly uses.
func TestFieldWithAnUnresolvableTypeZeroesToNil(t *testing.T) {
	wantOut(t, `type P struct {
	Fn   func()
	Name string
}
var p P
fmt.Println(p.Fn == nil, p.Name == "")`, "true true\n")
}

// ---- script struct types in TYPE position ----
//
// A script struct has no reflect.Type of its own, so `[]P` is stored as
// `[]*StructVal` and TypeDesc.ST carries the identity reflect dropped.
// These cover what that erasure has to reconstruct.

// make must FILL a struct-element slice: reflect.MakeSlice leaves nil
// pointers behind, where Go's zero for a struct element is a struct.
func TestMakeSliceOfStructZeroes(t *testing.T) {
	wantOut(t, `type P struct {
	X int
	S string
}
xs := make([]P, 2)
fmt.Println(xs[0], xs[1].X)`, "P{X: 0, S: } 0\n")
	// The element of make([][]P, n) is a SLICE, and a nil slice already
	// is Go's zero -- so this must NOT be filled with anything.
	wantOut(t, `type P struct {
	X int
}
g := make([][]P, 2)
g[1] = append(g[1], P{3})
fmt.Println(len(g[0]), len(g[1]), g[1][0].X)`, "0 1 3\n")
	// A map needs no fill at all; a missing key is a separate question.
	wantOut(t, `type P struct {
	X int
}
m := make(map[string]P)
m["a"] = P{4}
fmt.Println(m["a"].X, len(m))`, "4 1\n")
}

// A struct-typed FIELD now resolves, so its zero is a real nested struct
// rather than nil. That zero lives on the StructType and is shared by
// every instance, which is what newZero's copy is for: without it, two
// zero values of the outer struct would share one inner struct.
func TestStructTypedFieldZeroIsPerInstance(t *testing.T) {
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
}
a := Out{}
b := Out{}
a.I.N = 7
fmt.Println(a.I.N, b.I.N)`, "7 0\n")
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
}
var o Out
fmt.Println(o)`, "Out{I: In{N: 0}}\n")
	// make's fill is the caller that RELIES on newZero duplicating the
	// type's nested zero: it writes each instance straight into the
	// slice, with no store site in between to isolate it.
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
}
xs := make([]Out, 2)
xs[0].I.N = 7
fmt.Println(xs[0].I.N, xs[1].I.N)`, "7 0\n")
	// The field's type also drives elision, the same as a []string field.
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
}
o := Out{I: {5}}
fmt.Println(o.I.N)`, "5\n")
}

// A self-referential field is left unresolved rather than looping:
// declareType defines the name only after every field is resolved, so a
// field type must already exist and the type graph is a DAG.
func TestSelfReferentialFieldDoesNotLoop(t *testing.T) {
	wantOut(t, `type N struct {
	Next N
	V    int
}
n := N{V: 1}
fmt.Println(n.V, n.Next == nil)`, "1 true\n")
}

// Value semantics reach through the new container types: a slice element
// is a store site on the way IN and a place on the way OUT, exactly as
// the scalar cases already pinned.
func TestStructSliceValueSemantics(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
xs := []P{{1}, {2}}
p := xs[0]
p.X = 99
xs[1].X = 5
for _, e := range xs {
	e.X = 100
}
fmt.Println(xs[0].X, xs[1].X, p.X)`, "1 5 99\n")
	// A pointer receiver reached through an element must see the element
	// itself, not a copy. Spelled in the post-transform form, as the
	// other method cases above are.
	wantOut(t, `type P struct {
	X int
}
__m_P_Bump := func(p *P) {
	p.X = p.X + 1
}
xs := []P{{1}}
xs[0].Bump()
fmt.Println(xs[0].X)`, "2\n")
}

// A key type Go itself would refuse is refused here, with the same
// verdict `==` uses so the two answers cannot drift apart. A
// non-comparable key type used to reach reflect.MapOf and surface as an
// unpositioned internal error; it is a positioned script error now.
func TestMapKeyTypesRefused(t *testing.T) {
	wantErr(t, `type P struct {
	Tags []string
}
m := map[P]int{}
_ = m`, "invalid map key type P: field Tags has type []string")
	wantErr(t, `m := map[[]int]int{}
_ = m`, "invalid map key type []int")
}

// A missing entry in a struct-valued map yields Go's ZERO STRUCT, which
// it could not before: the element type had erased to *StructVal, and a
// slot with no value in it has nothing to read a StructType off. It works
// now because the erasure moved into the TYPE -- a container holds the
// type minted for its own struct, and that type names one. See store.go.
func TestStructMapMissYieldsZeroStruct(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
m := map[string]P{"a": {1}}
v, ok := m["nope"]
fmt.Println(m["nope"] == nil, v, ok)`, "false P{X: 0} false\n")
	// The zero is a real struct, not a placeholder: its fields read and
	// write like any other, and it is not aliased to anything in the map.
	wantOut(t, `type P struct {
	X int
	S string
}
m := map[string]P{}
p := m["nope"]
p.X = 7
fmt.Println(p, m["nope"], len(m))`, "P{X: 7, S: } P{X: 0, S: } 0\n")
	// The miss zeroes every field type the struct declares, including a
	// nested struct and a reference field.
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I  In
	Xs []int
}
m := map[string]Out{}
fmt.Println(m["nope"], m["nope"].I.N, m["nope"].Xs == nil)`,
		"Out{I: In{N: 0}, Xs: []} 0 true\n")
	// Two misses do not share a nested struct -- the same isolation
	// newZero gives make([]P, 2).
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
}
m := map[string]Out{}
a := m["x"]
b := m["y"]
a.I.N = 5
fmt.Println(a.I.N, b.I.N)`, "5 0\n")
	// A missing SLICE element type still yields the nil slice, because
	// that IS Go's zero there. The repair is for struct slots only.
	wantOut(t, `type P struct {
	X int
}
m := map[string][]P{}
fmt.Println(m["nope"] == nil, len(m["nope"]))`, "true 0\n")
	// A nil element is reachable the same way through a slice, and every
	// path that could dereference it reports instead of panicking.
	wantErr(t, `type P struct {
	X int
}
var xs []P
xs = append(xs, nil)
fmt.Println(xs)
fmt.Println(xs[0].X)`, "nil struct has no field X")
}

// KNOWN DIVERGENCE, pinned.
//
// x.(P) is exact -- the declared struct type is compared, not the erased
// storage type, or every struct would satisfy every assertion. One level
// up the erasure still shows: []P and []Q are both []*StructVal, and the
// slice carries no per-element promise to check.
func TestTypeAssertionOnScriptStruct(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
type Q struct {
	X int
}
var v any = P{1}
_, okP := v.(P)
_, okQ := v.(Q)
z, _ := v.(Q)
fmt.Println(okP, okQ, z)`, "true false Q{X: 0}\n")
	// One level up, the type alone cannot answer: []P and []Q are the
	// SAME reflect.Type. The ELEMENTS answer instead, so a []P holding a
	// P is not a []Q -- P and Q having identical fields changes nothing,
	// because the check is on StructType identity, not shape.
	wantOut(t, `type P struct {
	X int
}
type Q struct {
	X int
}
var v any = []P{{1}}
_, okQ := v.([]Q)
_, okP := v.([]P)
fmt.Println(okQ, okP)`, "false true\n")
}

// ---- container assertions, and the one leaf still answered by value ----

func TestContainerAssertionIsExactOnElements(t *testing.T) {
	decls := `type P struct {
	X int
}
type Q struct {
	A bool
}
`
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		// A container holds the type minted for its own struct, so these
		// are ordinary reflect.Type comparisons -- no contents consulted.
		{"slice", `var v any = []P{{1}}
_, a := v.([]P)
_, b := v.([]Q)
fmt.Println(a, b)`, "true false\n"},
		{"map value", `var v any = map[string]P{"k": {1}}
_, a := v.(map[string]P)
_, b := v.(map[string]Q)
fmt.Println(a, b)`, "true false\n"},
		{"map value through a slice", `var v any = map[string][]P{"k": {{1}}}
_, a := v.(map[string][]P)
_, b := v.(map[string][]Q)
fmt.Println(a, b)`, "true false\n"},
		// The case the old element walk could not decide: with nothing
		// inside to read a StructType off, only the TYPE can answer -- and
		// now it does.
		{"empty is still exact", `var v any = []P{}
_, a := v.([]P)
_, b := v.([]Q)
fmt.Println(a, b)`, "true false\n"},
		{"nil element is still exact", `xs := []P{}
xs = append(xs, nil)
var v any = xs
_, a := v.([]P)
_, b := v.([]Q)
fmt.Println(a, b)`, "true false\n"},
		// Identical FIELDS are not identical types: the storage type is
		// keyed on the struct's name too (see structSig).
		{"same shape different name", `type R struct {
	X int
}
var v any = []P{{1}}
_, a := v.([]R)
fmt.Println(a)`, "false\n"},
		// A []any holding structs is not a []P, which is Go's answer too.
		{"[]any is not []P", `var v any = []any{P{1}}
_, a := v.([]P)
fmt.Println(a)`, "false\n"},
		// Nothing else moved.
		{"no struct leaf", `var v any = []int{1, 2}
_, a := v.([]int)
_, b := v.([]string)
fmt.Println(a, b)`, "true false\n"},
		{"failed assertion zeroes", `var v any = []P{{1}}
z, ok := v.([]Q)
fmt.Println(z, ok, z == nil)`, "[] false true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantOut(t, decls+tc.body, tc.want)
		})
	}
}

// A struct map KEY is minted per struct, so map[P]int and map[Q]int are
// different reflect.Types and the assertion is answered by the TYPE --
// including for a map that holds no key to read a struct off.
//
// This used to be a walk over the keys, which was exact for a populated
// map and had to ACCEPT whatever it could not decide: an empty map and a
// nil key both named no struct, so both asserted to either type.
func TestMapKeyAssertionUsesTheType(t *testing.T) {
	decls := `type P struct {
	X int
}
type Q struct {
	A bool
}
`
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"key names its struct", `var v any = map[P]int{{1}: 2}
_, a := v.(map[P]int)
_, b := v.(map[Q]int)
fmt.Println(a, b)`, "true false\n"},
		// Both leaves at once, each answered the same way: a minted type
		// per struct at the element edge and at the key edge.
		{"both leaves", `var v any = map[P]Q{{1}: {true}}
_, a := v.(map[P]Q)
_, b := v.(map[P]P)
_, c := v.(map[Q]Q)
fmt.Println(a, b, c)`, "true false false\n"},
		// THE LEAF THIS CLOSES. Neither an empty map nor a nil key names
		// a struct, so a walk over the keys had to accept both; the type
		// names one whether or not any key does.
		{"empty is exact", `var v any = map[P]int{}
_, a := v.(map[P]int)
_, b := v.(map[Q]int)
fmt.Println(a, b)`, "true false\n"},
		{"nil key is exact", `m := map[P]int{}
m[nil] = 7
var v any = m
_, a := v.(map[P]int)
_, b := v.(map[Q]int)
fmt.Println(a, b)`, "true false\n"},
		// A map reached through another container is exact too. The
		// descriptor drops its key leaf on the way down (TypeDesc.Elem),
		// so the walk could not see this key at all -- the type can.
		{"nested under a slice", `var v any = []map[P]int{{P{1}: 2}}
_, a := v.([]map[P]int)
_, b := v.([]map[Q]int)
fmt.Println(a, b)`, "true false\n"},
		{"nested and empty", `var v any = []map[P]int{}
_, a := v.([]map[P]int)
_, b := v.([]map[Q]int)
fmt.Println(a, b)`, "true false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantOut(t, decls+tc.body, tc.want)
		})
	}
}

// A container slot is TYPED now, so the write that used to slip a Q into
// a []P is reported where it happens rather than discovered later. Every
// route into a slot goes through convertTo, which is why one check covers
// all of them.
func TestContainerSlotRejectsAnotherStruct(t *testing.T) {
	decls := `type P struct {
	X int
}
type Q struct {
	A bool
}
`
	for _, tc := range []struct {
		name string
		body string
	}{
		{"append", `xs := []P{{1}}
xs = append(xs, Q{true})`},
		{"slice literal", `xs := []P{Q{true}}
_ = xs`},
		{"index assign", `xs := []P{{1}}
xs[0] = Q{true}`},
		{"map assign", `m := map[string]P{}
m["k"] = Q{true}`},
		{"map literal", `m := map[string]P{"k": Q{true}}
_ = m`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr(t, decls+tc.body, "cannot use Q{A: true} (Q) as P")
		})
	}
	// A nil still goes into any slot: it names no struct, and a typed nil
	// element was always reachable.
	wantOut(t, decls+`xs := []P{{1}}
xs = append(xs, nil)
fmt.Println(len(xs), xs[1] == nil)`, "2 true\n")
}

// ---- struct equality ----
//
// A *StructVal is a pointer, so `==` used to compare identities and call
// every separately-built pair of equal structs unequal. It compares
// FIELD-WISE now, the way Go compares struct values.

func TestStructEqualityFieldWise(t *testing.T) {
	// The case that was wrong before: two literals, same fields, never
	// the same instance.
	wantOut(t, `type P struct {
	X int
	S string
}
fmt.Println(P{1, "a"} == P{1, "a"}, P{1, "a"} == P{2, "a"}, P{1, "a"} != P{1, "b"})`,
		"true false true\n")
	// b := a makes a COPY (copyOnStore), so identity comparison called
	// these unequal. Field-wise, a copy equals its original.
	wantOut(t, `type P struct {
	X int
}
a := P{1}
b := a
fmt.Println(a == b, a == a)`, "true true\n")
	// A struct-typed field is part of the value and compares with it.
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
	T string
}
fmt.Println(Out{In{1}, "z"} == Out{In{1}, "z"}, Out{In{1}, "z"} == Out{In{2}, "z"})`,
		"true false\n")
	// A struct read back out of a container compares by value too --
	// there is no shared instance to lean on.
	wantOut(t, `type P struct {
	X int
}
m := map[string]P{"a": {1}}
xs := []P{{1}}
fmt.Println(m["a"] == P{1}, xs[0] == m["a"])`, "true true\n")
	// switch is the same operator, so it follows for free.
	wantOut(t, `type P struct {
	X int
}
p := P{2}
switch p {
case P{1}:
	fmt.Println("one")
case P{2}:
	fmt.Println("two")
}`, "two\n")
}

// Two DIFFERENT struct types are unequal rather than an error, matching
// what the rest of the interpreter already answers for a cross-type ==
// (`1 == "a"` is false, not a complaint). Go decides this at compile
// time; grsh has no compile time to decide it in.
func TestStructEqualityAcrossTypes(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
type Q struct {
	X int
}
fmt.Println(P{1} == Q{1}, P{1} != Q{1})`, "false true\n")
}

// A typed nil struct is reachable (append(xs, nil) makes one) and is the
// only struct value equal to nil. Go would reject `p == nil` outright;
// false is the honest answer where the comparison is allowed to happen.
func TestStructEqualityWithNil(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
var xs []P
xs = append(xs, nil)
fmt.Println(xs[0] == nil, P{1} == nil, nil == P{1}, xs[0] != nil)`,
		"true false false false\n")
	// Two typed nils reach structEqual itself, where the case above does
	// not: `xs[0] == nil` has an UNTYPED nil on the right, so it is
	// answered one level up. Only a comparison whose both sides came out
	// of a []P gets that far, and without the nil guard there it
	// dereferences.
	wantOut(t, `type P struct {
	X int
}
var xs []P
xs = append(xs, nil, nil, P{1})
fmt.Println(xs[0] == xs[1], xs[0] == xs[2], xs[2] == xs[0])`,
		"true false false\n")
}

// Comparability is decided from the STATIC field types, once, at
// declaration -- so == either always works for a type or always fails,
// instead of depending on whether a slice field happens to be nil at the
// moment of the comparison. The message names the field to change.
func TestStructEqualityRefusedOnUncomparableFields(t *testing.T) {
	wantErr(t, `type P struct {
	Tags []string
}
fmt.Println(P{} == P{})`, "P cannot be compared with ==: field Tags has type []string")
	wantErr(t, `type P struct {
	M map[string]int
}
fmt.Println(P{} == P{})`, "field M has type map[string]int")
	// A func type never resolves (grsh closures are not reflect funcs),
	// so this verdict is read off the syntax rather than off TypeDesc --
	// otherwise it would pass while both Fn fields were nil and start
	// failing the moment either was set.
	wantErr(t, `type P struct {
	Fn func(int) error
}
fmt.Println(P{} == P{})`, "field Fn has type func")
	// A slice OF the struct is the erasure's own case: []P is
	// []*StructVal, which reflect already calls incomparable.
	wantErr(t, `type P struct {
	X int
}
type Box struct {
	Ps []P
}
fmt.Println(Box{} == Box{})`, "field Ps has type []P")
	// The culprit can be several types down, so the path is dotted. A
	// struct field erases to a POINTER, which reflect calls comparable --
	// only the nested type's own verdict is correct here.
	wantErr(t, `type In struct {
	Tags []string
}
type Out struct {
	I In
}
fmt.Println(Out{} == Out{})`, "Out cannot be compared with ==: field I.Tags has type []string")
	// Two bad fields: the FIRST is what the message names, so the
	// diagnostic does not wander as a declaration grows.
	wantErr(t, `type P struct {
	Tags []string
	M    map[string]int
}
fmt.Println(P{} == P{})`, "field Tags has type []string")
	// Ordering is not defined on structs, in Go or here, and the message
	// names the script's type rather than *interp.StructVal.
	wantErr(t, `type P struct {
	X int
}
fmt.Println(P{1} < P{2})`, "operator < is not defined on P and P")
}

// A field whose declared type is comparable but whose VALUE need not be
// -- `any` -- is Go's runtime-panic case. The value is what gets checked,
// and the check reports instead of panicking.
func TestStructEqualityDynamicField(t *testing.T) {
	wantOut(t, `type P struct {
	V any
}
fmt.Println(P{1} == P{1}, P{1} == P{"a"}, P{} == P{})`, "true false true\n")
	wantErr(t, `type P struct {
	V any
}
fmt.Println(P{[]int{1}} == P{[]int{1}})`, "cannot compare a field holding []int")
	// Outside a struct the fallback still answers false for an
	// uncomparable pair rather than failing: only the FIELD walk, which
	// is claiming to implement Go's ==, reports.
	wantOut(t, `xs := []int{1}
ys := []int{1}
fmt.Println(xs == ys)`, "false\n")
}

// ---- struct map keys ----
//
// A struct key does not go into the map as itself: reflect.Map hashes
// with Go's own equality, which would compare the erased *StructVal
// POINTERS. It is encoded to a StructKey on the way in and decoded back
// on the way out, and these cover both directions.

func TestStructMapKeys(t *testing.T) {
	// The case the erasure used to make impossible: a lookup with a
	// FRESHLY BUILT key, which shares no identity with the stored one.
	wantOut(t, `type P struct {
	X int
	S string
}
m := map[P]int{}
m[P{1, "a"}] = 10
m[P{2, "b"}] = 20
fmt.Println(m[P{1, "a"}], m[P{2, "b"}], len(m))`, "10 20 2\n")
	// A copy is a different instance and still finds its entry.
	wantOut(t, `type P struct {
	X int
}
m := map[P]int{{1}: 10}
k := P{1}
k2 := k
fmt.Println(m[k2])`, "10\n")
	// A key literal elides its type, as Go's does.
	wantOut(t, `type P struct {
	X int
}
m := map[P]string{P{1}: "one", {2}: "two"}
fmt.Println(m[P{1}], m[P{2}], len(m))`, "one two 2\n")
	// delete and the comma-ok read take the same encoding path.
	wantOut(t, `type P struct {
	X int
}
m := map[P]int{{1}: 10, {2}: 20}
delete(m, P{1})
v, ok := m[P{1}]
w, ok2 := m[P{2}]
fmt.Println(len(m), v, ok, w, ok2)`, "1 0 false 20 true\n")
	// A struct on BOTH sides: the key erases to StructKey, the value takes
	// Q's minted storage type, and one descriptor carries both leaves. The
	// miss yields Q's zero, which the value side's minted type can name.
	wantOut(t, `type P struct {
	X int
}
type Q struct {
	Y int
}
m := map[P]Q{}
m[P{1}] = Q{9}
fmt.Println(m[P{1}].Y, len(m), m[P{2}])`, "9 1 Q{Y: 0}\n")
	// make and a nil map behave like any other map.
	wantOut(t, `type P struct {
	X int
}
m := make(map[P]int, 4)
m[P{1}] = 1
var n map[P]int
fmt.Println(len(m), len(n), n[P{1}])`, "1 0 0\n")
}

// The key travels BACK: range yields the script's own struct, not the
// storage, and printing renders it the same way.
func TestStructMapKeysComeBack(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
m := map[P]int{{2}: 20, {1}: 10}
for k, v := range m {
	fmt.Println(k.X, v, k)
}`, "1 10 P{X: 1}\n2 20 P{X: 2}\n")
	wantOut(t, `type P struct {
	X int
	S string
}
m := map[P]int{{1, "a"}: 10}
fmt.Println(m)`, "map[P{X: 1, S: a}:10]\n")
	// The rebuilt key is FRESH each iteration, so writing to the range
	// variable cannot reach the key inside the map and corrupt its
	// hashing.
	wantOut(t, `type P struct {
	X int
}
m := map[P]int{{1}: 10}
for k := range m {
	k.X = 99
}
fmt.Println(m[P{1}], len(m))`, "10 1\n")
	// The same claim for a map with SEVERAL keys, which is a different
	// path: one key is decoded on its own, more than one comes out of a
	// shared keyArena. Writing to the range variable must reach neither
	// the map nor the other keys of the same loop, and this is the only
	// place a script says so.
	wantOut(t, `type P struct {
	X int
	Y int
}
m := map[P]int{{1, 1}: 10, {2, 2}: 20, {3, 3}: 30}
for k, v := range m {
	k.X, k.Y = 99, 99
	fmt.Println(v)
}
for k, v := range m {
	fmt.Println(k, v)
}`, "10\n20\n30\nP{X: 1, Y: 1} 10\nP{X: 2, Y: 2} 20\nP{X: 3, Y: 3} 30\n")
}

// Two struct types never collide however alike their fields are, and the
// answer moved from the VALUE to the TYPE: a map[P]int's key type is P's,
// so a Q reaching it is reported where it happens.
//
// It used to be a silent miss -- the Q went in as a StructKey naming Q,
// found no entry, and yielded a zero. Go rejects `m[Q{1}]` outright, and
// this is the same line the element side draws for `xs[0] = Q{}`.
func TestStructMapKeysDoNotCollideAcrossTypes(t *testing.T) {
	decls := `type P struct {
	X int
}
type Q struct {
	X int
}
`
	// Every crossing takes the same hook, so all four report.
	for _, body := range []string{
		"m := map[P]int{{1}: 10}\nfmt.Println(m[Q{1}])",
		"m := map[P]int{{1}: 10}\nm[Q{1}] = 3",
		"m := map[P]int{{1}: 10}\ndelete(m, Q{1})",
		"m := map[P]int{Q{1}: 10}\n_ = m",
	} {
		wantErr(t, decls+body, "cannot use Q{X: 1} (Q) as P")
	}
	// A P still keys its own map, and the two maps stay separate types.
	wantOut(t, decls+`p := map[P]int{{1}: 10}
q := map[Q]int{{1}: 20}
fmt.Println(p[P{1}], q[Q{1}], len(p), len(q))`, "10 20 1 1\n")
}

// A key that is a CONTAINER of structs is not a struct key, and Go
// refuses it: []P is not comparable.
//
// It used to be accepted at the declaration, because the descriptor's
// struct leaf was consulted rather than whether the key IS one -- ST
// names the struct at an element leaf too. The map was then built keyed
// by P, and the first write failed with "cannot use [P{X: 1}] ([]P) as
// struct", pointing at the wrong line and the wrong thing.
func TestSliceOfStructIsNotAMapKey(t *testing.T) {
	wantErr(t, `type P struct {
	X int
}
m := map[[]P]int{}
_ = m`, "invalid map key type []P")
	// A map of them is refused the same way, and so is the case that
	// always worked: a struct whose own field costs it comparability.
	wantErr(t, `type P struct {
	X int
}
m := map[map[string]P]int{}
_ = m`, "invalid map key type map[string]P")
	wantErr(t, `type P struct {
	Tags []string
}
m := map[P]int{}
_ = m`, "invalid map key type P: field Tags has type []string")
}

// A nested struct FIELD is encoded recursively -- without that it would
// sit in the key as a *StructVal and be compared by identity again, one
// level deeper.
func TestStructMapKeysNestedField(t *testing.T) {
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
	T string
}
m := map[Out]int{}
m[Out{In{1}, "a"}] = 7
fmt.Println(m[Out{In{1}, "a"}], m[Out{In{2}, "a"}], m)`,
		"7 0 map[Out{I: In{N: 1}, T: a}:7]\n")
}

// structKeyOf has one code path PER FIELD COUNT up to keyArrFanout and a
// reflect path past it, so a key's arity is now a branch and every branch
// needs walking. A single case building an array of the wrong length --
// or dropping a field on the way in -- is invisible to any test that only
// ever uses a one-field key.
//
// Each arity is exercised through the map rather than through the
// encoder: an entry has to be STORED under a key and then FOUND by an
// equal one built separately, which is the property the array length and
// the field order both serve. Field 0 is held constant across the two
// keys so a miss can only come from the fields the arity added.
func TestStructMapKeysAtEveryArity(t *testing.T) {
	names := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L"}
	// Every arity the fast path enumerates, plus both edges: 0 crosses
	// the empty-array case, 8 is keyArrFanout itself, 9 is the first
	// arity that falls through to the general path, and 12 walks that
	// path somewhere it is not also the first case past a boundary.
	for _, n := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 12} {
		t.Run(fmt.Sprintf("%d-fields", n), func(t *testing.T) {
			var decl strings.Builder
			decl.WriteString("type P struct {\n")
			for i := 0; i < n; i++ {
				fmt.Fprintf(&decl, "\t%s int\n", names[i])
			}
			decl.WriteString("}\n")

			// lit writes a key literal whose field `differs` is bumped,
			// or the stored key itself when differs is out of range.
			// Every field gets a DISTINCT value, so a path that writes
			// them into the wrong slots -- or into one slot -- lands
			// somewhere the lookup does not.
			lit := func(differs int) string {
				var b strings.Builder
				b.WriteString("P{")
				for i := 0; i < n; i++ {
					if i > 0 {
						b.WriteString(", ")
					}
					v := i + 1
					if i == differs {
						v += 100
					}
					fmt.Fprintf(&b, "%d", v)
				}
				b.WriteString("}")
				return b.String()
			}

			// Both ends of the field list are probed, not just one: a
			// collapse that writes every field into slot 0 leaves the
			// LAST field deciding the key, so a miss on the last field
			// still reports correctly and only a miss on the first one
			// catches it.
			store := decl.String() + "m := map[P]int{}\nm[" + lit(-1) + "] = 7\n"
			src := store +
				"fmt.Println(m[" + lit(-1) + "], m[" + lit(n-1) + "], m[" + lit(0) + "], len(m))"
			// A zero-field struct has exactly one value, so both "miss"
			// keys are the stored key and find the entry.
			want := "7 0 0 1\n"
			if n == 0 {
				want = "7 7 7 1\n"
			}
			wantOut(t, src, want)

			// Lookups alone cannot see a PERMUTED encoding: a key stored
			// and sought through the same wrong order still matches
			// itself. Printing the map decodes the key back against the
			// field NAMES, which is where a swap shows.
			var render strings.Builder
			render.WriteString("map[P{")
			for i := 0; i < n; i++ {
				if i > 0 {
					render.WriteString(", ")
				}
				fmt.Fprintf(&render, "%s: %d", names[i], i+1)
			}
			render.WriteString("}:7]\n")
			wantOut(t, store+"fmt.Println(m)", render.String())
		})
	}
}

// The two paths in structKeyOf must produce the same TYPE, not merely a
// working key: the array's length is part of it, and keyArr is what the
// rest of the interpreter believes a key holds.
//
// A map only ever exercises one of the paths -- every key of a given
// struct takes the same branch -- so a length disagreement between them
// is invisible from a script and would surface only when the fanout is
// changed. This is the assertion that makes that safe to do.
func TestKeyEncodingMatchesTheDeclaredArrayType(t *testing.T) {
	names := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L"}
	for _, n := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 12} {
		src := "type P struct {\n"
		for i := 0; i < n; i++ {
			src += "\t" + names[i] + " int\n"
		}
		st := declare(t, src+"}", "P")
		k, err := structKeyOf(st.newZero())
		if err != nil {
			t.Fatalf("%d fields: encoding: %v", n, err)
		}
		if got := reflect.TypeOf(k.F); got != st.keyArr {
			t.Errorf("%d fields: key holds %s, want %s -- the fast path and the reflect path disagree on the array type", n, got, st.keyArr)
		}
	}
}

// A field declared `any` is statically comparable and dynamically
// anything -- Go's own runtime-panic case. Values that ARE comparable
// work, including different dynamic types under the same field; one that
// is not reports instead of panicking inside the map's hash.
func TestStructMapKeysDynamicField(t *testing.T) {
	wantOut(t, `type P struct {
	V any
}
m := map[P]int{{1}: 10, {"a"}: 20}
fmt.Println(m[P{1}], m[P{"a"}], len(m))`, "10 20 2\n")
	wantErr(t, `type P struct {
	V any
}
m := map[P]int{}
m[P{[]int{1}}] = 1`, "cannot use []int as part of a map key")
}

// KNOWN DIVERGENCE, pinned.
//
// TypeDesc carries ONE key leaf, so a map nested inside another container
// arrives knowing its key is a struct but not WHICH. Only ELISION is
// lost; naming the type works, and so does the map itself.
func TestStructMapKeyElisionNeedsTheTypeWhenNested(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
xs := []map[P]int{{P{1}: 2}}
fmt.Println(xs[0][P{1}])`, "2\n")
	wantErr(t, `type P struct {
	X int
}
xs := []map[P]int{{{1}: 2}}
_ = xs`, "a struct map key must name its type here")
}

// KNOWN DIVERGENCE, pinned.
//
// nil is a usable struct key, where Go rejects `m[nil]` for a struct key
// type outright. grsh has real typed-nil structs (append(xs, nil) makes
// one), so the zero StructKey is their honest encoding and nil finds nil.
func TestStructMapKeyNil(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
m := map[P]int{}
m[nil] = 5
fmt.Println(m[nil], len(m), m)`, "5 1 map[<nil>:5]\n")
}

// The inspector reads the key's name off the TYPE for the same reason it
// reads the element's off the type: both are minted per struct, so an
// EMPTY map[P]int is still a map[P]int on screen.
func TestInspectStructKeyedMap(t *testing.T) {
	got := inspect(t, `type P struct {
	X int
}
type Q struct {
	Y int
}
g := map[P]Q{{1}: {2}}`, "g")
	for _, want := range []string{"map[P]Q", "P{X: 1}: Q{Y: 2}"} {
		if !strings.Contains(got, want) {
			t.Errorf("inspect = %q, want it to contain %q", got, want)
		}
	}
	// An empty map has no key to read a name off, and used to fall back
	// to the neutral word. Its TYPE names P.
	if got := inspect(t, `type P struct {
	X int
}
g := map[P]int{}`, "g"); !strings.Contains(got, "map[P]int") {
		t.Errorf("inspect of an empty struct-keyed map = %q", got)
	}
}

// A typed nil struct is a usable key too, and it reaches the encoder
// where a bare `m[nil]` does not: convertTo answers an untyped nil with
// the key type's ZERO and never calls it. Both routes must agree, or a
// nil stored one way would be invisible to the other.
func TestStructMapKeyTypedNil(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
var xs []P
xs = append(xs, nil)
m := map[P]int{}
m[xs[0]] = 5
fmt.Println(m[nil], m[xs[0]], len(m))`, "5 5 1\n")
}

// The decode has to run at every DEPTH, not only at the top: a nested
// struct field went into the key as its own StructKey, and a range
// variable whose .I is still one of those is not a struct the script can
// use.
func TestStructMapKeyNestedFieldComesBack(t *testing.T) {
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
}
m := map[Out]int{{In{7}}: 1}
for k := range m {
	fmt.Println(k.I.N, k.I, k)
}`, "7 In{N: 7} Out{I: In{N: 7}}\n")
}

// The erasure must not reach a script-facing message, at any of the three
// places that render a type: the key of a map that IS one, a map type
// whose key struct the descriptor still names, and one whose key struct
// it no longer can.
func TestStructKeyTypeNamesInMessages(t *testing.T) {
	// convertTo used to see only the shared StructKey and could say no
	// more than the neutral word. A minted key type names its struct.
	wantErr(t, `type P struct {
	X int
}
m := map[P]int{}
m[5] = 1`, "cannot use 5 (int) as P")
	// The descriptor still holds KT here, so it names P.
	wantErr(t, `type P struct {
	X int
}
m := map[map[P]int]string{}
_ = m`, "invalid map key type map[P]int")
	// Elem dropped KT on the way through the slice, so the DESCRIPTOR
	// cannot name the struct -- but the minted key type in the reflect
	// type still does, which is what this used to render as
	// []map[struct]int.
	wantErr(t, `type P struct {
	X int
}
m := map[[]map[P]int]string{}
_ = m`, "invalid map key type []map[P]int")
}

// The general encode path hands an interface a pointer to an array it
// does not own a copy of, so the collector has to keep that array alive
// on the strength of the box alone. A type word and a data word that
// disagreed about what lives there would show up here and almost nowhere
// else: the key reads back correctly until a collection moves or frees
// what it points at.
//
// Ten fields puts every key on the general path, and the keys are held in
// a slice so they are live across the collections rather than dying
// immediately.
func TestWideKeysSurviveGC(t *testing.T) {
	st := declare(t, `type P struct {
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
}`, "P")
	vals := make([]Value, len(st.Fields))
	for i := range vals {
		vals[i] = i + 1
	}
	sv := &StructVal{Type: st, Vals: vals}
	want, err := structKeyOf(sv)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	keys := make([]StructKey, 0, 4096)
	for i := 0; i < 4096; i++ {
		k, err := structKeyOf(sv)
		if err != nil {
			t.Fatalf("encoding %d: %v", i, err)
		}
		keys = append(keys, k)
		if i%512 == 0 {
			runtime.GC()
		}
	}
	runtime.GC()
	runtime.GC()

	for i, k := range keys {
		if k != want {
			t.Fatalf("key %d stopped comparing equal after a collection", i)
		}
		// Decoding reads the array THROUGH the box, which is what a
		// range loop does, so a data word pointing somewhere stale shows
		// as the wrong fields rather than as an inequality.
		if got := decodeKeyArr(k.T, k.F); got.String() != sv.String() {
			t.Fatalf("key %d decodes to %s, want %s", i, got, sv)
		}
	}
}

// A nil field on the GENERAL path. The literal cases leave an unwritten
// buffer slot nil and the old reflect path skipped nil explicitly; the
// slice fill assigns it like any other value, and this is the shape that
// says all three agree. An `any` field is the only way a script reaches
// a nil field, and it is statically comparable, so P is still a key type.
func TestWideKeyWithNilField(t *testing.T) {
	wantOut(t, `type P struct {
	A int
	B int
	C int
	D int
	E any
	F int
	G int
	H int
	I int
	J int
}
m := map[P]int{}
m[P{1, 2, 3, 4, nil, 6, 7, 8, 9, 10}] = 7
fmt.Println(m[P{1, 2, 3, 4, nil, 6, 7, 8, 9, 10}], m[P{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}], len(m))`,
		"7 0 1\n")
}

// boxKeyArr claims two things the type system cannot: the result IS the
// declared array type, and it is the SAME array rather than a copy of it.
// Both are asserted here at several lengths, because the type word is
// borrowed from a cached zero and a length that took the wrong one would
// still produce a working-looking key.
//
// The aliasing half is checked by writing through the array AFTER boxing
// and watching the box change. That is not a property to rely on -- it is
// the hazard the design avoids by making structKeyOf's fill loop the
// array's only writer and letting it finish first -- but it is the
// evidence that no copy happened, which is the entire point of the box.
func TestKeyArrayBoxAliasesRatherThanCopies(t *testing.T) {
	for _, n := range []int{1, 3, 9, 12} {
		arr := reflect.ArrayOf(n, anyType)
		zero := mintKeyArrZero(arr)

		p := reflect.New(arr)
		slots := unsafe.Slice((*any)(p.UnsafePointer()), n)
		for i := range slots {
			slots[i] = i + 1
		}
		boxed := boxKeyArr(zero, p.UnsafePointer())

		if got := reflect.TypeOf(boxed); got != arr {
			t.Fatalf("%d: boxed as %v, want %v", n, got, arr)
		}
		bv := reflect.ValueOf(boxed)
		for i := 0; i < n; i++ {
			if got := bv.Index(i).Interface(); got != any(i+1) {
				t.Fatalf("%d: element %d is %v, want %d", n, i, got, i+1)
			}
		}
		// The box must not have copied: a later write through the array
		// has to be visible through it.
		slots[0] = "changed"
		if got := reflect.ValueOf(boxed).Index(0).Interface(); got != any("changed") {
			t.Errorf("%d: the box copied the array instead of aliasing it", n)
		}
	}
}

// decodeKeyArr reads the key's fields through an alias rather than
// through reflect, and an alias cannot bounds-check. What stands in for
// the bounds check is the type word: an array type's LENGTH is part of
// its identity, so comparing the box's type word against the one cached
// in keyArrZero settles length and element type in one pointer compare.
//
// The wrong-length case here is deliberately a LONGER array, so that
// both answers are defined: with the guard the fields come back nil,
// without it they come back 1, 2, 3. A shorter array would distinguish
// the two only by reading past the end, which is the thing being
// prevented and not a thing to demonstrate.
//
// The element type is checked the same way and cannot be probed the same
// way -- reading a [3]int as three interface words is exactly the misread
// the guard exists to stop -- so what this asserts about it is that the
// two type words differ, which is what the compare acts on.
func TestDecodeGuardsOnTheKeyArrayType(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
	C int
}`, "P")

	// The positive case first: a real key must still decode, or every
	// assertion below would pass for the wrong reason.
	k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{1, 2, 3}})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if got := decodeKeyArr(st, k.F).String(); got != "P{A: 1, B: 2, C: 3}" {
		t.Fatalf("a genuine key decoded to %s; the guard rejects what it must accept", got)
	}

	allNil := func(name string, a any) {
		t.Helper()
		sv := decodeKeyArr(st, a)
		if sv == nil || len(sv.Vals) != len(st.Fields) {
			t.Fatalf("%s: decoded to %v, want a %d-field struct", name, sv, len(st.Fields))
		}
		for i, v := range sv.Vals {
			if v != nil {
				t.Errorf("%s: field %d is %v, want nil -- the guard let a foreign array through", name, i, v)
			}
		}
	}
	// The reachable case: the zero StructKey, which `m[nil] = 1` puts in
	// a map, carries a nil F whose type word is nil.
	allNil("a nil field array", nil)
	// An array of the right element type and the wrong length.
	allNil("a [5]any", [5]any{1, 2, 3, 4, 5})

	intTyp, _ := unboxKeyArr([3]int{1, 2, 3})
	keyTyp, _ := unboxKeyArr(st.keyArrZero)
	if intTyp == keyTyp {
		t.Error("[3]int and [3]any share a type word; the guard cannot tell element types apart")
	}
}

// The decoded struct must own its values, and the alias is what makes
// that worth pinning: decodeKeyArr now reads the words of the key sitting
// INSIDE the map, so a version that passed those words on by reference
// rather than copying them out would hand a script a window into the
// map's own memory. Mutating a range variable would then change the key
// a live entry is hashed under, and the entry would become unfindable by
// the value it was stored with.
//
// TestStructMapKeysComeBack says the same thing from a script; this says
// it against the map key itself, where a shared backing array is visible
// directly rather than only through its consequences.
func TestDecodedKeyDoesNotAliasTheMapKey(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
}`, "P")
	k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{1, 2}})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(7))
	// From MapKeys, not from intoKeyStore: a key read out of the map is
	// the one a range loop yields, and it is the map's own memory.
	inMap := mp.MapKeys()[0]

	first := decodeMintedKey(inMap, nil)
	first.Vals[0] = 99

	second := decodeMintedKey(inMap, nil)
	if got := second.String(); got != "P{A: 1, B: 2}" {
		t.Errorf("after writing to a decoded key, the next decode reads %s; the decode aliases the map's key", got)
	}
	if &first.Vals[0] == &second.Vals[0] {
		t.Error("two decodes of one key share a backing array")
	}
	if got := mp.MapIndex(intoKeyStore(k, st.keyT)); !got.IsValid() || got.Int() != 7 {
		t.Errorf("the entry is no longer findable by the key it was stored with (%v)", got)
	}
}

// decodeMintedKey lifts the whole ScriptKey out of the map key with a
// single Interface(), and that is free only because a key from MapKeys is
// NOT ADDRESSABLE. reflect copies a three-word struct to the heap on its
// way into an interface when the value could still be written through,
// and hands back the words already sitting there when it could not.
//
// So the saving lives in a property of the CALLER rather than of the
// function, and losing it would change nothing about the answer -- only
// add an allocation per ranged key, which no test that reads the decoded
// struct back could notice. This is what notices.
//
// The count is exactly one: the *StructVal, fused with its field block by
// newStructVal. The value is asserted too, because a decode that did
// nothing would also allocate nothing.
func TestDecodingAMapKeyDoesNotCopyIt(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
}`, "P")
	k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{1, 2}})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(7))
	inMap := mp.MapKeys()[0]

	var sink *StructVal
	got := testing.AllocsPerRun(200, func() {
		sink = decodeMintedKey(inMap, nil)
	})
	if got != 1 {
		t.Errorf("decoding a map key allocates %.0f times, want 1 (the *StructVal): "+
			"lifting the carrier out of the key is copying it to the heap first", got)
	}
	if sink.String() != "P{A: 1, B: 2}" {
		t.Errorf("the key decoded to %s, want P{A: 1, B: 2}", sink.String())
	}
}

// A whole map's keys are decoded out of ONE arena, and the count that
// proves it is the count that does not grow.
//
// The assertion is written against BOTH ways of decoding the same keys,
// because "2 allocations" is worth nothing on its own -- it has to be 2
// where the alternative is n. Running the same loop with a nil arena in
// the same test is what turns the number into a guard, and it also pins
// the fallback: a nil arena must still decode, one fused block at a time.
func TestOneArenaServesAWholeMapsKeys(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
}`, "P")
	const nk = 8
	mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	for i := 0; i < nk; i++ {
		k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{i, i * 10}})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
	}
	keys := mp.MapKeys()

	var sink *StructVal
	arena := testing.AllocsPerRun(100, func() {
		a := newKeyArena(st, len(keys))
		for _, k := range keys {
			sink = decodeMintedKey(k, a)
		}
	})
	// The two slabs. newKeyArena itself is a third object, but it does
	// not escape this loop and the compiler keeps it on the stack.
	if arena != 2 {
		t.Errorf("decoding %d keys through one arena allocates %.0f times, want 2 (the two slabs)", nk, arena)
	}
	perKey := testing.AllocsPerRun(100, func() {
		for _, k := range keys {
			sink = decodeMintedKey(k, nil)
		}
	})
	if perKey != nk {
		t.Errorf("decoding %d keys without an arena allocates %.0f times, want %d "+
			"(one fused block each) -- the arena's %.0f is not being compared against anything", nk, perKey, nk, arena)
	}
	if sink == nil || sink.Type != st || len(sink.Vals) != 2 {
		t.Errorf("the decoded key is %v, want a two-field P -- a decode that did nothing would also allocate nothing", sink)
	}
}

// The arena has exactly one user, and this is the test that says so.
//
// Every other test here would pass with sortMapKeys reverted to a decode
// per key: the arena is a source of memory, not of answers, so nothing
// that reads a decoded key can see whether one was built. Removing the
// two lines that build it was tried, and the whole package stayed green.
//
// So the assertion is a COMPARISON against the same work done the other
// way. The control renders the keys exactly as sortMapKeys does -- a
// []string, a []Value, one String() per key -- differing only in where
// the StructVals come from, which makes every allocation the two share
// cancel and leaves the gap equal to what the arena replaced: nk fused
// blocks become 2 slabs.
func TestRangingAMapDecodesItsKeysIntoOneArena(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
}`, "P")
	const nk = 8
	mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	for i := 0; i < nk; i++ {
		k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{i, i * 10}})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
	}
	keys := mp.MapKeys()

	var sink []*StructVal
	sorted := testing.AllocsPerRun(50, func() { sink = sortMapKeys(keys, nil) })
	control := testing.AllocsPerRun(50, func() {
		// The control has to mirror sortMapKeys' CURRENT shape, not the
		// one it had when this test was written. It has been wrong twice
		// already: it built a []string with a String() per key after the
		// render became one appended slab, and it kept that slab after
		// the ordering stopped rendering at all. Both times the gap
		// silently grew from "the arena" to "the arena plus something
		// else" -- a floor assertion that still passed while measuring
		// something it did not name. Everything but the source of the
		// StructVals is copied from sortMapKeys deliberately, so that it
		// all cancels.
		out := make([]*StructVal, len(keys))
		for i, k := range keys {
			out[i] = decodeMintedKey(k, nil)
		}
		// keys is permuted in place, exactly as sortMapKeys permutes
		// it; copying it first would put an allocation in the control
		// that the thing under test does not have.
		var c keyCmp
		sort.Sort(&fieldOrder{keyOrder: &keyOrder{keys: keys, decoded: out}, cmp: &c})
		if c.declined {
			t.Errorf("the control's own keys declined; this map is meant to order field-wise")
		}
		sink = out
	})
	// The gap, not either number: what the two runs do differently is
	// where nk StructVals come from, so everything else cancels.
	//
	// It is a FLOOR rather than an equality because newKeyArena's header
	// is stack-allocated only while it inlines, and -race turns that off
	// -- so the ordinary build shows nk-2 (two slabs) and the race build
	// nk-3 (two slabs and a header). Both are the same claim; pinning the
	// exact value would make this test report the compiler's inlining
	// decisions as an arena bug.
	if gap, want := control-sorted, float64(nk-3); gap < want {
		t.Errorf("sortMapKeys allocates %.0f times for %d keys against %.0f for the same work decoded one key at a time, "+
			"a gap of %.0f; want at least %.0f, being %d fused blocks replaced by two slabs -- "+
			"the range path is not using an arena", sorted, nk, control, gap, want, nk)
	}
	if len(sink) != nk {
		t.Fatalf("the control decoded %d keys, want %d", len(sink), nk)
	}
	// THE ONE-KEY THRESHOLD IS NOT PINNED HERE, and that is a finding
	// rather than an omission. Three ways of catching a dropped length
	// test were built and all three measure the compiler instead:
	//
	//   - against this control, -race adds one allocation to sortMapKeys
	//     that the control does not see, which is exactly the size of the
	//     effect being looked for;
	//   - against a constant, every count on this path shifts under -race
	//     when newKeyArena stops inlining;
	//   - against the STEP from one key to two, which would cancel both,
	//     except that the step is not uniform: String's own allocations
	//     vary with the text a key renders to (7, 13, 19, 24, 29, 34 for
	//     one to six keys, and a different sequence under -race).
	//
	// Dropping the length test would cost 16-18ns per key on one-key maps
	// and 2-3ns on two-key ones, and would change no answer, so it is a
	// tuning decision. It is recorded the way this package records its
	// other tuned constants -- as a measurement in keyArena's doc, beside
	// BenchmarkMapKeyArena, which is where it can be re-run.
}

// Keys carved from one arena must be as independent as keys that were
// allocated apart, and that is the arena's ONE real hazard: n structs cut
// from a single []Value differ from n separate ones exactly if a carve
// ever hands out an overlapping window.
//
// TestStructMapKeysComeBack makes the same point from a script, but only
// for a one-key map -- which takes the nil-arena path and so cannot see
// this at all. Sharing would be invisible until a script wrote to a range
// variable and silently changed a DIFFERENT key of the same loop.
func TestArenaKeysDoNotShareFields(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
}`, "P")
	const nk = 5
	mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	for i := 0; i < nk; i++ {
		k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{i, i * 10}})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
	}
	keys := mp.MapKeys()

	a := newKeyArena(st, len(keys))
	svs := make([]*StructVal, len(keys))
	want := make([]string, len(keys))
	orig := make([][]Value, len(keys))
	for i, k := range keys {
		svs[i] = decodeMintedKey(k, a)
		want[i] = svs[i].String()
		orig[i] = append([]Value(nil), svs[i].Vals...)
	}
	// Write a sentinel into every field of every key in turn, and after
	// each write check that every OTHER key still reads what it decoded
	// to. Comparing backing-array addresses would catch the same bug, but
	// only the overlap it thought to look for; a write that lands in a
	// neighbour shows up here however the two came to overlap.
	//
	// Each field is restored immediately, so every struct stays an ARENA
	// slot for the whole sweep. Re-decoding the mutated key instead would
	// swap it for an independently allocated one and quietly stop testing
	// the slot the later keys are neighbours of.
	for i := range svs {
		for f := range svs[i].Vals {
			svs[i].Vals[f] = "sentinel"
			for j := range svs {
				if j == i {
					continue
				}
				if got := svs[j].String(); got != want[j] {
					t.Fatalf("writing field %d of key %d changed key %d to %s, want %s: "+
						"two keys from one arena share a backing array", f, i, j, got, want[j])
				}
			}
			svs[i].Vals[f] = orig[i][f]
		}
	}
}

// The general path sizes its slice from the declared array, never from
// the instance, so an instance carrying MORE values than the type has
// fields stops on a bounds check instead of writing past the array --
// which is what a slice sized from the instance would do, silently, into
// whatever the allocator put next.
//
// Nothing in the interpreter builds such an instance; every instance gets
// one Val per field. This pins the behaviour anyway, because the
// difference between the two ways of sizing that slice is invisible
// until the day something does.
func TestKeyEncodingRefusesAnOversizedInstance(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
	C int
	D int
	E int
	F int
	G int
	H int
	I int
}`, "P")
	// One Val per field plus one, and nine fields puts it on the general
	// path rather than in a literal case.
	vals := make([]Value, len(st.Fields)+1)
	for i := range vals {
		vals[i] = i
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("encoding an oversized instance did not panic; the fill wrote past the key array")
		}
	}()
	k, err := structKeyOf(&StructVal{Type: st, Vals: vals})
	t.Fatalf("expected a panic, got key %#v (err %v)", k, err)
}

// A SHORT instance is the other side of that guard and is not an error:
// the missing fields stay nil, which is what the literal path's unwritten
// buffer slots give too.
func TestKeyEncodingAcceptsAShortInstance(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
	C int
	D int
	E int
	F int
	G int
	H int
	I int
}`, "P")
	k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{1, 2}})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if got := reflect.TypeOf(k.F); got != st.keyArr {
		t.Fatalf("key holds %v, want %v", got, st.keyArr)
	}
	arr := reflect.ValueOf(k.F)
	if arr.Index(0).Interface() != any(1) || arr.Index(1).Interface() != any(2) {
		t.Errorf("the values given did not land in the first slots: %v", k.F)
	}
	for i := 2; i < arr.Len(); i++ {
		if got := arr.Index(i).Interface(); got != nil {
			t.Errorf("slot %d is %v, want nil", i, got)
		}
	}
}

// TestStructValFusesUpToTheFanout is what keeps valBlockFanout honest.
//
// The enumeration in newStructVal cannot see the constant -- the cases
// are written out one per length, because an array length cannot be a
// variable -- so nothing but this test stops the two from drifting. A
// constant raised without a case would silently put those arities back on
// the two-allocation path and cost nothing visible; here it fails.
//
// The assertion is on ALLOCATION COUNT rather than on time, because the
// count is what the fusing is: same bytes, one object instead of two.
func TestStructValFusesUpToTheFanout(t *testing.T) {
	for n := 1; n <= valBlockFanout; n++ {
		st := &StructType{Name: "P", Fields: make([]string, n)}
		var sink *StructVal
		got := testing.AllocsPerRun(200, func() {
			sink = newStructVal(st, n)
		})
		if got != 1 {
			t.Errorf("newStructVal(%d fields) allocates %.0f times, want 1: valBlockFanout is %d but the switch has no case for this length", n, got, valBlockFanout)
		}
		if sink == nil || len(sink.Vals) != n || cap(sink.Vals) != n {
			t.Errorf("newStructVal(%d fields) gave Vals len %d cap %d, want %d and %d", n, len(sink.Vals), cap(sink.Vals), n, n)
		}
	}
}

// TestStructValsDoNotShareABlock is the hazard the fusing introduces and
// has to be shown not to have.
//
// Vals now points INTO the same object as the StructVal it belongs to, so
// a mistake in the enumeration -- a case slicing the wrong block, or two
// instances handed the same one -- would make two script structs share
// their fields, and every test that writes one field and reads it back on
// the same instance would still pass. This writes through one and reads
// the other.
func TestStructValsDoNotShareABlock(t *testing.T) {
	for n := 1; n <= valBlockFanout+2; n++ { // +2 to cover the fallthrough
		st := &StructType{Name: "P", Fields: make([]string, n)}
		a, b := newStructVal(st, n), newStructVal(st, n)
		for i := range a.Vals {
			a.Vals[i] = i + 1
		}
		for i, v := range b.Vals {
			if v != nil {
				t.Fatalf("%d fields: writing one instance set slot %d of another to %v; the two share a block", n, i, v)
			}
		}
	}
}

// TestFusedStructValSurvivesGC forces a collection while only the
// StructVal pointer is held.
//
// &b.sv is the block's base pointer, so the block stays reachable through
// it -- but the fields live PAST that pointer, and a layout the collector
// read differently (a block whose sv were not at offset 0, say) would let
// them be swept while the struct is still live. Values are heap strings
// rather than small ints so a freed slot shows up as garbage rather than
// as a still-valid small-int box.
func TestFusedStructValSurvivesGC(t *testing.T) {
	const n = 6
	st := &StructType{Name: "P", Fields: make([]string, n)}
	sv := newStructVal(st, n)
	for i := range sv.Vals {
		sv.Vals[i] = fmt.Sprintf("field-%d-%s", i, strings.Repeat("x", 32))
	}
	runtime.GC()
	runtime.GC()
	for i, v := range sv.Vals {
		want := fmt.Sprintf("field-%d-%s", i, strings.Repeat("x", 32))
		if v != want {
			t.Fatalf("slot %d is %q after GC, want %q", i, v, want)
		}
	}
}

// TestCopyStructSizesFromTheInstance pins the reason newStructVal takes
// an n rather than reading t.Fields.
//
// copyStruct is the one constructor whose length comes from the VALUE:
// an instance carrying more Vals than its type has fields is malformed,
// and the copy has to reproduce it rather than truncate it -- truncating
// would turn a detectable bad value into a plausible good one, and
// structKeyOf's oversize check (which panics on exactly this shape) would
// then never see it.
func TestCopyStructSizesFromTheInstance(t *testing.T) {
	st := declare(t, "type P struct {\n\tA int\n}", "P")
	sv := &StructVal{Type: st, Vals: []Value{1, 2, 3}}
	got := sv.copyStruct()
	if len(got.Vals) != 3 {
		t.Fatalf("copying a 3-Val instance of a 1-field type gave %d Vals, want 3", len(got.Vals))
	}
	got.Vals[0] = 9
	if sv.Vals[0] != 1 {
		t.Fatal("the copy shares its Vals with the original")
	}
}

// ---- rendering into a caller's buffer ----

// appendValue's type switch is a FAST PATH for what fmt would print, so
// every case has to be held against fmt itself rather than against a
// hand-written string. A case that drifts from %v does not fail loudly:
// it silently changes what a script prints, and -- because the same
// render decides map key order -- silently reorders a range.
//
// The table is every case in the switch plus a fallthrough, and each
// value is checked both bare and as a struct field, because appendTo and
// appendValue are separately capable of getting the surrounding text
// wrong.
func TestAppendValueMatchesFmt(t *testing.T) {
	st := declare(t, `type P struct {
	A int
}`, "P")
	nested := &StructVal{Type: st, Vals: []Value{7}}
	vals := []Value{
		nil,
		"a string",
		"",
		0,
		-1,
		1 << 40,
		true,
		false,
		'x',        // rune, which is int32
		int64(-99), // what a stdlib call hands back
		3.5,
		0.1,
		1e21,
		-0.0,
		math.Inf(1),
		math.Inf(-1),
		math.NaN(),
		nested,
		(*StructVal)(nil), // a nil struct field, which []P makes reachable

		// One slice per case, each in three states, because the frame
		// and the separator are written per case and a case that drops
		// the brackets on an empty slice -- or emits a leading space on
		// a one-element one -- is wrong in exactly one of them.
		[]string{"x", "y"}, []string{"solo"}, []string{}, []string(nil),
		[]string{"", " ", "with space"}, // the separator is ambiguous, and fmt's is too
		[]int{1, -2, 0}, []int{}, []int(nil),
		[]int64{1 << 40, -1}, []int64{},
		[]float64{1.5, 0.1, 1e21, math.Inf(-1), math.NaN()}, []float64{},
		[]bool{true, false}, []bool{},
		[]byte("hi"), []byte{0, 255}, []byte{}, // %v on a []byte is NUMBERS, not text
		[]rune("hi"), []rune{}, // []int32, so numbers too
		[]any{1, "x", true, nil, nested, 2.5}, []any{}, []any(nil),
		[]any{[]string{"deep"}}, // a case reached from inside another
		[]error{errors.New("boom"), nil}, []error{},

		// No fast path: these must still fall through to fmt.
		map[string]int{"k": 1},
		[][]string{{"a"}, nil},
		[]uint{1, 2}, // spellable only from a stdlib call, not from typeOf
	}
	for _, v := range vals {
		if got, want := string(appendValue(nil, v)), fmt.Sprintf("%v", v); got != want {
			t.Errorf("appendValue(%#v) = %q, fmt says %q", v, got, want)
		}
		// The same value in a field, where appendTo supplies the frame.
		sv := &StructVal{Type: st, Vals: []Value{v}}
		if got, want := sv.String(), fmt.Sprintf("P{A: %v}", v); got != want {
			t.Errorf("rendering a field holding %#v gives %q, want %q", v, got, want)
		}
	}
}

// The slice fast paths are a CLOSED set, and this is the test that keeps
// them closed. typeOf builds a field's type from typeIdents, so the
// element types a script can spell are exactly typeIdents' names plus a
// script struct -- and adding a name there without adding a case to
// appendValue would silently drop that slice back onto fmt, with no
// visible symptom because fmt is still correct.
//
// So the table is keyed by typeIdents' own names and the key sets are
// required to match: a new native type name fails here until someone
// writes a sample for it and decides whether it gets a case.
//
// Each sample is checked two ways. Parity with fmt is the correctness
// claim. ZERO ALLOCATIONS is the claim the fast path exists for, and it
// is what actually detects a missing case: fmt reflects over the slice
// and boxes every element, which costs 2 allocations for seven of these
// nine and 4 for a []any of structs. It cannot detect the other two --
// fmt has its own fast path for []byte, and an error already carries its
// own interface -- so those two rest on parity alone, which is noted
// rather than hidden because a green test that cannot fail is worse than
// no test.
func TestEveryScriptSliceTypeHasAFastPath(t *testing.T) {
	st := declare(t, `type P struct {
	A int
}`, "P")
	nested := &StructVal{Type: st, Vals: []Value{7}}
	// Element values are chosen to make fmt's boxing VISIBLE: an int
	// small enough to hit the runtime's cached-small-integer table boxes
	// without allocating, which would hide a missing case.
	samples := map[string]Value{
		"int":     []int{1 << 40, -(1 << 41)},
		"int64":   []int64{1 << 40, -(1 << 41)},
		"float64": []float64{1.5, 1e21},
		"string":  []string{"alpha", "beta"},
		"bool":    []bool{true, false},
		"byte":    []byte{7, 200},                   // fmt does not allocate here
		"rune":    []rune{100000, 200000},           // []int32, printed as numbers
		"any":     []any{nested, "x", nil},          // recursion, not fmt's Stringer
		"error":   []error{errors.New("boom"), nil}, // fmt does not allocate here
	}
	for name := range typeIdents {
		if _, ok := samples[name]; !ok {
			t.Errorf("typeIdents has %q but this test has no sample for it: a script can "+
				"write []%s, so appendValue needs a case for it or a reason not to", name, name)
		}
	}
	for name := range samples {
		if _, ok := typeIdents[name]; !ok {
			t.Errorf("this test samples []%s but typeIdents no longer has %q", name, name)
		}
	}

	// The cases are also checked STRUCTURALLY, by reading the switch out
	// of the source. That is not belt and braces: removing the []byte or
	// []error case changes nothing the assertions below can see -- fmt
	// renders both correctly and, uniquely among these nine, allocates
	// for neither -- so those two cases would otherwise be code no test
	// can tell is gone. Parsing is what makes every case falsifiable.
	cases := map[string]bool{}
	for _, name := range sliceCasesOf(t, "appendValue") {
		cases[name] = true
		if _, ok := samples[name]; !ok {
			t.Errorf("appendValue has a case for []%s that this test does not sample", name)
		}
	}
	for name := range samples {
		if !cases[name] {
			t.Errorf("appendValue has no `case []%s:`, so a script's []%s falls back to fmt",
				name, name)
		}
	}

	// One buffer with room to spare, reused: a slab that has to grow
	// would allocate for reasons that have nothing to do with the case
	// under test, which is exactly how sortMapKeys uses this.
	buf := make([]byte, 0, 4096)
	for name, v := range samples {
		if got, want := string(appendValue(nil, v)), fmt.Sprintf("%v", v); got != want {
			t.Errorf("appendValue on a []%s gave %q, fmt says %q", name, got, want)
		}
		if n := testing.AllocsPerRun(100, func() { buf = appendValue(buf[:0], v) }); n != 0 {
			t.Errorf("rendering a []%s allocated %.0f times, want 0 -- has its case in "+
				"appendValue been removed, sending it back to fmt?", name, n)
		}
	}
}

// []P is the tenth slice type and the only one with no static case: its
// element type is minted at runtime, so the switch cannot name it and
// the render reaches it through reflect instead.
//
// The value is built by a SCRIPT rather than by calling mintStoreType
// here, because what the fast path has to match is the type a script
// actually produces -- a hand-built slice could be a []*StructVal and
// pass while the reachable one fell through to fmt.
func TestASliceOfStructsRendersWithoutFmt(t *testing.T) {
	in, _, err := evalKeep(t, `type P struct {
	A int
	B string
}
xs := []P{P{A: 1, B: "one"}, P{A: 2, B: "two"}}
empty := []P{}`, nil)
	if err != nil {
		t.Fatalf("building a []P: %v", err)
	}
	for _, name := range []string{"xs", "empty"} {
		v, ok := in.globals.Get(name)
		if !ok {
			t.Fatalf("%s was not defined", name)
		}
		if rt := reflect.TypeOf(v); rt.Kind() != reflect.Slice || storeOwnerOf(rt.Elem()) == nil {
			t.Fatalf("%s is a %v, not a slice of a minted element type -- this test is "+
				"no longer pointed at the path it means to test", name, rt)
		}
		if got, want := string(appendValue(nil, v)), fmt.Sprintf("%v", v); got != want {
			t.Errorf("appendValue on %s gave %q, fmt says %q", name, got, want)
		}
		buf := make([]byte, 0, 4096)
		if n := testing.AllocsPerRun(100, func() { buf = appendValue(buf[:0], v) }); n != 0 {
			t.Errorf("rendering %s allocated %.0f times, want 0 -- fmt would call String "+
				"and box per element, so this is the reflect path being lost", name, n)
		}
	}

	// A nil element is reachable: append(xs, nil) puts a typed nil in the
	// slot. It has to reach appendTo's nil check rather than panic on the
	// way through the two Field hops.
	v, _ := in.globals.Get("xs")
	rv := reflect.ValueOf(v)
	withNil := reflect.Append(rv, reflect.Zero(rv.Type().Elem())).Interface()
	if got, want := string(appendValue(nil, withNil)), fmt.Sprintf("%v", withNil); got != want {
		t.Errorf("a []P with a nil element gave %q, fmt says %q", got, want)
	}
}

// The end of the same claim, from the outside: a struct whose fields hold
// every slice shape prints through the interpreter exactly as fmt would
// print the same values. The unit tests above render each slice alone;
// this one puts them in the frame appendTo supplies, which is where a
// case that forgot its closing bracket would show.
func TestAStructWithSliceFieldsPrintsLikeFmt(t *testing.T) {
	wantOut(t, `type P struct {
	A int
}
type S struct {
	Str  []string
	Ints []int
	I64  []int64
	Fs   []float64
	Bs   []bool
	By   []byte
	Rs   []rune
	As   []any
	Ps   []P
	Nest [][]string
	M    map[string]int
}
s := S{Str: []string{"a", "b"}, Ints: []int{1, 2}, I64: []int64{3}, Fs: []float64{1.5},
	Bs: []bool{true}, By: []byte{104, 105}, Rs: []rune{97}, As: []any{1, "x"},
	Ps: []P{P{A: 9}}, Nest: [][]string{[]string{"deep"}}, M: map[string]int{"k": 1}}
fmt.Println(s)`,
		"S{Str: [a b], Ints: [1 2], I64: [3], Fs: [1.5], Bs: [true], By: [104 105], "+
			"Rs: [97], As: [1 x], Ps: [P{A: 9}], Nest: [[deep]], M: map[k:1]}\n")
}

// ---- rendering a struct-keyed map ----

// mapOfStructKeys builds a map[P]V the way the interpreter does: keys
// encoded through structKeyOf and wrapped in P's minted key type, values
// converted to vt. Tests that hand-build a map[StructKey]V instead would
// be testing a type no script can produce.
func mapOfStructKeys(t *testing.T, st *StructType, vt reflect.Type, keys [][]Value, vals []Value) reflect.Value {
	t.Helper()
	m := reflect.MakeMap(reflect.MapOf(st.keyT, vt))
	for i, fields := range keys {
		// A nil FIELD LIST means the nil key -- `m[nil] = v` -- which
		// encodes to the zero StructKey and is a distinct entry from any
		// struct.
		k := StructKey{}
		if fields != nil {
			var err error
			if k, err = structKeyOf(&StructVal{Type: st, Vals: fields}); err != nil {
				t.Fatalf("encoding key %v: %v", fields, err)
			}
		}
		v := reflect.New(vt).Elem()
		if vals[i] != nil {
			v.Set(reflect.ValueOf(vals[i]))
		}
		m.SetMapIndex(intoKeyStore(k, st.keyT), v)
	}
	return m
}

// THE TEST THE WHOLE FAST PATH RESTS ON.
//
// appendStructKeyedMap reproduces the ordering of internal/fmtsort, an
// UNEXPORTED package of the standard library with no compatibility
// promise. Reproducing it is only defensible if a divergence is loud, so
// this renders randomised maps both ways and requires the bytes to be
// equal: a Go release that changes how maps print fails here rather than
// leaving grsh quietly printing a different order from fmt.
//
// The randomisation is over the two things order depends on -- the field
// VALUES and their TYPES -- across every value type a map can hold, at
// sizes either side of the arena threshold. The seed is fixed so a
// failure is reproducible, and printed so a future seed sweep can be run
// by hand.
func TestAStructKeyedMapMatchesFmt(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B string
}`, "P")
	// Values a field can hold, chosen so that ties, sign, and every arm
	// of keyCmp.field are all reachable. nil is in the pool because an
	// unset field is nil and fmtsort puts it low.
	pool := []Value{
		nil, 0, 1, -1, 1 << 40, "", "a", "b", "zzz", true, false,
		0.5, -0.5, 'x', 'y', byte(3), int64(1 << 41),
	}
	valTypes := []reflect.Type{
		reflect.TypeFor[int](),
		reflect.TypeFor[string](),
		reflect.TypeFor[bool](),
		reflect.TypeFor[float64](),
		reflect.TypeFor[rune](),
		reflect.TypeFor[byte](),
		reflect.TypeFor[int64](),
		reflect.TypeFor[any](),
		st.storeT, // map[P]P: a minted struct in the value position
	}
	const seed = 20260829
	rng := rand.New(rand.NewSource(seed))
	for _, n := range []int{1, 2, 3, 7, 40} {
		for _, vt := range valTypes {
			for trial := 0; trial < 40; trial++ {
				keys := make([][]Value, n)
				vals := make([]Value, n)
				for i := range keys {
					if rng.Intn(8) == 0 {
						keys[i] = nil // the nil key, which must sort first
					} else {
						keys[i] = []Value{pool[rng.Intn(len(pool))], pool[rng.Intn(len(pool))]}
					}
					vals[i] = sampleOfType(t, st, vt, rng)
				}
				m := mapOfStructKeys(t, st, vt, keys, vals)
				got := string(appendValue(nil, m.Interface()))
				want := fmt.Sprintf("%v", m.Interface())
				if got != want {
					t.Fatalf("seed %d, n=%d, values %v, trial %d:\n got %s\nwant %s",
						seed, n, vt, trial, got, want)
				}
			}
		}
	}
}

// THE TEST THAT SAYS A RANGE AND A PRINT AGREE, over the same randomised
// maps the renderer's parity test uses.
//
// It is not a second copy of that test. That one holds appendStructKeyedMap
// against fmt; this one holds sortMapKeys -- a different function, with a
// different sort, reaching keyCmp from the other side -- against the same
// fixed point, by rebuilding fmt's map text out of the order the RANGE
// would visit. If the two ever choose different orders for one map, the
// rebuilt text stops matching.
//
// It skips the maps keyCmp declines, and that is not a gap: fmt orders
// those by a machine address, so there is no order to agree with. What
// they get instead is determinism, which
// TestADeclinedMapRangesInRenderedTextOrder pins. Both arms are counted
// and both are required to be non-empty, because a change that made
// everything decline would otherwise leave this test asserting nothing.
func TestARangeVisitsAStructKeyedMapInFmtsOrder(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B string
}`, "P")
	pool := []Value{
		nil, 0, 1, -1, 1 << 40, "", "a", "b", "zzz", true, false,
		0.5, -0.5, 'x', 'y', byte(3), int64(1 << 41),
	}
	valTypes := []reflect.Type{
		reflect.TypeFor[int](),
		reflect.TypeFor[string](),
		reflect.TypeFor[any](),
		st.storeT,
	}
	const seed = 20260829
	rng := rand.New(rand.NewSource(seed))
	var took, declined int
	for _, n := range []int{2, 3, 7, 40} {
		for _, vt := range valTypes {
			for trial := 0; trial < 40; trial++ {
				keys := make([][]Value, n)
				vals := make([]Value, n)
				for i := range keys {
					if rng.Intn(8) == 0 {
						keys[i] = nil // the nil key, which must sort first
					} else {
						keys[i] = []Value{pool[rng.Intn(len(pool))], pool[rng.Intn(len(pool))]}
					}
					vals[i] = sampleOfType(t, st, vt, rng)
				}
				m := mapOfStructKeys(t, st, vt, keys, vals)
				if _, ok := appendStructKeyedMap(nil, m); !ok {
					declined++
					continue
				}
				took++
				// sortMapKeys permutes both slices together, so keys[i]
				// is still the map key that decoded[i] came from and is
				// what its value has to be read with.
				mk := m.MapKeys()
				decoded := sortMapKeys(mk, nil)
				b := []byte("map[")
				for i, sv := range decoded {
					if i > 0 {
						b = append(b, ' ')
					}
					b = sv.appendTo(b)
					b = append(b, ':')
					b = appendValue(b, fromStore(m.MapIndex(mk[i])))
				}
				b = append(b, ']')
				if got, want := string(b), fmt.Sprintf("%v", m.Interface()); got != want {
					t.Fatalf("seed %d, n=%d, values %v, trial %d:\n ranged %s\n fmt    %s",
						seed, n, vt, trial, got, want)
				}
			}
		}
	}
	if took == 0 || declined == 0 {
		t.Errorf("%d maps ordered field-wise and %d declined; both arms have to be reached "+
			"or this test is asserting nothing", took, declined)
	}
}

// sampleOfType makes one value assignable to vt, for the randomised
// parity test above.
func sampleOfType(t *testing.T, st *StructType, vt reflect.Type, rng *rand.Rand) Value {
	t.Helper()
	switch vt {
	case reflect.TypeFor[int]():
		return rng.Intn(1000) - 500
	case reflect.TypeFor[string]():
		return []string{"", "x", "yy"}[rng.Intn(3)]
	case reflect.TypeFor[bool]():
		return rng.Intn(2) == 0
	case reflect.TypeFor[float64]():
		return float64(rng.Intn(100)) / 4
	case reflect.TypeFor[rune]():
		return rune('a' + rng.Intn(26))
	case reflect.TypeFor[byte]():
		return byte(rng.Intn(256))
	case reflect.TypeFor[int64]():
		return int64(rng.Intn(1000))
	case reflect.TypeFor[any]():
		return []Value{nil, 1, "s", true}[rng.Intn(4)]
	case st.storeT:
		if rng.Intn(4) == 0 {
			return nil // a nil struct value, which renders <nil>
		}
		return intoStore(&StructVal{Type: st, Vals: []Value{rng.Intn(9), "v"}}, st.storeT).Interface()
	}
	t.Fatalf("no sample for %v", vt)
	return nil
}

// The four ways fmt's answer comes from a MACHINE ADDRESS, which is the
// one thing no reproduction can predict. Each must DECLINE -- not guess
// and not approximate -- and the rendered result must still equal fmt's,
// because declining hands the map back to fmt itself.
//
// The assertion is on the decline, not only on the output: an
// implementation that stopped declining would still pass a parity check
// most of the time, since two addresses usually happen to be ordered the
// way the values are.
func TestAStructKeyedMapDeclinesWhereFmtUsesAnAddress(t *testing.T) {
	st := declare(t, `type P struct {
	A int
}`, "P")
	intT := reflect.TypeFor[int]()

	t.Run("a key type that is not a struct", func(t *testing.T) {
		m := reflect.ValueOf(map[string]int{"a": 1, "b": 2})
		if _, ok := appendStructKeyedMap(nil, m); ok {
			t.Error("took a map[string]int, whose keys it has no arena or comparator for")
		}
	})

	t.Run("two StructTypes in one map", func(t *testing.T) {
		// A re-declared P mints ONE key type -- minting interns on the
		// struct's shape -- but keeps a StructType per declaration. So a
		// map really can hold keys carrying different *StructTypes, and
		// fmt orders those two by the addresses of the StructTypes.
		other := declare(t, `type P struct {
	A int
}`, "P")
		if other == st {
			t.Fatal("the second declaration reused the first StructType; this test needs two")
		}
		if other.keyT != st.keyT {
			t.Fatal("the two declarations minted different key types; this test needs one map to hold both")
		}
		m := mapOfStructKeys(t, st, intT, [][]Value{{1}}, []Value{1})
		k, err := structKeyOf(&StructVal{Type: other, Vals: []Value{2}})
		if err != nil {
			t.Fatal(err)
		}
		m.SetMapIndex(intoKeyStore(k, other.keyT), reflect.ValueOf(2))
		if _, ok := appendStructKeyedMap(nil, m); ok {
			t.Error("ordered two keys whose *StructTypes differ, which fmt orders by address")
		}
		if got, want := string(appendValue(nil, m.Interface())), fmt.Sprintf("%v", m.Interface()); got != want {
			t.Errorf("after declining, got %q, fmt says %q", got, want)
		}
	})

	t.Run("one field holding two types", func(t *testing.T) {
		// Legal: a field is dynamically typed, so P{A: 1} and P{A: "1"}
		// are both keys of the same map. fmt orders them by the addresses
		// of `int` and `string`.
		m := mapOfStructKeys(t, st, intT, [][]Value{{1}, {"one"}}, []Value{1, 2})
		if _, ok := appendStructKeyedMap(nil, m); ok {
			t.Error("ordered an int against a string, which fmt orders by type address")
		}
		if got, want := string(appendValue(nil, m.Interface())), fmt.Sprintf("%v", m.Interface()); got != want {
			t.Errorf("after declining, got %q, fmt says %q", got, want)
		}
	})

	t.Run("a field holding a pointer", func(t *testing.T) {
		// A POINTER IS THE RESIDUE. keyCmp.rv orders every other shape a
		// field can hold -- a named int, a complex, an array, a native
		// struct -- so what is left declining is what fmt itself answers
		// with a machine address, and a pointer is the reachable one:
		// errors.New hands a script two of them.
		//
		// This subtest used to hold a uint, on the grounds that keyCmp
		// had no case for it. It has one now, by kind, so the uint moved
		// to TestAFieldOfANamedTypeOrdersByItsKind and the decline is
		// asserted where it is still true.
		a, b := new(int), new(int)
		m := mapOfStructKeys(t, st, intT, [][]Value{{a}, {b}}, []Value{1, 2})
		if _, ok := appendStructKeyedMap(nil, m); ok {
			t.Error("ordered two pointer fields, which fmt orders by address")
		}
		if got, want := string(appendValue(nil, m.Interface())), fmt.Sprintf("%v", m.Interface()); got != want {
			t.Errorf("after declining, got %q, fmt says %q", got, want)
		}
	})

	t.Run("a field holding a channel", func(t *testing.T) {
		// The other address kind, and the one fmtsort treats slightly
		// differently: it checks nil first, then compares addresses. Both
		// halves land on the same decline here.
		m := mapOfStructKeys(t, st, intT,
			[][]Value{{make(chan int)}, {make(chan int)}}, []Value{1, 2})
		if _, ok := appendStructKeyedMap(nil, m); ok {
			t.Error("ordered two channel fields, which fmt orders by address")
		}
	})
}

// appendMapValue is a SECOND way to render a value -- from a
// reflect.Value rather than from an interface -- and two renderers that
// disagree would put one spelling in a map and another everywhere else.
// So it is held against appendValue directly, which is a stronger
// equivalence than holding each against fmt separately.
//
// The named types are the reason it matches on TYPE and not on KIND:
// time.Duration and time.Month are Int-kinded and print through their
// String methods, so a kind switch would render them as bare numbers.
func TestMapValueRenderMatchesAppendValue(t *testing.T) {
	st := declare(t, `type P struct {
	A int
}`, "P")
	vals := []Value{
		"text", "", 0, 42, -7, int64(1 << 40), 'q', byte(200), 3.5, 0.1, true, false,
		float32(0.1),    // Float64's case must NOT widen this one
		3 * time.Second, // named int64 with a String method
		time.March,      // named int with a String method
		[]string{"a"},   // through appendValue's own slice case
		intoStore(&StructVal{Type: st, Vals: []Value{5}}, st.storeT).Interface(),
	}
	for _, v := range vals {
		rv := reflect.ValueOf(v)
		vr := mapValRender(rv.Type())
		got := string(appendMapValue(nil, rv, vr))
		// fromStore is what appendMapValue's general arm applies, so the
		// comparison has to start from the same value.
		want := string(appendValue(nil, fromStore(rv)))
		if got != want {
			t.Errorf("appendMapValue(%#v) = %q, appendValue says %q", v, got, want)
		}
		if want2 := fmt.Sprintf("%v", fromStore(rv)); got != want2 {
			t.Errorf("appendMapValue(%#v) = %q, fmt says %q", v, got, want2)
		}
	}
}

// The claim the fast path exists for. It is asserted as a COMPARISON with
// fmt rather than as a count, for the reason the arena tests give: a
// pinned number reports the compiler's inlining decisions, while "fewer
// than the thing we replaced" is the actual promise and cannot pass by
// accident -- fmt decodes and stringifies every key and boxes every
// value, which is six allocations an entry.
func TestRenderingAStructKeyedMapAllocatesLessThanFmt(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B string
}`, "P")
	// Both value shapes: a scalar, which appendMapValue renders straight
	// off the reflect.Value, and a minted struct, which it unwraps with
	// fromStore so the render is an appendTo rather than fmt calling the
	// carrier's String.
	shapes := []struct {
		name string
		vt   reflect.Type
		val  func(i int) Value
	}{
		{"map[P]int", reflect.TypeFor[int](), func(i int) Value { return i }},
		{"map[P]P", st.storeT, func(i int) Value {
			return intoStore(&StructVal{Type: st, Vals: []Value{i, "v"}}, st.storeT).Interface()
		}},
	}
	for _, sh := range shapes {
		for _, n := range []int{3, 16, 64} {
			t.Run(fmt.Sprintf("%s/n%d", sh.name, n), func(t *testing.T) {
				checkMapAllocs(t, st, sh.vt, n, sh.val)
			})
		}
	}
}

func checkMapAllocs(t *testing.T, st *StructType, vt reflect.Type, n int, val func(int) Value) {
	t.Helper()
	{
		keys := make([][]Value, n)
		vals := make([]Value, n)
		for i := range keys {
			keys[i] = []Value{i * 7 % n, "v"}
			vals[i] = val(i)
		}
		m := mapOfStructKeys(t, st, vt, keys, vals).Interface()
		buf := make([]byte, 0, 1<<16)
		fast := testing.AllocsPerRun(50, func() { buf = appendValue(buf[:0], m) })
		slow := testing.AllocsPerRun(50, func() { buf = fmt.Appendf(buf[:0], "%v", m) })
		if fast >= slow {
			t.Errorf("%d entries: the fast path allocated %.0f times, fmt %.0f -- "+
				"it is meant to decode into one arena and render values without boxing", n, fast, slow)
		}
		// The shape, not just the height: fmt's count grows by 6 an entry,
		// so a fast path still paying per-entry allocation for the KEY
		// decode alone must stay well under half.
		if fast > slow/2 {
			t.Errorf("%d entries: %.0f allocations against fmt's %.0f is more than half; "+
				"has the arena or the value renderer been lost?", n, fast, slow)
		}
	}
}

// The same claim from the outside, through the interpreter.
//
// IT PRINTS A STRUCT, NOT A MAP, and that is the reachability the whole
// fast path has: `fmt.Println(m)` on a bare map is handed to Go's fmt,
// which renders it itself and never asks grsh. appendValue sees a map
// only as a struct FIELD, reached through appendTo -- which is exactly
// what the open item this closes said. A test that printed the map
// directly would pass whatever this file did.
//
// The three shapes here are the ones a unit test builds least naturally:
// a nil key, a nested struct key, and struct values.
func TestAScriptPrintsAStructKeyedMapFieldLikeFmt(t *testing.T) {
	wantOut(t, `type Inner struct {
	X int
}
type P struct {
	A int
	B string
}
type Q struct {
	I Inner
	N int
}
type Holder struct {
	M  map[P]int
	PP map[P]P
	QQ map[Q]string
	E  map[P]int
}
m := map[P]int{}
m[P{A: 2, B: "b"}] = 2
m[P{A: 1, B: "a"}] = 1
m[nil] = 99
pp := map[P]P{}
pp[P{A: 1}] = P{A: 10}
pp[P{A: 2}] = P{A: 20}
qq := map[Q]string{}
qq[Q{I: Inner{X: 2}, N: 1}] = "b"
qq[Q{I: Inner{X: 1}, N: 9}] = "a"
fmt.Println(Holder{M: m, PP: pp, QQ: qq, E: map[P]int{}})
`,
		// A nil key sorts first: it encodes the zero StructKey, whose T is
		// the nil pointer, and no *StructType lives at address 0.
		"Holder{M: map[<nil>:99 P{A: 1, B: a}:1 P{A: 2, B: b}:2], "+
			"PP: map[P{A: 1, B: }:P{A: 10, B: } P{A: 2, B: }:P{A: 20, B: }], "+
			"QQ: map[Q{I: Inner{X: 1}, N: 9}:a Q{I: Inner{X: 2}, N: 1}:b], "+
			"E: map[]}\n")
}

// appendTo has to APPEND, which is the whole reason it exists: the
// text-order fallback in sortMapKeys renders a map's keys end to end into
// one slab, so a render that ignored the buffer it was handed would still
// pass every test that only ever calls String.
func TestAppendToExtendsItsBuffer(t *testing.T) {
	st := declare(t, `type P struct {
	A int
}`, "P")
	buf := []byte("before:")
	buf = (&StructVal{Type: st, Vals: []Value{1}}).appendTo(buf)
	buf = (&StructVal{Type: st, Vals: []Value{2}}).appendTo(buf)
	if got, want := string(buf), "before:P{A: 1}P{A: 2}"; got != want {
		t.Errorf("two renders into one buffer gave %q, want %q", got, want)
	}
}

// ---- ordering a whole map ----

// The claim sortMapKeys rests on is that its cost is PER MAP, not per
// key: one arena and one sort, where before the rewrite there were four
// allocations per key at one field and sixteen at ten.
//
// So the assertion is on the SHAPE, not on a number. Doubling the key
// count must not double the allocations -- it may add one or two, since
// the arena steps to a new chunk -- and anything per-key would show up
// here as a count that tracks nk. Pinning an exact count instead would report the
// compiler's inlining decisions, for the reasons written out in
// TestRangingAMapDecodesItsKeysIntoOneArena.
func TestOrderingAMapAllocatesPerMapNotPerKey(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
}`, "P")
	count := func(nk int) float64 {
		mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
		for i := 0; i < nk; i++ {
			k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{i, i * 10}})
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
		}
		keys := mp.MapKeys()
		var sink []*StructVal
		n := testing.AllocsPerRun(50, func() { sink = sortMapKeys(keys, nil) })
		if len(sink) != nk {
			t.Fatalf("ordering %d keys returned %d", nk, len(sink))
		}
		return n
	}
	small, large := count(16), count(32)
	// A per-key render would put the gap at 16 * (a render's allocations);
	// four is loose enough to absorb one more slab growth step and tight
	// enough that any per-key cost fails it.
	if gap := large - small; gap > 4 {
		t.Errorf("ordering 32 keys allocates %.0f times against %.0f for 16, a gap of %.0f: "+
			"the cost is tracking the key count, so something in the ordering pass is per-key again",
			large, small, gap)
	}
}

// The two slices sortMapKeys sorts have to move TOGETHER: keys is what
// the caller reads the map with, and decoded is what it hands the script
// as the range variable. A Swap that moved one and not the other would
// still produce sorted output -- and would pair every key with another
// key's value.
//
// Ranging the map through the interpreter is what makes that visible, so
// this drives a script rather than sortMapKeys directly.
func TestOrderedKeysStayPairedWithTheirValues(t *testing.T) {
	out, err := eval(t, `type P struct {
	N int
}
m := map[P]string{}
for i := 0; i < 40; i++ {
	m[P{N: i}] = fmt.Sprintf("v%d", i)
}
for k, v := range m {
	if v != fmt.Sprintf("v%d", k.N) {
		fmt.Println("MISPAIRED", k, v)
	}
}
fmt.Println("done")
`, nil)
	if err != nil {
		t.Fatalf("running: %v\n%s", err, out)
	}
	if strings.Contains(out, "MISPAIRED") {
		t.Errorf("a key was paired with another key's value:\n%s", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("the range did not finish:\n%s", out)
	}
}

// Forty keys is past every threshold in the ordering pass -- the one-key
// shortcut, the two-key arena bound, and the sizes a sort special-cases
// -- so this is the case that pins the ORDER itself.
//
// The order is fmt's, field by field, so P{N: 2} comes before P{N: 10}.
// It used to be the order of the RENDERED TEXT, which put 10 first
// because '0' < '}', and that is exactly what changed here.
//
// The second half is the whole reason it changed, and is the assertion
// that would survive a different order being chosen: the same script
// PRINTS the map, and the sequence the range visited has to be the
// sequence fmt printed. A bare map handed to fmt.Println never reaches
// grsh's renderer -- Go's fmt orders and prints it itself -- so that side
// of the comparison is the standard library's own answer, not a copy of
// the implementation under test.
func TestAMapRangesInFmtsOrder(t *testing.T) {
	out, err := eval(t, `type P struct {
	N int
}
m := map[P]int{}
for i := 0; i < 40; i++ {
	m[P{N: i}] = i
}
for k := range m {
	fmt.Print(k.N, " ")
}
fmt.Println()
fmt.Println(m)
`, nil)
	if err != nil {
		t.Fatalf("running: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a ranged line and a printed line, got:\n%s", out)
	}
	// Numeric order, spelled out rather than computed, so the test says
	// what the answer is instead of re-deriving it.
	want := make([]string, 40)
	for i := range want {
		want[i] = strconv.Itoa(i)
	}
	if got := strings.Fields(lines[0]); !slices.Equal(got, want) {
		t.Errorf("a 40-key map ranged as\n  %s\nwant\n  %s", strings.Join(got, " "), strings.Join(want, " "))
	}
	// What fmt itself printed, read back out of the map text. The values
	// are dropped: only the order of the keys is being compared.
	var printed []string
	for _, m := range regexp.MustCompile(`P\{N: (\d+)\}`).FindAllStringSubmatch(lines[1], -1) {
		printed = append(printed, m[1])
	}
	if !slices.Equal(printed, want) {
		t.Fatalf("fmt printed the same map as\n  %s\nwant\n  %s -- the test's own expectation is wrong, not the range",
			strings.Join(printed, " "), strings.Join(want, " "))
	}
	if got := strings.Fields(lines[0]); !slices.Equal(got, printed) {
		t.Errorf("the range visited\n  %s\nbut fmt printed the same map as\n  %s",
			strings.Join(got, " "), strings.Join(printed, " "))
	}
}

// A map keyCmp declines falls back to the rendered-text order, and this
// is the case that proves the fallback runs rather than being dead code.
//
// The two orders DISAGREE on the keys chosen, which is what makes the
// assertion meaningful: field-wise puts P{N: 2} first, text puts
// P{N: 10} first because '0' < '}'. Asserting the text order therefore
// fails if the decline is missed, and fails if the fallback is skipped.
//
// The decline itself is the *StructType one: a re-declared P mints one
// key type but keeps a StructType per declaration, so one map can hold
// keys from both, and fmt orders those by the addresses of the two
// StructTypes -- an answer that changes between runs and that nothing
// here can or should reproduce.
func TestADeclinedMapRangesInRenderedTextOrder(t *testing.T) {
	st := declare(t, `type P struct {
	N int
}`, "P")
	other := declare(t, `type P struct {
	N int
}`, "P")
	if other == st || other.keyT != st.keyT {
		t.Fatal("this test needs two StructTypes sharing one minted key type")
	}
	// Forty keys, alternating between the two declarations, for the
	// reason the two-key version of this test was not enough: a sort of
	// two elements makes one comparison and then stops, so it cannot tell
	// an ordering that keeps its slices in step from one that does not.
	const nk = 40
	m := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	for i := 0; i < nk; i++ {
		owner := st
		if i%2 == 1 {
			owner = other
		}
		k, err := structKeyOf(&StructVal{Type: owner, Vals: []Value{i}})
		if err != nil {
			t.Fatal(err)
		}
		m.SetMapIndex(intoKeyStore(k, owner.keyT), reflect.ValueOf(i))
	}
	// Both declarations render as "P{N: i}", so the expectation is the
	// plain text order and says out loud what the fallback is: P{N: 10}
	// before P{N: 2}, which is where it differs from the field-wise
	// answer this map cannot have.
	want := make([]string, nk)
	for i := range want {
		want[i] = fmt.Sprintf("P{N: %d}", i)
	}
	sort.Strings(want)

	// Ten runs, each starting from whatever permutation MapKeys hands
	// back, because DETERMINISM is what the fallback exists for: fmt's
	// own order for this map is an address and is not reproducible, so
	// the only property left to hold is that the script sees the same
	// order every time.
	for run := 0; run < 10; run++ {
		keys := m.MapKeys()
		decoded := sortMapKeys(keys, nil)
		got := make([]string, len(decoded))
		for i, sv := range decoded {
			got[i] = sv.String()
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d ranged as\n  %s\nwant\n  %s -- the text-order fallback did not run",
				run, strings.Join(got, " "), strings.Join(want, " "))
		}
		// The keys have to have moved with them: the caller reads each
		// entry's VALUE through keys[i], and the value here is the field.
		for i, k := range keys {
			if got, want := fromStore(m.MapIndex(k)), decoded[i].Vals[0]; got != want {
				t.Fatalf("run %d, slot %d: key %v carries value %v, want %v -- "+
					"the fallback sort moved the text and left the keys behind", run, i, decoded[i], got, want)
			}
		}
	}
}

// THE TEST THE PROJECTION RESTS ON: a projected sort and the general
// field-wise sort must produce the SAME permutation, and reach the same
// verdict on declining.
//
// projectedOrder claims to be a speed choice and nothing else. That claim
// is not observable from the outside -- both paths order the same maps
// the same way when they agree, so a projection that got a pair wrong
// would show up only as a range that disagreed with fmt somewhere in the
// middle of a large map. Running BOTH sorters over one map and comparing
// slot by slot is what turns it into an assertion.
//
// The equality is exact rather than approximate, and it is exact even for
// a map that DECLINES: the two Less functions agree pointwise under
// projectedOrderOf's preconditions, so one sort algorithm driven by them
// makes the same decisions in the same order and lands on the same
// permutation, meaningless order included.
//
// FIELD 0 DRAWS FROM A SMALL SET so ties are common: a tie is what sends
// a comparison down to the general comparator, and a test whose keys all
// differ in field 0 would never take that path. FIELD 1 IS MIXED-TYPE on
// purpose -- that is the decline the projection has to leave findable.
func TestAProjectedOrderIsTheGeneralOrder(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B string
}`, "P")
	// One bucket per trial. The last is mixed and must be REFUSED, which
	// is the other half of what is being tested: a projection taken where
	// fmt orders by the address of a type would be silently wrong.
	firsts := [][]Value{
		{0, 1, 2, -1, 1 << 40},
		{int64(0), int64(-3), int64(1 << 41)},
		{'a', 'b', 'c'},
		{byte(0), byte(9), byte(200)},
		{"", "a", "zz"},
		{0, "a", 'b', byte(9), int64(3), true, 0.5, nil},
	}
	seconds := []Value{nil, 0, 1, "a", "b", true, 2.5}
	const seed = 20260829
	rng := rand.New(rand.NewSource(seed))
	var taken, refused, declined, tied int
	for _, n := range []int{8, 9, 40} {
		for b, first := range firsts {
			for trial := 0; trial < 40; trial++ {
				keys := make([][]Value, n)
				vals := make([]Value, n)
				for i := range keys {
					if rng.Intn(16) == 0 {
						// The nil key, which sorts before every other and
						// has no field 0 to project.
						keys[i] = nil
					} else {
						keys[i] = []Value{first[rng.Intn(len(first))], seconds[rng.Intn(len(seconds))]}
					}
					vals[i] = rng.Intn(1000)
				}
				m := mapOfStructKeys(t, st, reflect.TypeFor[int](), keys, vals)
				mk := m.MapKeys()
				if len(mk) < projectMinKeys {
					continue // deduplicated below the threshold
				}

				// Two independent copies: same map, same keys, one sorted
				// each way. Each gets its own arena, so neither can be
				// reading structs the other permuted.
				pp := &keyOrder{keys: slices.Clone(mk), decoded: decodeForOrder(t, st, mk)}
				gp := &keyOrder{keys: slices.Clone(mk), decoded: decodeForOrder(t, st, mk)}
				var pc, gc keyCmp
				s := projectedOrderOf(pp, &pc)
				if s == nil {
					refused++
					continue
				}
				if b == len(firsts)-1 {
					t.Fatalf("n=%d trial %d: the mixed-type bucket projected, "+
						"which is the case fmt answers with a type's address", n, trial)
				}
				taken++
				if firstFieldsRepeat(pp.decoded) {
					tied++
				}
				sort.Sort(s)
				sort.Sort(&fieldOrder{keyOrder: gp, cmp: &gc})
				if pc.declined {
					declined++
				}
				if pc.declined != gc.declined {
					t.Fatalf("n=%d trial %d: the projected sort declined=%v, the general one declined=%v",
						n, trial, pc.declined, gc.declined)
				}
				for i := range pp.decoded {
					if pp.keys[i].Interface() != gp.keys[i].Interface() {
						t.Fatalf("n=%d trial %d, slot %d: projected %s, general %s -- "+
							"the projection is supposed to change speed and nothing else",
							n, trial, i, pp.decoded[i], gp.decoded[i])
					}
				}
			}
		}
	}
	// Every arm counted, because each one is a way this test could be
	// asserting nothing: no projection taken, no map that ties in field 0
	// and therefore never reaches the general comparator underneath, no
	// decline, or nothing refused.
	if taken == 0 || refused == 0 || declined == 0 || tied == 0 {
		t.Errorf("%d projected, %d refused, %d declined, %d with a repeated first field; "+
			"every arm has to be reached or this test is asserting less than it looks",
			taken, refused, declined, tied)
	}
}

// decodeForOrder decodes a map's keys the way sortMapKeys does, so a test
// can drive the two sorters directly instead of through the function that
// chooses between them.
func decodeForOrder(t *testing.T, st *StructType, keys []reflect.Value) []*StructVal {
	t.Helper()
	dec := make([]*StructVal, len(keys))
	arena := newKeyArena(st, len(keys))
	for i, k := range keys {
		dec[i] = decodeMintedKey(k, arena)
	}
	return dec
}

// firstFieldsRepeat reports whether two keys share a field 0, which is
// what makes a projected comparison fall through to the general one.
func firstFieldsRepeat(decoded []*StructVal) bool {
	seen := make(map[Value]bool, len(decoded))
	for _, sv := range decoded {
		if sv == nil || len(sv.Vals) == 0 {
			continue
		}
		if seen[sv.Vals[0]] {
			return true
		}
		seen[sv.Vals[0]] = true
	}
	return false
}

// A TIE IN THE PROJECTED FIELD MUST FALL THROUGH to the fields behind it.
//
// The projection answers only when field 0 differs; every other pair has
// to reach keyCmp, which is what orders those. A Less that returned its
// projection's verdict unconditionally would leave this map in an order
// the sort itself calls sorted and that nothing else agrees with.
//
// A takes TWO values rather than one, which is what makes this a test of
// the projection at all: projectedBy refuses a field that separates
// nothing, so a constant A would send the whole map down the general path
// and this would test it twice. Two values keep the projection and still
// tie four keys in five.
func TestAProjectedTieOrdersOnTheFieldsBehindIt(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B string
}`, "P")
	// Past projectMinKeys, or the projection is never built.
	names := []string{"m", "d", "k", "a", "z", "q", "b", "f", "t", "c"}
	m := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	firstOf := func(i int) int {
		if i%5 == 0 {
			return 9
		}
		return 7
	}
	for i, s := range names {
		k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{firstOf(i), s}})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		m.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
	}
	// A ascending first, and B within each run of it.
	var sevens, nines []string
	for i, s := range names {
		if firstOf(i) == 7 {
			sevens = append(sevens, fmt.Sprintf("P{A: 7, B: %s}", s))
		} else {
			nines = append(nines, fmt.Sprintf("P{A: 9, B: %s}", s))
		}
	}
	sort.Strings(sevens)
	sort.Strings(nines)
	want := append(sevens, nines...)

	mk := m.MapKeys()
	// Said out loud, because everything below is about a path this test
	// would otherwise only be assumed to take.
	if projectedOrderOf(&keyOrder{keys: mk, decoded: decodeForOrder(t, st, mk)}, &keyCmp{}) == nil {
		t.Fatal("these keys do not project, so the tie this test is about is never reached")
	}
	decoded := sortMapKeys(mk, nil)
	got := make([]string, len(decoded))
	for i, sv := range decoded {
		got[i] = sv.String()
	}
	if !slices.Equal(got, want) {
		t.Errorf("keys sharing a first field ordered as\n  %s\nwant\n  %s",
			strings.Join(got, " "), strings.Join(want, " "))
	}
}

// A FIRST FIELD THAT SEPARATES NOTHING REFUSES THE PROJECTION, and the
// map still orders. The refusal is a speed choice -- every comparison
// would tie and pay for the array twice over -- so what has to be pinned
// is that it costs no order: these keys are settled entirely by B.
func TestAConstantFirstFieldRefusesTheProjection(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B string
}`, "P")
	names := []string{"m", "d", "k", "a", "z", "q", "b", "f", "t", "c"}
	m := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	for i, s := range names {
		k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{7, s}})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		m.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
	}
	mk := m.MapKeys()
	if s := projectedOrderOf(&keyOrder{keys: mk, decoded: decodeForOrder(t, st, mk)}, &keyCmp{}); s != nil {
		t.Error("a constant first field projected, which can only cost time: " +
			"no comparison is settled by it and every one of them pays for the array")
	}
	want := make([]string, len(names))
	for i, s := range slices.Sorted(slices.Values(names)) {
		want[i] = fmt.Sprintf("P{A: 7, B: %s}", s)
	}
	decoded := sortMapKeys(mk, nil)
	got := make([]string, len(decoded))
	for i, sv := range decoded {
		got[i] = sv.String()
	}
	if !slices.Equal(got, want) {
		t.Errorf("ordered as\n  %s\nwant\n  %s", strings.Join(got, " "), strings.Join(want, " "))
	}
}

// A FIELD 0 OF TWO DYNAMIC TYPES MUST REFUSE THE PROJECTION and still
// decline, which is the failure mode a projection introduces: it skips
// the comparator on every pair field 0 settles, so a projection built
// over a field fmt orders by a TYPE'S ADDRESS would hand back a confident
// order for a map that has none.
//
// The assertion is the text-order fallback, which is only reachable
// through a decline -- and the two orders disagree on these keys, so it
// fails if the decline is missed rather than passing on a coincidence.
func TestAMixedFirstFieldRefusesTheProjectionAndDeclines(t *testing.T) {
	st := declare(t, `type P struct {
	A int
}`, "P")
	const nk = 40
	m := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	want := make([]string, 0, nk)
	for i := 0; i < nk; i++ {
		// Half ints and half strings in one field. fmt orders that pair
		// by which of int and string was linked at the lower address.
		var f Value = i
		if i%2 == 1 {
			f = strconv.Itoa(i)
		}
		k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{f}})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		m.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
		want = append(want, fmt.Sprintf("P{A: %v}", f))
	}
	// The rendered text of an int field and of a string field are the
	// same bytes here, which is why the fallback can order them at all.
	sort.Strings(want)

	// Ten runs from ten permutations, because determinism is the whole
	// property a declined map still has.
	for run := 0; run < 10; run++ {
		decoded := sortMapKeys(m.MapKeys(), nil)
		got := make([]string, len(decoded))
		for i, sv := range decoded {
			got[i] = sv.String()
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d ordered as\n  %s\nwant\n  %s -- a mixed first field was not declined",
				run, strings.Join(got, " "), strings.Join(want, " "))
		}
	}
}

// CROSSING projectMinKeys MUST NOT CHANGE THE ANSWER. The threshold is a
// measurement, so it is free to move; what it may never do is move an
// order with it, and a map of seven keys and a map of eight take
// different paths through sortMapKeys for no reason a script can see.
func TestTheProjectionThresholdChangesNoOrder(t *testing.T) {
	st := declare(t, `type P struct {
	A int
}`, "P")
	for _, nk := range []int{projectMinKeys - 2, projectMinKeys - 1, projectMinKeys, projectMinKeys + 1} {
		m := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
		want := make([]string, nk)
		for i := 0; i < nk; i++ {
			// Starting at 8 straddles a digit boundary, so the field-wise
			// order and the rendered-text order DISAGREE -- text puts 10
			// before 8 -- and an order that came from the wrong path is
			// visible here rather than hidden behind a coincidence.
			k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{i + 8}})
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			m.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
			want[i] = fmt.Sprintf("P{A: %d}", i+8)
		}
		decoded := sortMapKeys(m.MapKeys(), nil)
		got := make([]string, len(decoded))
		for i, sv := range decoded {
			got[i] = sv.String()
		}
		if !slices.Equal(got, want) {
			t.Errorf("%d keys ordered as\n  %s\nwant\n  %s", nk,
				strings.Join(got, " "), strings.Join(want, " "))
		}
	}
}

// EVERY SCALAR KEY KIND, against fmt, over randomised maps.
//
// A range used to leave all of these in the map's own randomised order --
// only string and struct keys were ordered at all -- so `for k := range m`
// over a map[int]V visited its entries differently on every run while
// fmt.Println(m) printed them in numeric order. This is the test that
// says the two agree now.
//
// The comparison is the whole map text rebuilt out of the order the RANGE
// would visit, held against what fmt prints for the same map, which is
// the same shape TestARangeVisitsAStructKeyedMapInFmtsOrder uses and for
// the same reason: fmt's map printing is the fixed point, and rebuilding
// its text is what turns "the same order" into a byte comparison.
//
// The kinds run past what a script can spell -- typeIdents has no uint32
// -- because a map handed back by a stdlib call is ranged by the same
// function, and fmtsort orders those too.
func TestAScalarKeyedMapRangesInFmtsOrder(t *testing.T) {
	const seed = 20260829
	rng := rand.New(rand.NewSource(seed))
	cases := []struct {
		name string
		kt   reflect.Type
		key  func() reflect.Value
	}{
		{"int", reflect.TypeFor[int](), func() reflect.Value {
			return reflect.ValueOf(rng.Intn(200) - 100)
		}},
		{"int64", reflect.TypeFor[int64](), func() reflect.Value {
			return reflect.ValueOf(int64(rng.Intn(200) - 100))
		}},
		{"rune", reflect.TypeFor[rune](), func() reflect.Value {
			return reflect.ValueOf(rune('a' + rng.Intn(26)))
		}},
		{"byte", reflect.TypeFor[byte](), func() reflect.Value {
			return reflect.ValueOf(byte(rng.Intn(256)))
		}},
		{"uint32", reflect.TypeFor[uint32](), func() reflect.Value {
			// Past 1<<31, where a signed comparison of the same bits
			// would order them the other way round.
			return reflect.ValueOf(uint32(rng.Uint32()))
		}},
		{"float64", reflect.TypeFor[float64](), func() reflect.Value {
			// NaN sorts below every other float, under fmtsort and here,
			// and is the only tie two distinct scalar keys can produce.
			if rng.Intn(12) == 0 {
				return reflect.ValueOf(math.NaN())
			}
			return reflect.ValueOf(float64(rng.Intn(400)-200) / 4)
		}},
		{"bool", reflect.TypeFor[bool](), func() reflect.Value {
			return reflect.ValueOf(rng.Intn(2) == 0)
		}},
		{"string", reflect.TypeFor[string](), func() reflect.Value {
			return reflect.ValueOf([]string{"", "a", "b", "zz", "A", "10", "2"}[rng.Intn(7)])
		}},
		{"any of one type", reflect.TypeFor[any](), func() reflect.Value {
			// One dynamic type throughout: fmtsort's type-by-address step
			// ties, so it goes on to the values and IS reproducible.
			return reflect.ValueOf(any(rng.Intn(50) - 25))
		}},
	}
	intT := reflect.TypeFor[int]()
	for _, c := range cases {
		for _, n := range []int{2, 3, 7, 40} {
			for trial := 0; trial < 20; trial++ {
				m := reflect.MakeMap(reflect.MapOf(c.kt, intT))
				for i := 0; i < n; i++ {
					k := reflect.New(c.kt).Elem()
					k.Set(c.key())
					// EVERY VALUE IS 1, and that is forced by the NaN
					// key: NaN does not equal itself, so MapIndex cannot
					// find one and a distinct value per entry could not
					// be read back to rebuild the text. Nothing is lost
					// -- a scalar key is not paired with a decoded slice
					// the way a struct key is, so there is no pairing
					// here for a distinct value to catch.
					m.SetMapIndex(k, reflect.ValueOf(1))
				}
				keys := m.MapKeys()
				if decoded := sortMapKeys(keys, nil); decoded != nil {
					t.Fatalf("%s: a scalar key was decoded; the caller reads the map's own keys", c.name)
				}
				b := []byte("map[")
				for i, k := range keys {
					if i > 0 {
						b = append(b, ' ')
					}
					b = appendValue(b, k.Interface())
					b = append(b, ':', '1')
				}
				b = append(b, ']')
				if got, want := string(b), fmt.Sprintf("%v", m.Interface()); got != want {
					t.Fatalf("seed %d, %s keys, n=%d, trial %d:\n ranged %s\n fmt    %s",
						seed, c.name, n, trial, got, want)
				}
			}
		}
	}
}

// A map[any]V holding two different dynamic types is fmt's address rule
// again, one level up from a struct key's field, and it declines for the
// same reason: which of int and string was linked at the lower address is
// not something to reproduce.
//
// So the assertion is DETERMINISM, and specifically the rendered-text
// order -- "1" before "3" before "a" -- which is neither fmt's answer nor
// a numeric one, and so could only come from the fallback having run.
func TestAnInterfaceKeyedMapDeclinesToTextOrder(t *testing.T) {
	m := reflect.ValueOf(map[any]int{3: 1, "b": 2, 1: 3, "a": 4, true: 5})
	want := []string{"1", "3", "a", "b", "true"}
	for run := 0; run < 10; run++ {
		keys := m.MapKeys()
		if decoded := sortMapKeys(keys, nil); decoded != nil {
			t.Fatal("an interface key was decoded")
		}
		got := make([]string, len(keys))
		for i, k := range keys {
			got[i] = string(appendValue(nil, k.Interface()))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d ranged as %v, want %v -- the text-order fallback did not run", run, got, want)
		}
	}
}

// The script-level half of the scalar story: an int-keyed map ranged and
// printed by the same script, which is how a user meets the disagreement
// and the only place the whole path -- rangeOver, sortMapKeys, and Go's
// own fmt on a bare map -- is exercised together.
func TestAScriptRangesAnIntKeyedMapLikeFmtPrintsIt(t *testing.T) {
	out, err := eval(t, `m := map[int]string{}
for i := -3; i < 12; i++ {
	m[i] = "v"
}
for k := range m {
	fmt.Print(k, " ")
}
fmt.Println()
fmt.Println(m)
`, nil)
	if err != nil {
		t.Fatalf("running: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a ranged line and a printed line, got:\n%s", out)
	}
	want := make([]string, 0, 15)
	for i := -3; i < 12; i++ {
		want = append(want, strconv.Itoa(i))
	}
	if got := strings.Fields(lines[0]); !slices.Equal(got, want) {
		t.Errorf("ranged\n  %s\nwant\n  %s", strings.Join(got, " "), strings.Join(want, " "))
	}
	// What fmt itself printed, read back out of the map text.
	var printed []string
	for _, mm := range regexp.MustCompile(`(-?\d+):v`).FindAllStringSubmatch(lines[1], -1) {
		printed = append(printed, mm[1])
	}
	if got := strings.Fields(lines[0]); !slices.Equal(got, printed) {
		t.Errorf("the range visited\n  %s\nbut fmt printed the same map as\n  %s",
			strings.Join(got, " "), strings.Join(printed, " "))
	}
}

// String keys take their own branch -- no decode, no render, and the keys
// sorted in place -- so they need their own order test. The map is big
// enough to be past anything a small-n shortcut could cover.
func TestAStringKeyedMapRangesInOrder(t *testing.T) {
	out, err := eval(t, `m := map[string]int{}
for i := 0; i < 40; i++ {
	m[fmt.Sprintf("k%d", i)] = i
}
for k := range m {
	fmt.Print(k, " ")
}
fmt.Println()
`, nil)
	if err != nil {
		t.Fatalf("running: %v\n%s", err, out)
	}
	want := make([]string, 40)
	for i := range want {
		want[i] = fmt.Sprintf("k%d", i)
	}
	sort.Strings(want)
	if got := strings.TrimSpace(out); got != strings.Join(want, " ") {
		t.Errorf("a 40-key string map ranged as\n  %s\nwant\n  %s", got, strings.Join(want, " "))
	}
}

// ---- a key no lookup can find ----

// THE CRASH THIS PINS. A range used to walk a map's keys and fetch each
// value with rv.MapIndex(k), which fails on the one key that is not equal
// to itself: a NaN is a live entry no lookup can ever find, MapIndex
// handed back the zero Value, and the range died reading it --
//
//	grsh: grsh internal error: reflect: call of reflect.Value.Interface on zero Value
//
// -- for a bare float key, for a map[any]V boxing one, and for a script
// struct with a float field, which are the three ways a NaN reaches a map
// key. All three run here, each ranged AND printed, because the fix must
// not cost the order the previous two sessions bought.
func TestAScriptRangesAMapWithANaNKey(t *testing.T) {
	for _, c := range []struct {
		name string
		src  string
		want []string
	}{{
		name: "float key",
		// fmt sorts NaN below every other float, and so does the range.
		src:  `m := map[float64]string{1.5: "a", 3: "b", math.NaN(): "n", -2: "c"}`,
		want: []string{"NaN=n", "-2=c", "1.5=a", "3=b"},
	}, {
		name: "interface key",
		// Every key is a float64, so the interface order is the float
		// order rather than a decline.
		src:  `m := map[any]string{1.5: "a", math.NaN(): "n", -2.0: "c"}`,
		want: []string{"NaN=n", "-2=c", "1.5=a"},
	}, {
		name: "struct key",
		// The minted key holds its fields as `any`, so the NaN is two
		// levels down and the whole struct-key path -- decode, arena,
		// field-wise order -- runs over it.
		//
		// The literals are written 1 and -2 rather than 1.0 and -2.0
		// deliberately, and this is the case that used to have to spell
		// them out: `P{X: 1}` stored an int in a float64 field, so the
		// field held two dynamic types across the keys and the order
		// declined to the text fallback. coerceField converts at the
		// slot now, so the bare integers reach the field as float64 and
		// the field-wise order runs.
		src: `type P struct {
	X float64
}
m := map[P]string{P{X: 1}: "a", P{X: math.NaN()}: "n", P{X: -2}: "c"}`,
		want: []string{"P{X: NaN}=n", "P{X: -2}=c", "P{X: 1}=a"},
	}} {
		t.Run(c.name, func(t *testing.T) {
			// The script builds and ranges the map five times over, from
			// five fresh maps, because the order the sort starts from is
			// the map's own randomised one -- a pairing bug that leaves
			// a small map already correct would otherwise pass here as
			// often as it fails.
			out, err := eval(t, `for run := 0; run < 5; run++ {
`+c.src+`
for k, v := range m {
	fmt.Print(k, "=", v, " ")
}
fmt.Println()
}
`, nil)
			if err != nil {
				t.Fatalf("running: %v\n%s", err, out)
			}
			// Compared line by line rather than field by field: a struct
			// key renders with a space inside it (`P{X: 1}`), so
			// splitting on whitespace would cut the keys in half and
			// compare two wrong lists that print identically in a
			// failure message.
			want := strings.Join(c.want, " ")
			for run, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if got := strings.TrimSpace(line); got != want {
					t.Errorf("run %d ranged\n  %s\nwant\n  %s", run, got, want)
				}
			}
		})
	}
}

// Every value must still land beside the key it belongs to, which is the
// thing the fix could get wrong: keys and values are now two slices, and
// a sort that moved one without the other would produce a perfectly
// ordered map of mismatched pairs.
//
// EVERY VALUE HERE IS ITS OWN KEY'S TEXT, so the pairing is checkable
// without a lookup -- which is the point, since a NaN key is exactly the
// key no lookup can find. Each map is built and ranged ten times, because
// the permutation the sort starts from is the map's own randomised one
// and a swap bug need not show on the first.
//
// The cases cover all three sorters that carry values: scalarOrder for a
// float and an accepted interface key, textOrder for a declined one, and
// keyOrder for a struct key.
func TestARangedMapPairsEveryValueWithItsOwnKey(t *testing.T) {
	st := declare(t, `type P struct {
	X float64
	Y int
}`, "P")
	nan := math.NaN()

	// build makes one map[K]string whose every value is fmt's text for
	// its key, from the keys given.
	build := func(kt reflect.Type, keys []any) reflect.Value {
		m := reflect.MakeMap(reflect.MapOf(kt, reflect.TypeFor[string]()))
		for _, k := range keys {
			kv := reflect.ValueOf(k)
			if kt.Kind() == reflect.Interface {
				// A map[any]string wants the key boxed, not converted.
				kv = reflect.ValueOf(&k).Elem()
			}
			m.SetMapIndex(kv, reflect.ValueOf(fmt.Sprint(k)))
		}
		return m
	}
	// structMap is build for a P key: the key is minted, and its text is
	// the promoted String the minted type carries.
	structMap := func(fields [][]Value) reflect.Value {
		m := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[string]()))
		for _, f := range fields {
			k, err := structKeyOf(&StructVal{Type: st, Vals: f})
			if err != nil {
				t.Fatalf("encoding key %v: %v", f, err)
			}
			kv := intoKeyStore(k, st.keyT)
			m.SetMapIndex(kv, reflect.ValueOf(fmt.Sprint(kv.Interface())))
		}
		return m
	}

	cases := []struct {
		name string
		m    reflect.Value
	}{
		{"float with a NaN", build(reflect.TypeFor[float64](), []any{1.5, -2.0, nan, 0.0, 99.25})},
		{"int, the unpaired path", build(reflect.TypeFor[int](), []any{3, -1, 0, 77, 12})},
		{"string, the unpaired path", build(reflect.TypeFor[string](), []any{"b", "a", "zz", "", "m"})},
		// One dynamic type throughout: the interface order is taken.
		{"interface, accepted", build(reflect.TypeFor[any](), []any{1.5, -2.0, nan, 7.0})},
		// int and string together: fmt orders those by which type was
		// linked lower, so keyCmp declines and the text order runs.
		{"interface, declined to text", build(reflect.TypeFor[any](), []any{1, "a", 2.5, nan, "z"})},
		{"struct with a NaN field", structMap([][]Value{{1.0, 2}, {nan, 3}, {-4.0, 5}, {1.0, 9}})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for run := 0; run < 10; run++ {
				keys, vals := mapKeysAndVals(c.m)
				sortMapKeys(keys, vals)
				for i, k := range keys {
					want := fmt.Sprint(k.Interface())
					// Read the value exactly as rangeOver does, which is
					// what makes the two unpaired rows worth having: they
					// assert the MapIndex route still pairs correctly and
					// that mapKeysAndVals really did leave them on it.
					v := c.m.MapIndex(k)
					if vals != nil {
						v = vals[i]
					}
					if got := v.String(); got != want {
						t.Fatalf("run %d, slot %d: key %s carries value %q, want %q "+
							"-- the sort moved keys without their values",
							run, i, want, got, want)
					}
				}
			}
		})
	}
}

// mayNotEqualItself decides which maps pay for the paired pass, and it is
// pinned by type rather than by behaviour because a wrong NO is a crash
// while a wrong YES is only slower -- the two failures are not symmetric,
// and only a table makes the NO side visible.
func TestMayNotEqualItselfNamesTheKeysMapIndexCannotFind(t *testing.T) {
	st := declare(t, `type P struct {
	X int
}`, "P")
	type plainStruct struct {
		A int
		B string
	}
	type floatStruct struct {
		A int
		B float32
	}
	type nested struct{ In floatStruct }
	cases := []struct {
		t    reflect.Type
		want bool
	}{
		// The two commonest maps a script writes, and the reason the
		// question is asked at all: neither pays anything.
		{reflect.TypeFor[string](), false},
		{reflect.TypeFor[int](), false},
		{reflect.TypeFor[uint64](), false},
		{reflect.TypeFor[bool](), false},
		{reflect.TypeFor[rune](), false},
		{reflect.TypeFor[*int](), false},
		{reflect.TypeFor[chan int](), false},
		{reflect.TypeFor[plainStruct](), false},
		{reflect.TypeFor[[4]int](), false},
		// A NaN lives in any of these.
		{reflect.TypeFor[float32](), true},
		{reflect.TypeFor[float64](), true},
		{reflect.TypeFor[complex128](), true},
		{reflect.TypeFor[any](), true},
		{reflect.TypeFor[error](), true},
		{reflect.TypeFor[floatStruct](), true},
		{reflect.TypeFor[nested](), true},
		{reflect.TypeFor[[4]float64](), true},
		// A MINTED KEY IS ALWAYS A YES, even for a struct of one int
		// field: the carrier holds the script's fields as `any`, and the
		// type cannot say what went into them. This is the conservative
		// arm mapKeysAndVals documents, and it is here so that a future
		// change of storage that made it a NO would have to be a
		// deliberate one.
		{st.keyT, true},
	}
	for _, c := range cases {
		if got := mayNotEqualItself(c.t); got != c.want {
			t.Errorf("mayNotEqualItself(%s) = %v, want %v", c.t, got, c.want)
		}
	}
}

// ---- the key kinds that used to have no order ----

// THE TEST THAT SAYS WHICH KEY KINDS GET AN ORDER, and it is written as a
// table because the interesting half is the NOs.
//
// A wrong YES here is a script seeing an order that changes between runs
// while looking stable in testing; a wrong NO is the disagreement with
// fmt this whole path exists to remove. Neither is visible from a test
// that only checks the kinds it happens to think of, so every kind a Go
// map key can have is listed, and the three that are left unordered are
// listed as such rather than omitted.
//
// The YES arm is checked by ordering a deliberately reversed slice and
// requiring fmt's order out. The NO arm is checked by requiring the same
// reversed slice back UNTOUCHED -- not merely "some order", which a sort
// that silently ran would also satisfy.
func TestWhichKeyKindsGetAnOrder(t *testing.T) {
	type native struct {
		A int
		B string
	}
	// Each case's keys are given in DESCENDING fmt order, so an ordered
	// kind must reverse them and an unordered one must not move them.
	cases := []struct {
		name    string
		ordered bool
		keys    []any
	}{
		{"int", true, []any{2, 1, -3}},
		{"int64", true, []any{int64(2), int64(1), int64(-3)}},
		{"uint32", true, []any{uint32(1 << 31), uint32(9), uint32(2)}},
		{"float64", true, []any{2.5, 1.0, math.NaN()}},
		{"string", true, []any{"b", "aa", "a"}},
		{"bool", true, []any{true, false}},
		{"rune", true, []any{'z', 'b', 'a'}},
		// NAMED types, whose Kind is what fmtsort switches on. A
		// time.Duration ordered by its rendered text would put 1h before
		// 1ms; this is the case a script actually reaches, through a
		// map[any]V key.
		{"time.Duration", true, []any{time.Hour, time.Second, time.Millisecond}},
		{"time.Month", true, []any{time.December, time.March, time.January}},
		// The three kinds this session added. fmtsort orders a complex by
		// real then imag, and an array and a struct element by element.
		{"complex128", true, []any{complex(2, 0), complex(1, 5), complex(1, 2)}},
		{"[3]int", true, []any{[3]int{2, 0, 0}, [3]int{1, 9, 9}, [3]int{1, 2, 3}}},
		{"native struct", true, []any{
			native{2, "a"}, native{1, "b"}, native{1, "a"},
		}},
		{"[2]any of one type", true, []any{[2]any{2, 0}, [2]any{1, 9}, [2]any{1, 2}}},
		{"[2]any holding a nil", true, []any{[2]any{1, 0}, [2]any{nil, 9}, [2]any{nil, 2}}},
		// THE RESIDUE. fmt orders these three by a machine address, so
		// its own order changes between runs and there is nothing to
		// reproduce -- and unlike every other decline, the rendered text
		// is no fallback either, because the text of a pointer IS the
		// address. They keep the map's own order.
		{"pointer", false, []any{new(int), new(int), new(int)}},
		{"chan", false, []any{make(chan int), make(chan int), make(chan int)}},
		{"unsafe.Pointer", false, []any{
			unsafe.Pointer(new(int)), unsafe.Pointer(new(int)), unsafe.Pointer(new(int)),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kt := reflect.TypeOf(c.keys[0])
			given := make([]reflect.Value, len(c.keys))
			for i, k := range c.keys {
				given[i] = reflect.ValueOf(k)
			}
			keys := slices.Clone(given)
			if decoded := sortMapKeys(keys, nil); decoded != nil {
				t.Fatalf("a %s key was decoded; only a minted struct key is", kt)
			}
			if !c.ordered {
				// Identity, element by element: a pointer's own text is
				// not stable enough to compare, and the claim is that
				// nothing moved rather than that something equal is here.
				for i := range keys {
					if keys[i].Pointer() != given[i].Pointer() {
						t.Fatalf("%s keys were reordered; fmt orders them by address, so there is no order to reproduce", kt)
					}
				}
				return
			}
			// fmt's order, read off fmt itself: a map of these keys
			// printed by fmt names the order fmtsort chose.
			m := reflect.MakeMap(reflect.MapOf(kt, reflect.TypeFor[int]()))
			for _, k := range given {
				m.SetMapIndex(k, reflect.ValueOf(1))
			}
			b := []byte("map[")
			for i, k := range keys {
				if i > 0 {
					b = append(b, ' ')
				}
				b = appendValue(b, k.Interface())
				b = append(b, ':', '1')
			}
			b = append(b, ']')
			if got, want := string(b), fmt.Sprintf("%v", m.Interface()); got != want {
				t.Fatalf("ranged  %s\nfmt     %s", got, want)
			}
		})
	}
}

// keyCmp.rv mirrors internal/fmtsort's compare, an unexported standard
// library detail, so the mirror is held against fmt itself rather than
// against a hand-written expectation -- the same discipline
// TestAStructKeyedMapMatchesFmt applies to the struct-key half.
//
// It compares PAIRS rather than whole maps, which is what makes it a test
// of the comparator instead of a second test of the sort: every pair of
// values in the pool goes into a two-entry map, and fmt's printing of
// that map says which of the two fmtsort put first.
//
// The pool is deliberately one type per case, because a map holding two
// dynamic types is the decline this cannot check against fmt -- fmt's
// answer there is a type address. Those are counted, required to occur,
// and checked only for having declined.
func TestKeyCmpMirrorsFmtOverEveryComparableKind(t *testing.T) {
	type inner struct {
		N int
		S string
	}
	type outer struct {
		I inner
		F float64
		A [2]int
	}
	pool := []any{
		0, 1, -1, 1 << 40,
		int64(0), int64(-1 << 41),
		// ^uint(0) has its top bit set, so an Int() comparison of the same
		// bits would sort it FIRST instead of last. uint64 needs a
		// same-typed companion of its own: a pool entry only ever meets
		// values of its own type here.
		uint(0), uint(1), ^uint(0), uint64(1), uint64(1 << 63),
		byte(0), byte(255), 'a', 'b',
		0.0, -0.0, 1.5, -1.5, math.Inf(1), math.Inf(-1), math.NaN(),
		float32(1.5), float32(-1.5),
		complex(0.0, 0.0), complex(1.0, 2.0), complex(1.0, -2.0), complex(2.0, 0.0),
		complex64(complex(1, 2)),
		"", "a", "b", "10", "2",
		true, false,
		time.Duration(0), time.Second, time.Hour, time.January, time.December,
		[3]int{0, 0, 0}, [3]int{1, 2, 3}, [3]int{1, 2, 4}, [3]int{2, 0, 0},
		inner{1, "a"}, inner{1, "b"}, inner{2, "a"},
		outer{inner{1, "a"}, 0.5, [2]int{1, 2}},
		outer{inner{1, "a"}, 0.5, [2]int{1, 3}},
		outer{inner{1, "a"}, 1.5, [2]int{1, 2}},
		[2]any{1, "a"}, [2]any{1, "b"}, [2]any{2, "a"},
		// A NIL INSIDE a composite, which is the only way rv's interface
		// case is reached at all: a bare interface KEY goes through
		// keyCmp.field, which answers for nil itself. fmtsort's nilCompare
		// puts these two below every non-nil element, and it settles the
		// pair before the dynamic types are ever compared -- so these
		// order against the int-first entries above rather than declining.
		[2]any{nil, 1}, [2]any{nil, 2},
	}
	intT := reflect.TypeFor[int]()
	var same, declined int
	for i, a := range pool {
		for j, b := range pool {
			if i == j {
				continue
			}
			av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
			var c keyCmp
			got := c.rv(av, bv)
			if av.Type() != bv.Type() {
				if !c.declined {
					t.Fatalf("%T against %T: two dynamic types is fmt's address rule and must decline", a, b)
				}
				declined++
				continue
			}
			if c.declined {
				t.Fatalf("%T: declined on a pair of its own type", a)
			}
			same++
			// fmt's verdict, read off a two-entry map. Two keys that
			// compare EQUAL -- two NaNs, and 0.0 against -0.0 -- are
			// still distinct entries whose relative order fmt does not
			// fix either, so those are checked for equality and no more.
			if got == 0 {
				if r := c.rv(bv, av); r != 0 {
					t.Fatalf("%v against %v: equal one way and %d the other", a, b, r)
				}
				continue
			}
			m := reflect.MakeMap(reflect.MapOf(av.Type(), intT))
			m.SetMapIndex(av, reflect.ValueOf(1))
			m.SetMapIndex(bv, reflect.ValueOf(2))
			if m.Len() != 2 {
				// The two are == to each other despite comparing
				// non-zero, which nothing in the pool should manage.
				t.Fatalf("%v and %v are one map entry but compare %d", a, b, got)
			}
			text := fmt.Sprintf("%v", m.Interface())
			first := strings.HasPrefix(text, "map["+fmt.Sprintf("%v", a)+":1 ")
			if wantFirst := got < 0; first != wantFirst {
				t.Fatalf("rv(%v, %v) = %d, but fmt printed %s", a, b, got, text)
			}
		}
	}
	if same == 0 || declined == 0 {
		t.Fatalf("the pool exercised %d same-type pairs and %d cross-type ones; both arms are needed", same, declined)
	}
}

// A NAMED type is the reachable half of this session's change, and it is
// reachable through the one door a script has to these kinds: a map[any]V
// key boxes whatever a stdlib call handed back.
//
// keyCmp.field matches on GO TYPE, so `case int64` does not catch a
// time.Duration and the map declined to the rendered-text order -- which
// puts 1h0m0s before 1ms, because 'h' < 'm'. fmt switches on KIND and
// puts 1ms first. The two disagreed, visibly, in a script anyone could
// write.
func TestAFieldOfANamedTypeOrdersByItsKind(t *testing.T) {
	t.Run("as a map[any] key", func(t *testing.T) {
		m := reflect.ValueOf(map[any]int{
			time.Hour: 1, time.Second: 2, time.Millisecond: 3, time.Minute: 4,
		})
		want := "map[1ms:3 1s:2 1m0s:4 1h0m0s:1]"
		if got := fmt.Sprintf("%v", m.Interface()); got != want {
			t.Fatalf("fmt itself prints %s, so this test's premise is stale", got)
		}
		for run := 0; run < 10; run++ {
			keys, vals := mapKeysAndVals(m)
			sortMapKeys(keys, vals)
			b := []byte("map[")
			for i, k := range keys {
				if i > 0 {
					b = append(b, ' ')
				}
				b = appendValue(b, k.Interface())
				b = append(b, ':')
				b = appendValue(b, vals[i].Interface())
			}
			if got := string(append(b, ']')); got != want {
				t.Fatalf("run %d ranged %s, want %s", run, got, want)
			}
		}
	})

	t.Run("as a struct key's field", func(t *testing.T) {
		// The same question one level down: keyCmp.field is what orders a
		// script struct's fields, and a field takes whatever a stdlib
		// call handed back. uint is here because it is what this used to
		// decline on -- see TestAStructKeyedMapDeclinesWhereFmtUsesAnAddress.
		st := declare(t, `type P struct {
	A int
}`, "P")
		intT := reflect.TypeFor[int]()
		for _, fields := range [][][]Value{
			{{time.Hour}, {time.Second}, {time.Millisecond}},
			{{uint(3)}, {uint(1)}, {uint(2)}},
			{{time.December}, {time.January}},
		} {
			vals := make([]Value, len(fields))
			for i := range vals {
				vals[i] = i
			}
			m := mapOfStructKeys(t, st, intT, fields, vals)
			got, ok := appendStructKeyedMap(nil, m)
			if !ok {
				t.Fatalf("%v: declined a field type fmtsort orders by kind", fields)
			}
			if want := fmt.Sprintf("%v", m.Interface()); string(got) != want {
				t.Fatalf("%v:\n got %s\nwant %s", fields, got, want)
			}
		}
	})
}

// THE DECLINE IS FOUND BY THE SORT, one level further down than
// sortMapKeys makes that argument: a composite key is compared element by
// element and stops at the first element that differs, so a pointer in
// the LAST field costs nothing on any pair an earlier field settles.
//
// time.Time is the case that matters, because it is what a script gets
// from time.Now: three unexported fields, wall, ext and a *Location. A
// pre-pass over the type would see the pointer and give up on every
// time-keyed map; comparing as it goes gives up only on two times that
// are identical in wall and ext, which differ only by location.
func TestATimeKeyedMapOrdersOnTheFieldsBeforeItsPointer(t *testing.T) {
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	m := reflect.ValueOf(map[any]int{
		base.Add(2 * time.Hour): 2,
		base:                    0,
		base.Add(time.Hour):     1,
	})
	want := fmt.Sprintf("%v", m.Interface())
	for run := 0; run < 10; run++ {
		keys, vals := mapKeysAndVals(m)
		sortMapKeys(keys, vals)
		b := []byte("map[")
		for i, k := range keys {
			if i > 0 {
				b = append(b, ' ')
			}
			b = appendValue(b, k.Interface())
			b = append(b, ':')
			b = appendValue(b, vals[i].Interface())
		}
		if got := string(append(b, ']')); got != want {
			t.Fatalf("run %d ranged %s\n            fmt %s", run, got, want)
		}
	}
	// THE OTHER HALF OF THE CLAIM: a pair that DOES reach the pointer
	// declines rather than inventing an answer. In changes only loc, so
	// wall and ext tie and loc is the field that decides -- which is
	// exactly the pair a field-wise comparison cannot answer.
	//
	// FixedZone rather than LoadLocation: this must not depend on a
	// zoneinfo database being installed.
	other := base.In(time.FixedZone("elsewhere", 0))
	if base.Location() == other.Location() {
		t.Fatal("In kept the same *Location; this test needs two Times differing only in location")
	}
	var c keyCmp
	if r := c.rv(reflect.ValueOf(base), reflect.ValueOf(other)); !c.declined {
		t.Fatalf("two Times differing only in *Location compared %d; fmt answers that by address", r)
	}
}

// A COMPOSITE KEY CAN DECLINE, and when it does the answer has to be
// deterministic rather than fmt's. An array of `any` holding two
// different dynamic types is the reachable shape: fmt orders those two by
// the addresses of `int` and `string`, which changes between builds, so
// the fallback renders the keys and orders by the text.
//
// The expected order -- "[1 a]" before "[a 1]" -- is the text one, which
// is neither numeric nor fmt's, so it could only come from the fallback
// having run.
func TestACompositeKeyedMapDeclinesToTextOrder(t *testing.T) {
	m := reflect.ValueOf(map[[2]any]int{
		{"a", 1}: 1,
		{1, "a"}: 2,
		{1, 2}:   3,
	})
	want := []string{"[1 2]", "[1 a]", "[a 1]"}
	for run := 0; run < 10; run++ {
		keys, vals := mapKeysAndVals(m)
		if decoded := sortMapKeys(keys, vals); decoded != nil {
			t.Fatal("an array key was decoded")
		}
		got := make([]string, len(keys))
		for i, k := range keys {
			got[i] = string(appendValue(nil, k.Interface()))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("run %d ranged as %v, want %v -- the text-order fallback did not run", run, got, want)
		}
	}
}

// The script-level half, and the only end-to-end reach a script has to
// any of this: map[any]V, keyed by values a registered stdlib call handed
// back. A range and a print of the same map, in the same script, which is
// where a user meets a disagreement between them.
func TestAScriptRangesAMapKeyedByNamedTypes(t *testing.T) {
	out, err := eval(t, `m := map[any]int{}
m[time.Hour] = 1
m[time.Second] = 2
m[time.Millisecond] = 3
m[time.Minute] = 4
fmt.Println(m)
for k, v := range m {
	fmt.Println(k, v)
}`, nil)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	want := "map[1ms:3 1s:2 1m0s:4 1h0m0s:1]\n1ms 3\n1s 2\n1m0s 4\n1h0m0s 1\n"
	if out != want {
		t.Errorf("got:\n%swant:\n%s", out, want)
	}
}

// A NESTED STRUCT FIELD ORDERS BY ITS OWN FIELDS, which keyCmp.field has
// always done -- and which the reflect fallback added beneath it could
// silently take away, because a *StructVal is a POINTER and rv orders
// pointers by declining. The case order inside field is therefore
// load-bearing in a way it was not before, and nothing pinned it: no test
// built a map whose key had a struct field and then asked what ORDER it
// came out in. A probe pass found the gap; this closes it.
//
// The keys are chosen to tell the two answers apart rather than merely to
// be true of one. In{N: 10} against In{N: 2} is where field order and
// TEXT order disagree -- '0' < '}' puts 10 first as text, comparing N
// puts 2 first -- so a decline would leave the keys the other way round
// and be caught, where equal-length numbers would pass either way.
func TestANestedStructKeyFieldOrdersFieldWise(t *testing.T) {
	in, _, err := evalKeep(t, `type In struct {
	N int
}
type P struct {
	A In
}`, nil)
	if err != nil {
		t.Fatalf("declaring: %v", err)
	}
	get := func(name string) *StructType {
		t.Helper()
		v, ok := in.globals.Get(name)
		if !ok {
			t.Fatalf("%s was not defined", name)
		}
		return v.(*StructType)
	}
	inT, pT := get("In"), get("P")

	fields := make([][]Value, 0, 3)
	for _, n := range []int{10, 2, 1} {
		fields = append(fields, []Value{&StructVal{Type: inT, Vals: []Value{n}}})
	}
	m := mapOfStructKeys(t, pT, reflect.TypeFor[int](), fields, []Value{1, 2, 3})

	got, ok := appendStructKeyedMap(nil, m)
	if !ok {
		t.Fatal("declined a key whose field is a nested struct; fmt orders those by the nested fields")
	}
	// fmt's own answer, which for this map is reproducible: the two keys
	// carry the same *StructType, so fmtsort's address step ties and it
	// goes on to the fields.
	if want := fmt.Sprintf("%v", m.Interface()); string(got) != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// And the answer is the FIELD order, not the text order the fallback
	// would have produced.
	if want := "map[P{A: In{N: 1}}:3 P{A: In{N: 2}}:2 P{A: In{N: 10}}:1]"; string(got) != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

// ---- the retention cap ----

// keyChunkFor is arithmetic with two ends that are easy to get wrong and
// impossible to notice: a zero divisor for a fieldless struct, and a
// quotient that rounds to zero for a struct wider than the whole cap.
// Both would be a panic or an infinite carve of empty chunks rather than
// a wrong answer, so they are pinned here rather than left to a script to
// find.
func TestKeyChunkForCoversEveryArity(t *testing.T) {
	const many = 1 << 20
	cases := []struct{ nf, left, want int }{
		{0, many, keyChunkVals / svSlots},         // type P struct{}
		{1, many, keyChunkVals / (1 + svSlots)},   // the common case
		{10, many, keyChunkVals / (10 + svSlots)}, //
		{keyChunkVals, many, 1},                   // exactly as wide as the cap
		{keyChunkVals * 4, many, 1},               // wider than the cap: one key per chunk
		{2, 3, 3},                                 // fewer keys left than fit
		{2, 0, 0},                                 // nothing left to carve
	}
	for _, c := range cases {
		if got := keyChunkFor(c.nf, c.left); got != c.want {
			t.Errorf("keyChunkFor(nf=%d, left=%d) = %d, want %d", c.nf, c.left, got, c.want)
		}
	}
	// The bound the constant is FOR: whatever the arity, one chunk holds
	// about the same number of slots, counting each key's header. The
	// slack is the rounding in the division, which is a whole key's worth
	// at wide arities.
	for _, nf := range []int{0, 1, 2, 5, 10, 40, 200} {
		slots := keyChunkFor(nf, 1<<20) * (nf + svSlots)
		if slots > keyChunkVals || slots < keyChunkVals-(nf+svSlots) {
			t.Errorf("a chunk of %d-field keys holds %d slots, want within one key of %d -- "+
				"the retention bound is not uniform across arities", nf, slots, keyChunkVals)
		}
	}
}

// THE CAP'S ONLY OBSERVABLE EFFECT is that a big map is served by several
// slabs instead of one, and that is exactly what bounds retention: a key
// that outlives its loop pins the chunk it was carved from, so as long as
// there is more than one chunk, one key cannot pin the whole map.
//
// The assertion is therefore on the allocation count, which is the only
// place the chunking surfaces -- two per chunk, ceil(n/chunk) chunks.
// Nothing a script can read changes, which is why this test exists at
// all.
func TestABigMapsKeysComeFromSeveralChunks(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
}`, "P")
	per := keyChunkFor(len(st.Fields), 1<<20)
	// Three chunks and a bit, so the count catches both an off-by-one in
	// the refill and a cap that quietly stopped applying.
	nk := per*3 + 1
	mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	for i := 0; i < nk; i++ {
		k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{i, i * 10}})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
	}
	keys := mp.MapKeys()

	var sink *StructVal
	got := testing.AllocsPerRun(20, func() {
		a := newKeyArena(st, len(keys))
		for _, k := range keys {
			sink = decodeMintedKey(k, a)
		}
	})
	want := float64(2 * 4) // four chunks, two slabs each
	if got != want {
		t.Errorf("decoding %d keys (%d per chunk) allocates %.0f times, want %.0f -- "+
			"two slabs for each of four chunks", nk, per, got, want)
	}
	if sink == nil || len(sink.Vals) != 2 {
		t.Fatalf("the last decoded key is %v, want a two-field P", sink)
	}
	// Without the cap this is 2 for any n, which is the leak the cap
	// exists to close: one retained key would hold all nk StructVals.
	if want <= 2 {
		t.Fatal("the test is not crossing a chunk boundary, so it proves nothing")
	}
}

// Keys carved from DIFFERENT chunks must be as independent as keys carved
// from the same one. TestArenaKeysDoNotShareFields makes the point within
// a chunk; the hazard here is the other one -- a refill that reused the
// old slab, or advanced the wrong slice -- which no small map can reach.
func TestKeysFromDifferentChunksDoNotShareFields(t *testing.T) {
	st := declare(t, `type P struct {
	A int
	B int
}`, "P")
	per := keyChunkFor(len(st.Fields), 1<<20)
	nk := per + 2 // just over the boundary: the last two are a new chunk
	mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	for i := 0; i < nk; i++ {
		k, err := structKeyOf(&StructVal{Type: st, Vals: []Value{i, i * 10}})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(i))
	}
	keys := mp.MapKeys()

	a := newKeyArena(st, len(keys))
	svs := make([]*StructVal, len(keys))
	want := make([]string, len(keys))
	for i, k := range keys {
		svs[i] = decodeMintedKey(k, a)
		want[i] = svs[i].String()
	}
	// Only the keys either side of the boundary are swept against every
	// other key: the within-chunk case is already covered, and sweeping
	// all of them against all of them is quadratic in a chunk.
	for _, i := range []int{per - 1, per, per + 1} {
		for f := range svs[i].Vals {
			orig := svs[i].Vals[f]
			svs[i].Vals[f] = "sentinel"
			for j := range svs {
				if j == i {
					continue
				}
				if got := svs[j].String(); got != want[j] {
					t.Fatalf("writing field %d of key %d (chunk boundary at %d) changed key %d to %s, want %s: "+
						"a refill handed out memory another chunk was already using", f, i, per, j, got, want[j])
				}
			}
			svs[i].Vals[f] = orig
		}
	}
}

// sliceCasesOf reads the named function out of structs.go and returns the
// element type names of its `case []T:` arms.
//
// A test that reads its own package's source is unusual enough to say
// why. What appendValue's slice cases buy is speed, and speed is only
// sometimes observable: two of the nine -- []byte and []error -- render
// identically and allocate identically whether their case is present or
// removed, because fmt has its own fast path for one and the other
// already carries its interface. Behaviour cannot tell those cases from
// their absence. The source can, and a case nothing can falsify is
// indistinguishable from a case nobody wrote.
//
// It reads the file rather than a string constant so it cannot drift, and
// it recognises only `[]Ident`, which is exactly the shape typeIdents
// names produce -- a case for `[][]string` or `[]P` is not a native
// element type and is deliberately not reported.
func sliceCasesOf(t *testing.T, fn string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "structs.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("reading structs.go to check %s's cases: %v", fn, err)
	}
	var decl *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == fn {
			decl = fd
			break
		}
	}
	if decl == nil {
		t.Fatalf("structs.go has no func %s -- has it been renamed?", fn)
	}
	var names []string
	ast.Inspect(decl, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			at, ok := e.(*ast.ArrayType)
			if !ok || at.Len != nil {
				continue
			}
			if id, ok := at.Elt.(*ast.Ident); ok {
				names = append(names, id.Name)
			}
		}
		return true
	})
	if len(names) == 0 {
		t.Fatalf("%s has no `case []T:` arms at all -- either they are gone or this "+
			"helper no longer recognises their shape", fn)
	}
	return names
}

// TestAFieldHoldsItsDeclaredType is the core of the conversion: whatever
// a script writes into a field, the field holds its OWN type afterwards.
//
// Every case asserts %T as well as the value, because the value alone
// cannot see the defect -- `1` and `1.0` both PRINT as 1, and the whole
// bug was invisible until something asked what type was actually stored.
// That is also why it went unnoticed: it only surfaced through == and
// through a map key's ordering, two places where the dynamic type decides
// the answer and never appears in it.
func TestAFieldHoldsItsDeclaredType(t *testing.T) {
	for _, c := range []struct {
		name, src, want string
	}{{
		name: "keyed literal",
		src:  `p := P{X: 1}`,
		want: "float64 1",
	}, {
		name: "positional literal",
		src:  `p := P{1}`,
		want: "float64 1",
	}, {
		name: "field assignment",
		// The literal was the reported site; the assignment is the same
		// defect one function over. Without it a field would hold a
		// float64 or an int depending on which statement wrote it.
		src: `var p P
p.X = 1`,
		want: "float64 1",
	}, {
		name: "compound assignment",
		src: `p := P{X: 1}
p.X += 1`,
		want: "float64 2",
	}, {
		name: "elided literal in a slice",
		// []P{{X: 1}} reaches structComposite through elidedElem rather
		// than through evalComposite, so it is a separate route into the
		// same two lines.
		src:  `p := []P{{X: 1}}[0]`,
		want: "float64 1",
	}, {
		name: "the zero already agreed",
		// The control. An untouched field was ALWAYS its declared type --
		// that is what made the bug so odd to look at: `var p P` gave a
		// float64 and `P{X: 1}` gave an int, for the same field.
		src:  `var p P`,
		want: "float64 0",
	}, {
		name: "a float constant into an int field",
		// The other direction, and the reason the fix is a conversion
		// rather than a check: Go accepts P{X: 1.0} for an int X because
		// the constant is representable. grsh cannot tell that constant
		// from a float64 variable, so it converts both -- which means
		// P{X: 1.5} truncates here where Go would refuse it. That
		// looseness is convertTo's, inherited unchanged from what
		// []int{1.5} has always done, not something coerceField adds.
		src:  `type P struct { X int }; p := P{X: 1.0}`,
		want: "int 1",
	}, {
		name: "an interface field takes the value as spelled",
		// A field declared `any` has no type to convert TO, so the
		// dynamic type is the one the script wrote. Converting here
		// would be actively wrong.
		src:  `type P struct { X any }; p := P{X: 1}`,
		want: "int 1",
	}} {
		t.Run(c.name, func(t *testing.T) {
			src := c.src
			if !strings.Contains(src, "type P struct") {
				src = "type P struct {\n\tX float64\n}\n" + src
			}
			wantOut(t, src+"\nfmt.Printf(\"%T %v\\n\", p.X, p.X)", c.want+"\n")
		})
	}
}

// TestAKeyedLiteralCoercesTheFieldItNames separates the two indexes a
// keyed element carries.
//
// `P{X: 1, S: "a"}` walks the literal's elements in the order WRITTEN,
// so the loop counter i is the element's position and idx is the field's
// -- and they disagree the moment a literal names its fields out of
// declaration order. Coercing against the wrong one is silent when the
// two fields happen to share a type, so the struct here is deliberately
// mixed: a float64 and a string, written in the other order.
func TestAKeyedLiteralCoercesTheFieldItNames(t *testing.T) {
	wantOut(t, `type P struct {
	S string
	X float64
}
p := P{X: 1, S: "a"}
fmt.Printf("%T %v %T %v\n", p.X, p.X, p.S, p.S)`, "float64 1 string a\n")
}

// TestAFieldOfAnUnmodelledTypeStaysDynamic pins the escape hatch
// declareType deliberately leaves open, because coerceField is exactly
// the thing that could close it by accident.
//
// A field whose type grsh cannot resolve -- a self-referential one is the
// reachable case -- records a zero TypeDesc with a nil RT, and the
// declaration comment says such a field "simply starts nil and works".
// Coercing against a nil type would be a nil dereference at best and a
// blanket refusal at worst, so the nil RT has to short-circuit.
func TestAFieldOfAnUnmodelledTypeStaysDynamic(t *testing.T) {
	wantOut(t, `type N struct {
	Next N
}
var n N
n.Next = 5
fmt.Println(n.Next)
n.Next = "anything"
fmt.Println(n.Next)`, "5\nanything\n")
}

// TestNilIsStillWritableIntoAnyField pins the second short-circuit, and
// it is a decision rather than an oversight.
//
// nil is the one value with a meaning at every type, and coerceField
// could plausibly convert it the way a container slot does -- `xs[0] =
// nil` on a []float64 stores 0. It does not, for the case that has no
// good answer: a struct-TYPED field. Go rejects `p.I = nil` outright, so
// there is nothing to match; converting would have to invent either a
// typed nil *StructVal or a freshly allocated zero struct, neither of
// which the script wrote. Leaving nil alone keeps a value the
// interpreter already stores everywhere storable, and costs nothing.
func TestNilIsStillWritableIntoAnyField(t *testing.T) {
	wantOut(t, `type In struct {
	N int
}
type P struct {
	Xs []int
	A  any
	I  In
}
var p P
p.Xs = nil
p.A = nil
p.I = nil
fmt.Println(p.Xs == nil, p.A == nil, len(p.Xs))
q := P{Xs: nil, A: nil, I: nil}
fmt.Println(q.Xs == nil, q.A == nil)`, "true true 0\ntrue true\n")
}

// TestTwoStructsAreEqualHoweverTheirFieldsWereSpelled is the first of the
// two places the untyped-constant gap was actually VISIBLE.
//
// structEqual compares field-wise, so two fields holding int(1) and
// float64(1) made two structs that print identically compare unequal --
// and the script has no way to see why, since %v shows 1 on both sides.
func TestTwoStructsAreEqualHoweverTheirFieldsWereSpelled(t *testing.T) {
	wantOut(t, `type P struct {
	X float64
	Y int
}
a := P{X: 1, Y: 2}
b := P{X: 1.0, Y: 2.0}
fmt.Println(a == b)
fmt.Println(P{X: 1} == P{X: 1.0000001})`, "true\nfalse\n")
}

// TestAStructKeyedMapOrdersFieldsWrittenAsBareIntegers is the second, and
// it is the symptom the Open item was filed under.
//
// keyCmp declines to order a field that holds two dynamic types across
// the keys, and falls back to comparing the RENDERED text. The values are
// 1, 2 and 10 precisely because those two orders disagree: field-wise
// gives 1 2 10, text gives 1 10 2. A test using 1 and -2 would pass under
// the bug.
func TestAStructKeyedMapOrdersFieldsWrittenAsBareIntegers(t *testing.T) {
	wantOut(t, `type P struct {
	X float64
}
m := map[P]string{P{X: 10}: "c", P{X: 1}: "a", P{X: 2}: "b"}
for k, v := range m {
	fmt.Print(k, "=", v, " ")
}
fmt.Println()`, "P{X: 1}=a P{X: 2}=b P{X: 10}=c \n")
}

// TestAFieldRefusesAValueItsTypeCannotHold covers what the conversion
// turns into an error rather than a silent store.
//
// The struct case is the one that needed code of its own: a field whose
// type is a script struct has structValType as its RT, every struct
// erases to that, so convertTo alone would wave a Q into an In field.
func TestAFieldRefusesAValueItsTypeCannotHold(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{{
		name: "a string into a float64 field",
		src:  `type P struct { X float64 }; _ = P{X: "s"}`,
		want: "cannot use s (string) as float64",
	}, {
		name: "an int into a string field",
		// convertTo refuses this specially rather than converting: Go's
		// string(65) is "A", which is never what a script meant here.
		src:  `type P struct { X string }; _ = P{X: 1}`,
		want: "(use strconv.Itoa)",
	}, {
		name: "another struct into a struct-typed field",
		src: `type In struct { N int }
type Q struct { N int }
type Out struct { I In }
_ = Out{I: Q{N: 1}}`,
		want: "cannot use Q{N: 1} (Q) as In",
	}, {
		name: "a non-struct into a struct-typed field",
		src: `type In struct { N int }
type Out struct { I In }
_ = Out{I: 7}`,
		want: "cannot use 7 (int) as In",
	}, {
		name: "at an assignment, not only a literal",
		src: `type P struct { X float64 }
var p P
p.X = "s"`,
		want: "cannot use s (string) as float64",
	}} {
		t.Run(c.name, func(t *testing.T) { wantErr(t, c.src, c.want) })
	}
}

// TestARefusedFieldWriteRecordsWhichField checks the breadcrumb rather
// than the message, because that is where it lives.
//
// The user-facing one-liner is "loc: message" (see runner.UserMessage),
// and the loc already pins the exact value that was refused -- naming the
// field in the text too would be noise at the prompt. The serr attribute
// is the same shape callReflect attaches for a bad argument, and it is
// what --debug prints.
func TestARefusedFieldWriteRecordsWhichField(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{"literal", `type P struct { X float64 }; _ = P{X: "s"}`, "P.X"},
		{"positional literal", `type P struct { X float64 }; _ = P{"s"}`, "P.X"},
		{"assignment", `type P struct { X float64 }
var p P
p.X = "s"`, "P.X"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := eval(t, c.src, nil)
			if err == nil {
				t.Fatalf("expected an error, got none\nsource:\n%s", c.src)
			}
			got, ok := serr.WrapAsSErr(err).GetAttribute("field")
			if !ok {
				t.Fatalf("no \"field\" attribute on %v", err)
			}
			if got != c.want {
				t.Errorf("field attribute = %v, want %q", got, c.want)
			}
		})
	}
}

// TestTwoDeclarationsOfAStructShareAFieldSlot pins coerceField against
// the interning rule in store.go rather than against StructType identity.
//
// Redeclaring P in an inner scope makes a SECOND *StructType with the
// same shape, and the two share a minted storage type. Comparing the
// *StructType pointers would refuse this, and would disagree with what a
// []P slot already accepts -- one rule, checked the same way in both
// places.
func TestTwoDeclarationsOfAStructShareAFieldSlot(t *testing.T) {
	wantOut(t, `type In struct {
	N int
}
type Out struct {
	I In
}
mk := func() any {
	type In struct {
		N int
	}
	return In{N: 7}
}
var o Out
o.I = mk()
fmt.Println(o.I.N)`, "7\n")
}
