package interp

import "testing"

// Env is small enough that these are true unit tests -- no AST, no
// evaluator. They pin the three properties the rest of the interpreter
// leans on: lookup walks outward, Define always shadows locally, and Set
// mutates the binding it finds rather than creating a new one.

func TestEnvDefineAndGet(t *testing.T) {
	e := NewEnv(nil)
	if _, ok := e.Get("x"); ok {
		t.Fatal("fresh env reported a binding for x")
	}
	e.Define("x", 42)
	v, ok := e.Get("x")
	if !ok || v != 42 {
		t.Fatalf("Get(x) = %v, %v; want 42, true", v, ok)
	}
}

// A nil value is a real binding. `x := nil` must be distinguishable from
// an undefined name, which is why Get returns a separate ok rather than
// letting callers test the value against nil.
func TestEnvNilValueIsStillBound(t *testing.T) {
	e := NewEnv(nil)
	e.Define("x", nil)
	v, ok := e.Get("x")
	if !ok {
		t.Fatal("Get(x) = !ok; a nil-valued binding must still report bound")
	}
	if v != nil {
		t.Fatalf("Get(x) = %v; want nil", v)
	}
}

func TestEnvLookupWalksOutward(t *testing.T) {
	outer := NewEnv(nil)
	outer.Define("x", 1)
	mid := NewEnv(outer)
	inner := NewEnv(mid)

	v, ok := inner.Get("x")
	if !ok || v != 1 {
		t.Fatalf("Get(x) through two scopes = %v, %v; want 1, true", v, ok)
	}
}

// Define always writes into the receiving scope, never the one that holds
// an outer binding of the same name. This is what makes `:=` shadow.
func TestEnvDefineShadows(t *testing.T) {
	outer := NewEnv(nil)
	outer.Define("x", "outer")
	inner := NewEnv(outer)
	inner.Define("x", "inner")

	if v, _ := inner.Get("x"); v != "inner" {
		t.Errorf("inner sees %v, want inner", v)
	}
	if v, _ := outer.Get("x"); v != "outer" {
		t.Errorf("outer was clobbered: sees %v, want outer", v)
	}
}

// Set is the `=` path: it finds the existing cell -- possibly several
// scopes out -- and overwrites it in place.
func TestEnvSetMutatesOuterBinding(t *testing.T) {
	outer := NewEnv(nil)
	outer.Define("x", 1)
	inner := NewEnv(NewEnv(outer))

	if !inner.Set("x", 2) {
		t.Fatal("Set(x) = false; the binding exists two scopes out")
	}
	if v, _ := outer.Get("x"); v != 2 {
		t.Errorf("outer x = %v after inner Set; want 2", v)
	}
	// No local binding was created on the way.
	if _, local := inner.vars["x"]; local {
		t.Error("Set created a binding in the inner scope; it must mutate in place")
	}
}

// An unbound Set fails rather than silently declaring. The evaluator
// turns that false into "undefined: x (use := to declare)".
func TestEnvSetUnboundFails(t *testing.T) {
	e := NewEnv(NewEnv(nil))
	if e.Set("nope", 1) {
		t.Error("Set on an unbound name returned true")
	}
	if _, ok := e.Get("nope"); ok {
		t.Error("a failed Set left a binding behind")
	}
}

// Cells are pointers so that sibling scopes -- and closures -- observe
// each other's writes to a shared binding. Without the indirection each
// scope would need its own copy and mutation would not propagate.
func TestEnvSiblingsShareCells(t *testing.T) {
	parent := NewEnv(nil)
	parent.Define("n", 0)
	a, b := NewEnv(parent), NewEnv(parent)

	a.Set("n", 7)
	if v, _ := b.Get("n"); v != 7 {
		t.Errorf("sibling sees %v after the other set 7; cells are not shared", v)
	}
}

// Shadowing is undone by leaving the scope: the outer cell is untouched,
// so a later Set from a sibling still hits it.
func TestEnvShadowDoesNotCaptureLaterSets(t *testing.T) {
	outer := NewEnv(nil)
	outer.Define("x", 1)
	shadowed := NewEnv(outer)
	shadowed.Define("x", 100)
	shadowed.Set("x", 101) // hits the shadow, not the outer cell

	if v, _ := outer.Get("x"); v != 1 {
		t.Errorf("outer x = %v; a Set against a shadow must not reach it", v)
	}
	if v, _ := shadowed.Get("x"); v != 101 {
		t.Errorf("shadow x = %v; want 101", v)
	}
}
