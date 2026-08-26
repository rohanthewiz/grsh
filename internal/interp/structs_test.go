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
