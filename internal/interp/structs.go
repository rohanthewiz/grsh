package interp

import (
	"fmt"
	"go/ast"
	"reflect"
	"strings"
)

// StructType is a script-declared struct type. Field types are advisory
// (used for zero values); assignment is dynamically typed like the rest
// of the interpreter.
type StructType struct {
	Name   string
	Fields []string
	Zero   []Value // zero value per field (nil when the type is exotic)
	Index  map[string]int

	// FieldTypes is the resolved type per field, parallel to Fields, and
	// nil at any position typeOf could not resolve (another script
	// struct, or a type grsh does not model). It exists so a field's
	// literal can elide its type the way a slice element's can:
	// P{Tags: {"a"}} needs to know Tags is a []string.
	FieldTypes []reflect.Type
}

// StructVal is an instance of a script-declared struct.
type StructVal struct {
	Type *StructType
	Vals []Value
}

func (sv *StructVal) String() string {
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
		rt, err := in.typeOf(f.Type)
		var zero Value
		if err == nil {
			zero = reflect.Zero(rt).Interface()
		} else {
			rt = nil
		}
		for _, n := range f.Names {
			t.Index[n.Name] = len(t.Fields)
			t.Fields = append(t.Fields, n.Name)
			t.Zero = append(t.Zero, zero)
			t.FieldTypes = append(t.FieldTypes, rt)
		}
	}
	env.Define(ts.Name.Name, t)
	return nil
}

func (t *StructType) newZero() *StructVal {
	vals := make([]Value, len(t.Fields))
	copy(vals, t.Zero)
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
	if !ok {
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
