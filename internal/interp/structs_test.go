package interp

import (
	"strings"
	"testing"
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

// A script struct is refused as a map KEY. The erasure makes it a
// pointer, and a pointer IS comparable -- so reflect.MapOf would build
// the map happily and then compare identities, making every lookup with
// a freshly built key miss. A non-comparable key type used to reach
// reflect.MapOf and surface as an unpositioned internal error; it is a
// positioned script error now.
func TestMapKeyTypesRefused(t *testing.T) {
	wantErr(t, `type P struct {
	X int
}
m := map[P]int{}
_ = m`, "script struct cannot be a map key")
	wantErr(t, `m := map[[]int]int{}
_ = m`, "invalid map key type []int")
}

// KNOWN DIVERGENCE, pinned.
//
// A missing entry in a struct-valued map yields nil, where Go yields the
// zero struct. The element type has erased to *StructVal, which cannot
// say WHICH struct, so reflect.Zero would hand back a typed nil that
// panics on the first field access. Untyped nil is the honest answer,
// and the comma-ok form is exact either way.
func TestStructMapMissYieldsNil(t *testing.T) {
	wantOut(t, `type P struct {
	X int
}
m := map[string]P{"a": {1}}
v, ok := m["nope"]
fmt.Println(m["nope"] == nil, v, ok)`, "true <nil> false\n")
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
	// The divergence, stated as a test so it cannot change silently.
	wantOut(t, `type P struct {
	X int
}
type Q struct {
	X int
}
var v any = []P{{1}}
_, ok := v.([]Q)
fmt.Println(ok)`, "true\n")
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
