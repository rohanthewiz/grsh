package interp

import (
	"fmt"
	"go/ast"
	"reflect"
	"strings"

	"github.com/rohanthewiz/serr"
)

// StructType is a script-declared struct type. Field types are advisory
// (used for zero values); assignment is dynamically typed like the rest
// of the interpreter.
type StructType struct {
	Name   string
	Fields []string
	Zero   []Value // zero value per field (nil when the type is exotic)
	Index  map[string]int

	// FieldTypes is the resolved type per field, parallel to Fields, with
	// a nil RT at any position typeOf could not resolve (a type grsh does
	// not model). It exists so a field's literal can elide its type the
	// way a slice element's can: P{Tags: {"a"}} needs to know Tags is a
	// []string, and P{In: {1}} needs to know In is an Inner.
	FieldTypes []TypeDesc

	// structFields records whether any Zero entry is itself a *StructVal.
	// Zero is shared by every instance, so those entries have to be
	// duplicated in newZero rather than aliased — and the flag keeps the
	// ordinary all-scalar struct out of that loop entirely.
	structFields bool

	// keyArr is the [len(Fields)]any array type a value of this struct
	// encodes into when it is used as a MAP KEY -- see StructKey. It is
	// built once at declaration rather than on demand, so nothing mutates
	// a StructType after env.Define publishes it.
	keyArr reflect.Type

	// sig is the struct's identity, and storeT and keyT are its types
	// inside a container -- storeT at an ELEMENT slot, keyT at a map KEY
	// -- see store.go, which owns all three and explains why a container
	// cannot just hold the erased *StructVal. They are set once at
	// declaration, after the field loop has resolved FieldTypes.
	//
	// keyT is nil for an INCOMPARABLE struct, which can never reach a map
	// key at all, so nothing would ever hold one.
	sig    string
	storeT reflect.Type
	keyT   reflect.Type

	// noCmp names the field that makes this type incomparable, or is nil
	// when == is allowed. Go decides comparability from the STATIC field
	// types, and so does this: the verdict is computed once at
	// declaration, so `p == q` either always works or always fails,
	// rather than depending on whether a slice field happens to be nil
	// at the moment of the comparison.
	noCmp *cmpDefect
}

// cmpDefect names the first field that costs a struct its comparability,
// and is what turns Go's blunt "struct containing []string cannot be
// compared" into a message pointing at the field to change.
//
// Path is dotted through struct-typed fields ("I.Tags"), because the
// offending field can be several types down from the one being compared.
type cmpDefect struct {
	Path string // field path from the compared struct to the culprit
	Type string // the culprit's type, spelled as the script spelled it
}

// compareDefect reports why a field named name, declared as e and
// resolved to d, makes its struct incomparable — or nil if it does not.
func compareDefect(name string, d TypeDesc, e ast.Expr) *cmpDefect {
	switch {
	case d.RT == nil:
		// A type grsh does not model can still be one Go plainly refuses
		// to compare, and a func field is the reachable case: grsh leaves
		// func types unresolved because its closures are *Closure values,
		// not reflect funcs. Reading the verdict off the SYNTAX is what
		// keeps it stable -- left to the field walk, `p == q` would
		// succeed while both Fn fields were nil and start failing the
		// moment either was set.
		if _, isFunc := ast.Unparen(e).(*ast.FuncType); isFunc {
			// The bare kind word, not the full signature. Rendering the
			// signature means an AST printer: go/types.ExprString costs
			// 1.6MB of binary (+14%) and go/printer 276KB (+2.5%), both
			// for decoration -- the FIELD NAME is the actionable half of
			// the message, and it is already there.
			return &cmpDefect{Path: name, Type: "func"}
		}
		// Anything else unresolved has nothing static to say, so the
		// verdict is left to the field walk, which checks the value the
		// field actually holds.
		return nil
	case d.IsStruct():
		// The erasure makes a struct field a POINTER, and reflect calls
		// every pointer comparable — so RT.Comparable() would wave
		// through a struct whose own fields are slices. The nested type's
		// verdict is the only correct one, and it is already final: a
		// field type must be declared before the struct that uses it, so
		// the type graph is a DAG and d.ST was finished first.
		if inner := d.ST.noCmp; inner != nil {
			return &cmpDefect{Path: name + "." + inner.Path, Type: inner.Type}
		}
		return nil
	case !d.RT.Comparable():
		return &cmpDefect{Path: name, Type: d.String()}
	}
	return nil
}

// StructKey is the comparable stand-in a script struct becomes when it
// crosses into a map's KEY position.
//
// The interpreter owns the == operator and made it field-wise, but it
// does NOT own the map: reflect.Map hashes and compares keys with Go's
// own runtime equality, which this package cannot reach into. A map keyed
// on the erased *StructVal would therefore compare POINTERS, and every
// lookup made with a freshly built key would miss. The fix is not to
// teach the map about structs -- it is to hand it a key whose Go-native
// equality is already the answer we want.
//
//	T   the struct type, so P{1} and Q{1} never collide
//	F   a [len(T.Fields)]any ARRAY of the field values, which Go compares
//	    element-wise, recursively, exactly as structEqual does
//
// An array rather than an encoded string, because the key has to travel
// BACK: `range m` yields keys, and the script must get its P returned,
// not grsh's storage. Holding the values as themselves makes the return
// trip a copy instead of a parse -- so there is no decoder that could
// ever disagree with the encoder, and no ambiguity about whether a field
// held an int or a rune.
//
// The whole type is unreachable from script code. It exists only between
// convertTo and the map.
type StructKey struct {
	T *StructType
	F any // [len(T.Fields)]any
}

// String renders the key as the struct the script wrote, which is what
// puts `map[P{X: 1}:v]` rather than grsh's internals in front of anyone
// who prints a struct-keyed map. fmt finds this on the key values
// themselves, so every printing path gets it without knowing about it.
func (k StructKey) String() string {
	sv := k.structVal()
	if sv == nil {
		return "<nil>"
	}
	return sv.String()
}

// structVal rebuilds the script's struct from the key.
//
// The result is FRESH on every call, which is what makes it safe to hand
// a range variable to the script: mutating it cannot reach the key inside
// the map, so `for k := range m { k.X = 9 }` cannot corrupt m's hashing.
func (k StructKey) structVal() *StructVal {
	// The zero StructKey is what a nil struct key encodes to, and it is
	// reachable: `m[nil] = 1` converts nil to the key type's zero.
	if k.T == nil {
		return nil
	}
	return decodeKeyArr(k.T, reflect.ValueOf(k.F))
}

// decodeKeyArr rebuilds a script struct from a key's field array.
//
// It takes the array as a reflect.Value rather than as the StructKey that
// holds it so that decodeMintedKey can reach it WITHOUT ever materialising
// a StructKey: a key sitting in a map is read through reflect, and pulling
// a three-word struct out of a field with Interface() has to copy it to
// the heap first. One decoder, two ways in, so the two can never drift.
//
// An invalid arr -- a key whose F was never set -- decodes to the
// struct's fields all nil rather than panicking, which is the same answer
// the field loop would give for an array of nil interfaces.
func decodeKeyArr(t *StructType, arr reflect.Value) *StructVal {
	vals := make([]Value, len(t.Fields))
	if arr.IsValid() {
		for i := range vals {
			vals[i] = fromKeyValue(arr.Index(i).Interface())
		}
	}
	return &StructVal{Type: t, Vals: vals}
}

// keyArrFanout is the field count up to which structKeyOf builds the key
// array as a Go literal instead of through reflect.
//
// It is a threshold rather than a rule because the array's TYPE is chosen
// at runtime -- [len(Fields)]any -- and Go has no way to write a literal
// whose length is a variable. Enumerating the small lengths is the only
// way to reach the fast path at all, so the cutoff is set where the
// enumeration stops paying: struct map keys in practice have a handful of
// fields, and every length past this one falls back to the reflect path
// below, which is correct for any length.
const keyArrFanout = 4

// structKeyOf encodes a struct value into the key the map actually holds.
//
// Recursion terminates for copyStruct's reason: a struct value cannot
// contain itself, because every store takes a copy first.
//
// This is the hot half of a struct-keyed crossing -- every read, write,
// delete and literal entry encodes one key -- so it has two paths for
// what is the same job. See keyArrFanout for why the split exists and
// BenchmarkKeyCrossing for what it buys.
func structKeyOf(sv *StructVal) (StructKey, error) {
	// A typed nil is an ordinary value in grsh, and the zero StructKey is
	// its encoding -- so a nil stored under one name is found by a nil
	// looked up under another.
	if sv == nil {
		return StructKey{}, nil
	}
	if n := len(sv.Vals); n <= keyArrFanout && n == len(sv.Type.Fields) {
		// A fixed-size stack array collects the encoded fields, and the
		// switch copies out of it into an array of the RIGHT length. That
		// length is what makes the key's type -- a [2]any and a [3]any are
		// different types and can never collide -- so it must match
		// sv.Type.keyArr, which is ArrayOf(len(Fields), anyType).
		//
		// Every instance carries one Val per field, so the two lengths
		// agree; the guard above checks it rather than trusting it,
		// because a short Vals would otherwise mint a key of a length
		// the rest of the interpreter does not expect. The reflect path
		// below sizes itself from keyArr and handles that case as it
		// always did.
		//
		// A nil field needs no special case here: an unset element of the
		// buffer is already the nil interface the reflect path leaves
		// behind, so both paths encode a nil field identically.
		var buf [keyArrFanout]any
		for i, v := range sv.Vals {
			ev, err := keyValue(v)
			if err != nil {
				return StructKey{}, err
			}
			buf[i] = ev
		}
		// Each case RETURNS, so a length with no case here simply falls
		// through to the general path below. Raising keyArrFanout without
		// adding a case therefore costs speed and never correctness.
		switch n {
		case 0:
			return StructKey{T: sv.Type, F: [0]any{}}, nil
		case 1:
			return StructKey{T: sv.Type, F: [1]any{buf[0]}}, nil
		case 2:
			return StructKey{T: sv.Type, F: [2]any{buf[0], buf[1]}}, nil
		case 3:
			return StructKey{T: sv.Type, F: [3]any{buf[0], buf[1], buf[2]}}, nil
		case 4:
			return StructKey{T: sv.Type, F: [4]any{buf[0], buf[1], buf[2], buf[3]}}, nil
		}
	}
	// The general path: reflect is the only way to build a value of an
	// array type chosen at runtime. It costs one allocation more than the
	// literals above, because Interface() on an ADDRESSABLE value has to
	// copy the array out before it can box it.
	arr := reflect.New(sv.Type.keyArr).Elem()
	for i, v := range sv.Vals {
		ev, err := keyValue(v)
		if err != nil {
			return StructKey{}, err
		}
		// A nil field stays the array element's own zero: reflect.ValueOf
		// of a nil Value is invalid and Set would panic on it.
		if ev == nil {
			continue
		}
		arr.Index(i).Set(reflect.ValueOf(ev))
	}
	return StructKey{T: sv.Type, F: arr.Interface()}, nil
}

// keyValue encodes one field value on its way into a key, and is where
// the erasure gets undone one level down: a struct-typed field would
// otherwise sit in the array as a *StructVal and be compared by identity
// again, defeating the whole point one level deeper.
//
// The comparability check is not redundant with the noCmp gate typeOf
// applies to the key type. noCmp is static, and an `any` field is
// statically comparable and dynamically anything -- Go's own
// runtime-panic case. Reporting beats the panic Go's map would raise
// while hashing.
func keyValue(v Value) (Value, error) {
	if sv, ok := v.(*StructVal); ok {
		return structKeyOf(sv)
	}
	if v == nil {
		return nil, nil
	}
	if t := reflect.TypeOf(v); !t.Comparable() {
		return nil, serr.New("cannot use " + scriptTypeName(t) + " as part of a map key")
	}
	return v, nil
}

// fromKeyValue is keyValue's inverse, and the only thing the decode side
// needs: every other value went in as itself.
func fromKeyValue(v Value) Value {
	if k, ok := v.(StructKey); ok {
		return k.structVal()
	}
	return v
}

// StructVal is an instance of a script-declared struct.
type StructVal struct {
	Type *StructType
	Vals []Value
}

func (sv *StructVal) String() string {
	// A nil instance is reachable now that []P exists: append(xs, nil)
	// converts nil to a typed nil element. Print it rather than panic
	// inside fmt, which would report the panic instead of the value.
	if sv == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(sv.Type.Name)
	b.WriteByte('{')
	for i, f := range sv.Type.Fields {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s: %v", f, sv.Vals[i])
	}
	b.WriteByte('}')
	return b.String()
}

// declareType handles `type Name struct { ... }`.
func (in *Interp) declareType(env *Env, ts *ast.TypeSpec) error {
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		return in.errAt(ts, "only struct type declarations are supported yet")
	}
	t := &StructType{Name: ts.Name.Name, Index: map[string]int{}}
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			return in.errAt(f, "embedded fields are not supported yet")
		}
		// A field type grsh cannot model is not an error: assignment is
		// dynamically typed, so the field simply starts nil and works.
		// The resolved type is kept rather than discarded after the zero
		// is taken, because an elided nested literal needs it later.
		//
		// A struct-typed field resolves like any other one, which is what
		// makes `type Outer struct { In Inner }` zero to a real Inner{}.
		// It cannot recurse forever: a field type must already be
		// declared when the struct is (env.Define runs last, below), so
		// the type graph is a DAG by construction — `type N struct { Next
		// N }` leaves Next unresolved rather than looping.
		d, err := in.typeOf(env, f.Type)
		var zero Value
		if err == nil {
			zero = d.Zero()
		} else {
			d = TypeDesc{}
		}
		if _, nested := zero.(*StructVal); nested {
			t.structFields = true
		}
		for _, n := range f.Names {
			t.Index[n.Name] = len(t.Fields)
			t.Fields = append(t.Fields, n.Name)
			t.Zero = append(t.Zero, zero)
			t.FieldTypes = append(t.FieldTypes, d)
			// First defect wins: the message names one field to fix, and
			// the rest of the declaration is still recorded normally --
			// an incomparable struct is a perfectly usable struct.
			if t.noCmp == nil {
				t.noCmp = compareDefect(n.Name, d, f.Type)
			}
		}
	}
	t.keyArr = reflect.ArrayOf(len(t.Fields), anyType)
	// The signature reads FieldTypes, so it can only be taken once the
	// loop above is done -- and it is exact by then for the reason the
	// loop relies on: a field's type must already be declared, so every
	// nested struct already has a signature of its own.
	t.sig = structSig(t)
	t.storeT = mintStoreType(t)
	// The key type is minted only when the struct can BE a key. noCmp was
	// settled by the loop above, and typeOf refuses map[P]... when it is
	// set -- so minting one here would leak a type nothing can reach.
	if t.noCmp == nil {
		t.keyT = mintKeyType(t)
	}
	env.Define(ts.Name.Name, t)
	return nil
}

// newZero builds a fresh instance with every field at its zero, and its
// contract is that the result shares nothing with the TYPE.
//
// The plain copy is the whole job for a struct of scalars. A
// struct-TYPED field is the exception: its zero in t.Zero is one
// *StructVal held by the type, so copying the slice alone would hand
// every instance that same nested struct.
//
// Most callers would survive without the duplication, because their
// result reaches a name through copyOnStore, which descends and isolates
// it anyway. `make([]Out, 2)` is the caller that would not: its fill
// writes each instance STRAIGHT into the slice, so without this the two
// elements share one In and xs[0].I.N = 7 sets xs[1].I.N too. Tested.
//
// The flag keeps the loop off the common path, and copyStruct's descent
// terminates for the reason declareType records: the type graph is a DAG.
func (t *StructType) newZero() *StructVal {
	vals := make([]Value, len(t.Fields))
	copy(vals, t.Zero)
	if t.structFields {
		for i, v := range vals {
			if sv, ok := v.(*StructVal); ok {
				vals[i] = sv.copyStruct()
			}
		}
	}
	return &StructVal{Type: t, Vals: vals}
}

// structComposite builds Point{X: 1} or Point{1, 2}.
//
// Field values are not copied on the way in, and that is safe for a
// reason worth stating: the literal is a value, so it reaches a name, a
// parameter or a slot through some store, and copyStruct DESCENDS into
// struct fields -- so the store that isolates the literal isolates its
// fields with it.
//
// A slice or map literal is the opposite case and does copy its elements:
// storing a slice copies the reference and stops there, so nothing
// downstream would ever isolate what the elements alias.
func (in *Interp) structComposite(env *Env, t *StructType, n *ast.CompositeLit) (Value, error) {
	sv := t.newZero()
	for i, el := range n.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				return nil, in.errAt(kv.Key, "struct literal key must be a field name")
			}
			idx, ok := t.Index[key.Name]
			if !ok {
				return nil, in.errAt(kv.Key, fmt.Sprintf("unknown field %s in %s", key.Name, t.Name))
			}
			v, err := in.elidedElem(env, kv.Value, t.FieldTypes[idx])
			if err != nil {
				return nil, err
			}
			sv.Vals[idx] = v
			continue
		}
		if i >= len(t.Fields) {
			return nil, in.errAt(el, fmt.Sprintf("too many values in %s literal", t.Name))
		}
		v, err := in.elidedElem(env, el, t.FieldTypes[i])
		if err != nil {
			return nil, err
		}
		sv.Vals[i] = v
	}
	return sv, nil
}

// copyOnStore applies Go's value semantics at the points where a value
// ENTERS a storage location: a binding, a parameter, a container slot, a
// struct field. A StructVal is a pointer, so without this a second name
// for a struct is a second name for the SAME struct.
//
// Reads deliberately do not copy, and that asymmetry is the whole design:
//
//	b := a         store — b must be a separate struct
//	xs[0].X = 1    read  — must reach the element in place, as Go does
//	p.Move()       read  — a pointer receiver must see the instance
//
// Copying on read would break the last two, and copying on neither is
// the bug this replaces. Only structs need it: everything else the
// interpreter holds is either immutable (numbers, strings, bools) or a
// reference type Go shares across a copy too, so the overwhelming
// majority of calls are one type assertion and a return.
func copyOnStore(v Value) Value {
	sv, ok := v.(*StructVal)
	// A TYPED nil arrives from convertTo whenever nil is stored into a
	// []P or a map[K]P slot. There is nothing to duplicate, and
	// copyStruct would dereference it.
	if !ok || sv == nil {
		return v
	}
	return sv.copyStruct()
}

// copyOnStoreAll copies a whole argument list in place.
func copyOnStoreAll(vs []Value) {
	for i, v := range vs {
		vs[i] = copyOnStore(v)
	}
}

// copyStruct duplicates the instance at Go's struct-copy depth: a
// struct-typed FIELD is part of the value and copies with it, while a
// slice, map or closure field is a reference that a Go copy shares too.
//
//	type Inner struct { Xs []int }
//	type Outer struct { In Inner }
//
//	b := a     b.In is a fresh Inner, so b.In.N = 9 leaves a alone
//	           b.In.Xs is still a.In.Xs — one slice, two names
//
// The recursion terminates because a struct value cannot contain itself.
// Building one needs a finite literal, and `a.In = a` stores a copy taken
// BEFORE the write — copy-on-store is itself what makes a cycle
// unconstructible.
//
// This is also the copy a value receiver gets. It used to be a flat
// `copy(vals, sv.Vals)`, which shared nested struct fields with the
// caller: a method with a value receiver could reach through one and
// mutate the instance it was supposed to be insulated from.
func (sv *StructVal) copyStruct() *StructVal {
	vals := make([]Value, len(sv.Vals))
	for i, v := range sv.Vals {
		vals[i] = copyOnStore(v)
	}
	return &StructVal{Type: sv.Type, Vals: vals}
}

// structEqual is `a == b` for two script structs, compared FIELD-WISE the
// way Go compares struct values -- not by the identity of the two
// *StructVal pointers, which is what the erasure would otherwise leave
// `==` meaning.
//
// There is deliberately no `a == b` identity fast path. It would be
// correct for the answer and wrong for the ERROR: comparing an
// incomparable struct to itself would quietly succeed while comparing it
// to an equal-looking twin failed, and a diagnostic that depends on
// whether the two operands happen to be the same instance is worse than
// no diagnostic.
//
// Recursion terminates for the same reason copyStruct's does: a struct
// value cannot contain itself. Building one needs a finite literal, and
// `a.Next = a` stores a copy taken BEFORE the write, so copy-on-store is
// what makes a cycle unconstructible. (The type DAG alone would not be
// enough -- a self-referential field like `Next N` is left unresolved and
// still holds a real struct at runtime.)
func (in *Interp) structEqual(n ast.Node, a, b *StructVal) (bool, error) {
	// Typed nils are reachable (append(xs, nil) makes one), so nil is a
	// value here rather than an impossibility.
	if a == nil || b == nil {
		return a == nil && b == nil, nil
	}
	// Two DIFFERENT struct types are unequal rather than an error, which
	// is the answer the rest of the interpreter already gives for a
	// cross-type ==: `1 == "a"` is false, not a type complaint. Go decides
	// this at compile time and grsh has no compile time to decide it in.
	if a.Type != b.Type {
		return false, nil
	}
	if d := a.Type.noCmp; d != nil {
		return false, in.errAt(n, fmt.Sprintf("%s cannot be compared with ==: field %s has type %s",
			a.Type.Name, d.Path, d.Type),
			"hint", "compare the fields that matter, or give "+a.Type.Name+" an Equal method")
	}
	for i := range a.Vals {
		eq, err := in.valuesEqual(n, a.Vals[i], b.Vals[i])
		if err != nil || !eq {
			return false, err
		}
	}
	return true, nil
}

// valuesEqual compares two values under struct equality: the operands of
// a top-level `==` where at least one side is a struct, and every field
// pair reached from there.
//
// The non-comparable complaint at the bottom is reachable only through
// the FIELD walk. A top-level `==` routes a non-struct pair to binaryOp's
// ordinary fallback, which answers false for an uncomparable pair rather
// than failing -- so `xs == ys` on two slices keeps behaving as it did.
// Inside a struct the same silence would be a trap: the field walk is
// claiming to implement Go's ==, and Go rejects that struct outright.
func (in *Interp) valuesEqual(n ast.Node, x, y Value) (bool, error) {
	xs, xok := x.(*StructVal)
	ys, yok := y.(*StructVal)
	switch {
	case xok && yok:
		return in.structEqual(n, xs, ys)
	case xok:
		// A struct against a non-struct. Only untyped nil can match, and
		// only a typed-nil struct matches it: `p == nil` is false for a
		// real instance, which is the honest answer where Go would not
		// have allowed the comparison at all.
		return xs == nil && y == nil, nil
	case yok:
		return ys == nil && x == nil, nil
	case x == nil || y == nil:
		return x == nil && y == nil, nil
	}
	// A field noCmp could not settle statically reaches here: an `any`,
	// which Go also calls comparable and also fails at runtime, or a type
	// grsh does not model. The VALUE is what gets checked, and reporting
	// is what Go's own runtime panic would have done.
	for _, v := range [2]Value{x, y} {
		if t := reflect.TypeOf(v); !t.Comparable() {
			return false, in.errAt(n, "cannot compare a field holding "+scriptTypeName(t))
		}
	}
	return safeEqual(x, y), nil
}

// methodKey is the global a top-level method declaration transforms into
// (transform.MethodPrefix; spelled out here to avoid the import cycle).
func methodKey(typeName, method string) string {
	return "__m_" + typeName + "_" + method
}

// lookupMethod finds a script-declared method for a struct type.
func (in *Interp) lookupMethod(typeName, method string) (*Closure, bool) {
	v, ok := in.globals.Get(methodKey(typeName, method))
	if !ok {
		return nil, false
	}
	cl, ok := v.(*Closure)
	return cl, ok
}

// methodHasPtrRecv reports whether the method was declared with a pointer
// receiver — its first parameter, after the transform rewrite.
func methodHasPtrRecv(cl *Closure) bool {
	fl := cl.Fn.Type.Params
	if fl == nil || len(fl.List) == 0 {
		return false
	}
	_, ok := fl.List[0].Type.(*ast.StarExpr)
	return ok
}

// callStructMethod dispatches sv.Method(args...). Pointer receivers share
// the instance; value receivers get a shallow copy.
func (in *Interp) callStructMethod(env *Env, call *ast.CallExpr, sv *StructVal, name string) ([]Value, error) {
	if sv == nil {
		return nil, in.errAt(call, "cannot call "+name+" on a nil struct")
	}
	cl, ok := in.lookupMethod(sv.Type.Name, name)
	if !ok {
		// Native methods on the value itself (e.g. String) still work.
		if m := reflect.ValueOf(sv).MethodByName(name); m.IsValid() {
			return in.callValue(env, call, m.Interface(), name)
		}
		return nil, in.errAt(call, fmt.Sprintf("unknown method %s on %s", name, sv.Type.Name),
			"hint", fmt.Sprintf("declare it at top level: func (v %s) %s(...) { ... }", sv.Type.Name, name))
	}
	if call.Ellipsis.IsValid() {
		return nil, in.errAt(call, "spread calls (xs...) are not supported yet")
	}
	self := sv
	if !methodHasPtrRecv(cl) {
		self = sv.copyStruct()
	}
	args, err := in.evalArgs(env, call)
	if err != nil {
		return nil, err
	}
	return in.callClosure(call, cl, append([]Value{self}, args...))
}

// structField reads sv.Field.
func (in *Interp) structField(n ast.Node, sv *StructVal, field string) (Value, error) {
	if sv == nil {
		return nil, in.errAt(n, "nil struct has no field "+field)
	}
	idx, ok := sv.Type.Index[field]
	if !ok {
		if _, isMethod := in.lookupMethod(sv.Type.Name, field); isMethod {
			return nil, in.errAt(n, fmt.Sprintf("%s is a method of %s — call it: .%s(...)", field, sv.Type.Name, field),
				"hint", "method values are not supported in grsh")
		}
		return nil, in.errAt(n, fmt.Sprintf("unknown field %s in %s", field, sv.Type.Name))
	}
	return sv.Vals[idx], nil
}

// setStructField writes sv.Field = v.
//
// It does NOT copy v. setLValue is its only caller and copies every value
// it writes, whichever of the three target kinds it is writing to; a
// second copy here would allocate a duplicate on every field write and no
// test could tell the difference. The store site owns the copy, and there
// is exactly one store site.
func (in *Interp) setStructField(n ast.Node, sv *StructVal, field string, v Value) error {
	if sv == nil {
		return in.errAt(n, "nil struct has no field "+field)
	}
	idx, ok := sv.Type.Index[field]
	if !ok {
		return in.errAt(n, fmt.Sprintf("unknown field %s in %s", field, sv.Type.Name))
	}
	sv.Vals[idx] = v
	return nil
}

// lookupStructType resolves a type-position identifier to a declared
// struct type, if any.
func lookupStructType(env *Env, e ast.Expr) (*StructType, bool) {
	id, ok := e.(*ast.Ident)
	if !ok {
		return nil, false
	}
	v, ok := env.Get(id.Name)
	if !ok {
		return nil, false
	}
	t, ok := v.(*StructType)
	return t, ok
}
