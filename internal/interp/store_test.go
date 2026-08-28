package interp

import (
	"fmt"
	"reflect"
	"testing"
)

// declare runs a `type` declaration through the interpreter and returns
// the StructType it defined, which is the only way to get one with every
// field (sig and storeT included) filled in the way the real path fills
// them.
func declare(t *testing.T, src, name string) *StructType {
	t.Helper()
	in, _, err := evalKeep(t, src, nil)
	if err != nil {
		t.Fatalf("declaring %s: %v", name, err)
	}
	v, ok := in.globals.Get(name)
	if !ok {
		t.Fatalf("%s was not defined", name)
	}
	st, ok := v.(*StructType)
	if !ok {
		t.Fatalf("%s is %T, want *StructType", name, v)
	}
	return st
}

// THE CANARY for the constraint ScriptStruct exists to satisfy.
//
// reflect.StructOf's method promotion breaks if the embedded type has an
// UNEXPORTED method, and it breaks by killing the process:
//
//	fatal error: runtime: type offset base pointer out of range
//
// raised from itabInit the first time the value is asserted to an
// interface — which fmt does to every argument it is handed. There is no
// recover for that, so it must never ship.
//
// The method count catches the cause before the crash catches the effect:
// promotion pulls the unexported method in too, so a second method here
// means someone gave ScriptStruct — or something it embeds — a method it
// should not have. The formatting check below is the effect itself, and
// would take the test binary down with it rather than fail.
func TestMintedTypePromotesExactlyOneMethod(t *testing.T) {
	st := declare(t, "type P struct {\n\tX int\n}", "P")
	mt := st.storeT

	if got := mt.NumMethod(); got != 1 {
		t.Fatalf("minted type has %d methods, want exactly 1 (String).\n"+
			"An unexported method on ScriptStruct or *StructVal breaks "+
			"reflect.StructOf promotion and crashes the process from "+
			"itabInit -- see the warning on ScriptStruct.", got)
	}
	if !mt.Implements(reflect.TypeFor[fmt.Stringer]()) {
		t.Fatal("minted type is not a fmt.Stringer; a []P would print as grsh's storage")
	}
	if got, want := mt.Size(), reflect.TypeFor[*StructVal]().Size(); got != want {
		t.Errorf("minted type is %d bytes, want %d: the storage must stay one pointer wide", got, want)
	}

	// The effect. A container of minted values has to survive the trip
	// through fmt, which is where the broken promotion actually detonates.
	sl := reflect.MakeSlice(reflect.SliceOf(mt), 0, 1)
	sl = reflect.Append(sl, intoStore(st.newZero(), mt))
	if got := fmt.Sprint(sl.Interface()); got != "[P{X: 0}]" {
		t.Errorf("a []P renders as %q, want [P{X: 0}]", got)
	}
	// The zero minted value is a nil carrier, and printing one must not
	// panic inside fmt -- a map miss and a nil element both produce it.
	if got := fmt.Sprint(reflect.Zero(mt).Interface()); got != "<nil>" {
		t.Errorf("the zero minted value renders as %q, want <nil>", got)
	}
}

// Minting is interned on the struct's SHAPE, and that is what keeps a
// type declared inside a loop from leaking one reflect.Type per iteration
// (reflect.StructOf never frees). These pin both halves: what collides,
// and what does not.
func TestMintedTypesInternByShape(t *testing.T) {
	same := declare(t, "type P struct {\n\tX int\n}", "P")
	again := declare(t, "type P struct {\n\tX int\n}", "P")
	if same.storeT != again.storeT {
		t.Error("two identical declarations of P got different storage types")
	}
	if same == again {
		t.Error("harness: the two declarations should still be distinct StructTypes")
	}

	for _, tc := range []struct{ name, src, decl string }{
		{"different name", "type Q struct {\n\tX int\n}", "Q"},
		{"different field name", "type P struct {\n\tY int\n}", "P"},
		{"different field type", "type P struct {\n\tX string\n}", "P"},
		{"extra field", "type P struct {\n\tX int\n\tY int\n}", "P"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if other := declare(t, tc.src, tc.decl); other.storeT == same.storeT {
				t.Errorf("%s shares a storage type with P{X int}", tc.name)
			}
		})
	}

	// A nested struct is spelled out in the signature rather than named,
	// so two Outers differing only in what their Inner holds do not
	// collide -- the case a name-only signature would get wrong.
	outerA := declare(t, "type In struct {\n\tN int\n}\ntype Out struct {\n\tI In\n}", "Out")
	outerB := declare(t, "type In struct {\n\tN string\n}\ntype Out struct {\n\tI In\n}", "Out")
	if outerA.storeT == outerB.storeT {
		t.Error("two Outs over different Ins share a storage type")
	}
}

// The leak bound itself: declareType runs on every execution of its
// statement, so a `type` inside a loop makes a StructType per iteration.
// Only the SHAPE is minted, so the table must not grow with the trip
// count.
func TestRepeatedDeclarationMintsOnce(t *testing.T) {
	storeMu.Lock()
	before := len(storeTypes)
	storeMu.Unlock()

	if _, err := eval(t, `for i := 0; i < 50; i++ {
	type Loopy struct {
		X int
	}
	p := Loopy{i}
	_ = p
}`, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	storeMu.Lock()
	after := len(storeTypes)
	storeMu.Unlock()
	if grew := after - before; grew != 1 {
		t.Errorf("50 executions of one declaration minted %d types, want 1", grew)
	}
}

// fromStore and convertTo are the whole boundary, so a minted value must
// never be what a read hands back.
func TestMintedValuesDoNotEscapeToScripts(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"index", `xs := []P{{1}}
fmt.Printf("%T\n", xs[0])`},
		{"map read", `m := map[string]P{"k": {1}}
fmt.Printf("%T\n", m["k"])`},
		{"map miss", `m := map[string]P{}
fmt.Printf("%T\n", m["zz"])`},
		{"comma-ok", `m := map[string]P{"k": {1}}
v, _ := m["k"]
fmt.Printf("%T\n", v)`},
		{"range slice", `for _, p := range []P{{1}} {
	fmt.Printf("%T\n", p)
}`},
		{"range map", `for _, p := range map[string]P{"k": {1}} {
	fmt.Printf("%T\n", p)
}`},
		{"make fill", `xs := make([]P, 1)
fmt.Printf("%T\n", xs[0])`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// %T is the one place the erasure is visible to a script at
			// all, so it is the sharpest probe: whatever comes back must
			// be the plain *StructVal every other path already deals in,
			// never a minted carrier.
			wantOut(t, "type P struct {\n\tX int\n}\n"+tc.body, "*interp.StructVal\n")
		})
	}
}
