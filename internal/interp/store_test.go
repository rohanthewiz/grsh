package interp

import (
	"fmt"
	"reflect"
	"sync/atomic"
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

	// Checked before it is dereferenced, and with Fatal: a comparable
	// struct that minted no key type would otherwise nil-panic below and
	// take the whole package's output down with it, which is the same
	// reason this test checks the CAUSE before the effect.
	if st.keyT == nil {
		t.Fatal("a comparable struct minted no key type; map[P]int cannot name its key struct")
	}
	// Both carriers are held to it. The key carrier is the sharper case:
	// what it HOLDS, StructKey, has an unexported method, so it is only
	// the named-field indirection that keeps promotion down to one.
	for _, c := range []struct {
		what string
		t    reflect.Type
	}{{"element", mt}, {"key", st.keyT}} {
		if got := c.t.NumMethod(); got != 1 {
			t.Fatalf("minted %s type has %d methods, want exactly 1 (String).\n"+
				"An unexported method on ScriptStruct, ScriptKey, or anything "+
				"they embed breaks reflect.StructOf promotion and crashes the "+
				"process from itabInit -- see the warning on ScriptStruct.", c.what, got)
		}
		if !c.t.Implements(reflect.TypeFor[fmt.Stringer]()) {
			t.Fatalf("minted %s type is not a fmt.Stringer; it would print as grsh's storage", c.what)
		}
	}
	// A key type must also be usable AS one, which is the property the
	// element type is not asked for.
	if !st.keyT.Comparable() {
		t.Fatal("minted key type is not comparable; reflect.MapOf would panic on it")
	}
	// intoKeyStore builds a key by reinterpreting a *ScriptKey as a
	// pointer to the minted type, so the two must be the same bytes: one
	// ScriptKey at offset zero and nothing else. mintKeyType panics if
	// that ever stops holding, and this is the same invariant stated
	// where a reader of the test will meet it.
	//
	// The decode side leans on the same equivalence from the other end:
	// decodeMintedKey lifts the carrier back out with
	// rv.Field(0).Interface().(ScriptKey), so a mint that grew a second
	// field would fail the assertion there. That assertion is a runtime
	// check the old by-index field reads never had -- which is why
	// StructKey's field ORDER is no longer pinned anywhere.
	kc := reflect.TypeFor[ScriptKey]()
	if st.keyT.NumField() != 1 || st.keyT.Field(0).Type != kc ||
		st.keyT.Field(0).Offset != 0 || st.keyT.Size() != kc.Size() {
		t.Fatalf("minted key type %s is not layout-identical to ScriptKey (%d fields, %d bytes vs %d): "+
			"intoKeyStore's reflect.NewAt alias reads the wrong memory",
			st.keyT, st.keyT.NumField(), st.keyT.Size(), kc.Size())
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

	// The same detonation on the key side: a map keyed by a minted type
	// hands every key to fmt, and the zero key is what `m[nil]` stores.
	k, err := structKeyOf(st.newZero())
	if err != nil {
		t.Fatalf("encoding a key: %v", err)
	}
	mp := reflect.MakeMap(reflect.MapOf(st.keyT, reflect.TypeFor[int]()))
	mp.SetMapIndex(intoKeyStore(k, st.keyT), reflect.ValueOf(7))
	if got := fmt.Sprint(mp.Interface()); got != "map[P{X: 0}:7]" {
		t.Errorf("a map[P]int renders as %q, want map[P{X: 0}:7]", got)
	}
	if got := fmt.Sprint(reflect.Zero(st.keyT).Interface()); got != "<nil>" {
		t.Errorf("the zero minted key renders as %q, want <nil>", got)
	}
	// A freshly built key must find the entry an equal one stored: the
	// whole reason a key is encoded rather than held as a *StructVal.
	again, err := structKeyOf(st.newZero())
	if err != nil {
		t.Fatalf("re-encoding a key: %v", err)
	}
	if got := mp.MapIndex(intoKeyStore(again, st.keyT)); !got.IsValid() {
		t.Error("an equal key built fresh did not find its entry; minting broke field-wise equality")
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
	if same.keyT != again.keyT {
		t.Error("two identical declarations of P got different key types")
	}
	if same == again {
		t.Error("harness: the two declarations should still be distinct StructTypes")
	}
	// The two positions must never share a type: one unwraps to a
	// *StructVal and the other to a StructKey, and a single type would
	// make storeOwnerOf answer for a key and read it as the wrong thing.
	if same.storeT == same.keyT {
		t.Error("the element and key storage types are the same type")
	}
	if storeOwnerOf(same.keyT) != nil {
		t.Error("a key type is registered as an element type")
	}
	if keyOwnerOf(same.storeT) != nil {
		t.Error("an element type is registered as a key type")
	}

	for _, tc := range []struct{ name, src, decl string }{
		{"different name", "type Q struct {\n\tX int\n}", "Q"},
		{"different field name", "type P struct {\n\tY int\n}", "P"},
		{"different field type", "type P struct {\n\tX string\n}", "P"},
		{"extra field", "type P struct {\n\tX int\n\tY int\n}", "P"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := declare(t, tc.src, tc.decl)
			if other.storeT == same.storeT {
				t.Errorf("%s shares a storage type with P{X int}", tc.name)
			}
			if other.keyT == same.keyT {
				t.Errorf("%s shares a key type with P{X int}", tc.name)
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

// loopyRun numbers the struct shapes TestRepeatedDeclarationMintsOnce
// declares, so each execution of the test mints one nothing has minted
// before. See the comment on that test for why a fixed name cannot work.
var loopyRun atomic.Int64

// The leak bound itself: declareType runs on every execution of its
// statement, so a `type` inside a loop makes a StructType per iteration.
// Only the SHAPE is minted, so the table must not grow with the trip
// count.
//
// The declared type is given a name unique to this RUN, and that is load
// bearing rather than tidy. The mint tables are process-global and
// outlive the test: a fixed name is already in them the second time the
// test executes, the growth is 0 rather than 1, and the exact assertion
// below fails on perfectly correct code. That made `go test -count=2`
// red on this package, and it made a mutation-probe pass -- which runs
// the suite over and over and reads a failure as "the probe was caught"
// -- report a catch for every probe, including ones that changed nothing
// but speed. A unique shape per run is what makes the exact count
// assertable more than once.
func TestRepeatedDeclarationMintsOnce(t *testing.T) {
	storeMu.Lock()
	before, keysBefore := len(storeTypes), len(keyTypes)
	storeMu.Unlock()

	name := fmt.Sprintf("Loopy%d", loopyRun.Add(1))
	if _, err := eval(t, `for i := 0; i < 50; i++ {
	type `+name+` struct {
		X int
	}
	p := `+name+`{i}
	_ = p
}`, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	storeMu.Lock()
	after, keysAfter := len(storeTypes), len(keyTypes)
	storeMu.Unlock()
	if grew := after - before; grew != 1 {
		t.Errorf("50 executions of one declaration minted %d types, want 1", grew)
	}
	// The key table is bounded the same way and by the same signature --
	// it is the second thing per shape that reflect.StructOf never frees.
	if grew := keysAfter - keysBefore; grew != 1 {
		t.Errorf("50 executions of one declaration minted %d key types, want 1", grew)
	}
}

// An incomparable struct can never reach a map key -- typeOf refuses
// map[P]... and names the field to blame -- so no key type is minted for
// one. Nothing would hold it, and reflect.StructOf never frees.
func TestIncomparableStructMintsNoKeyType(t *testing.T) {
	storeMu.Lock()
	before := len(keyTypes)
	storeMu.Unlock()

	st := declare(t, "type Leaky struct {\n\tTags []string\n}", "Leaky")
	if st.keyT != nil {
		t.Error("an incomparable struct minted a key type nothing can use")
	}
	if st.storeT == nil {
		t.Error("an incomparable struct is still a perfectly good container element")
	}

	storeMu.Lock()
	after := len(keyTypes)
	storeMu.Unlock()
	if after != before {
		t.Errorf("the key table grew by %d for an incomparable struct", after-before)
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
		// The key side. A minted KEY unwraps through a different door --
		// decodeMintedKey, which decodes rather than dereferences -- and
		// range is the only path that hands one back to a script.
		{"range map keys", `for k := range map[P]int{{1}: 2} {
	fmt.Printf("%T\n", k)
}`},
		{"range map keys, nil key", `m := map[P]int{}
m[nil] = 1
for k := range m {
	fmt.Printf("%T\n", k)
}`},
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
