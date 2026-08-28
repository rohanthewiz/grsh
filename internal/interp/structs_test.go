package interp

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unsafe"
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
		if got := decodeKeyArr(k.T, reflect.ValueOf(k.F)); got.String() != sv.String() {
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
