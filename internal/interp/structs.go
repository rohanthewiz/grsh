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
		return in.errAt(ts, "only struct type declarations are supported in grsh v1")
	}
	t := &StructType{Name: ts.Name.Name, Index: map[string]int{}}
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			return in.errAt(f, "embedded fields are not supported in grsh v1")
		}
		var zero Value
		if rt, err := in.typeOf(f.Type); err == nil {
			zero = reflect.Zero(rt).Interface()
		}
		for _, n := range f.Names {
			t.Index[n.Name] = len(t.Fields)
			t.Fields = append(t.Fields, n.Name)
			t.Zero = append(t.Zero, zero)
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
			v, err := in.eval1(env, kv.Value)
			if err != nil {
				return nil, err
			}
			sv.Vals[idx] = v
			continue
		}
		if i >= len(t.Fields) {
			return nil, in.errAt(el, fmt.Sprintf("too many values in %s literal", t.Name))
		}
		v, err := in.eval1(env, el)
		if err != nil {
			return nil, err
		}
		sv.Vals[i] = v
	}
	return sv, nil
}

// structField reads sv.Field.
func (in *Interp) structField(n ast.Node, sv *StructVal, field string) (Value, error) {
	idx, ok := sv.Type.Index[field]
	if !ok {
		return nil, in.errAt(n, fmt.Sprintf("unknown field %s in %s", field, sv.Type.Name))
	}
	return sv.Vals[idx], nil
}

// setStructField writes sv.Field = v.
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
