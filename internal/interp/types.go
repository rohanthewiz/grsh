package interp

import (
	"reflect"
	"strings"
)

// structValType is the single storage type every script struct erases to.
// It is named once here because three separate decisions turn on the
// comparison: TypeDesc.IsStruct, the map-miss zero in evalIndex, and the
// error/inspector rendering below.
var structValType = reflect.TypeFor[*StructVal]()

// TypeDesc is a resolved type in TYPE position: the reflect.Type values
// of it are stored as, plus the script struct that reflect.Type cannot
// name.
//
// reflect cannot mint a NAMED type at runtime, so a script-declared
// struct has no reflect.Type of its own: every script struct erases to
// the same storage type, *StructVal, and `[]P` is stored as
// `[]*StructVal`. ST is what repairs that erasure.
//
// ST is the struct sitting at the LEAF of RT, reached by descending
// element (and map value) edges only:
//
//	P                 RT *StructVal               ST P
//	[]P               RT []*StructVal             ST P
//	map[string][]P    RT map[string][]*StructVal  ST P
//	[]int             RT []int                    ST nil
//
// ONE leaf is enough, and that is a property typeOf enforces rather than
// a hope: a script struct is rejected in map-KEY position (erasure would
// make every lookup compare pointers instead of fields), so the element
// and key edges can never fork toward two different structs. Elem and Key
// therefore carry ST down unchanged, and it is consulted only where RT
// bottoms out at *StructVal — which is exactly what IsStruct tests.
//
// The whole descriptor is two words and travels by value: resolving a
// type allocates nothing, so the composite-literal path costs what it
// cost when it was a bare reflect.Type.
type TypeDesc struct {
	RT reflect.Type // storage type; nil only for a type that did not resolve
	ST *StructType  // the script struct at RT's leaf, if any
}

// IsStruct reports whether this descriptor IS a script struct — not a
// container holding one.
func (d TypeDesc) IsStruct() bool { return d.RT == structValType && d.ST != nil }

// Elem is the element descriptor of a slice or map type. ST rides along
// because the leaf it names is reached through element edges.
func (d TypeDesc) Elem() TypeDesc { return TypeDesc{RT: d.RT.Elem(), ST: d.ST} }

// Key is the key descriptor of a map type. ST rides along for uniformity
// and is inert: typeOf refuses a script struct as a key, so RT here never
// bottoms out at *StructVal.
func (d TypeDesc) Key() TypeDesc { return TypeDesc{RT: d.RT.Key(), ST: d.ST} }

// Zero builds the value a `var x T` or an unset slot starts at.
//
// A script struct's zero is a fresh instance with every field zeroed, not
// the nil that reflect.Zero would hand back for the erased *StructVal —
// which is the whole reason this is a method and not a reflect call at
// each site.
func (d TypeDesc) Zero() Value {
	switch {
	case d.RT == nil:
		return nil
	case d.IsStruct():
		return d.ST.newZero()
	}
	return reflect.Zero(d.RT).Interface()
}

// String renders the type the way the script spelled it: the erased
// *interp.StructVal is replaced by the struct's own name, at whatever
// depth it appears, so `[]P` reads as `[]P` in an error message.
func (d TypeDesc) String() string {
	if d.RT == nil {
		return "?"
	}
	if d.ST != nil {
		return strings.ReplaceAll(d.RT.String(), structValType.String(), d.ST.Name)
	}
	return d.RT.String()
}

// scriptTypeName renders a reflect.Type for a script-facing message where
// no TypeDesc survived to say WHICH struct — inside convertTo, for one,
// which sees only the storage type. The neutral word "struct" is all that
// is honest there; naming grsh's internal *interp.StructVal is not.
func scriptTypeName(t reflect.Type) string {
	s := t.String()
	if !strings.Contains(s, structValType.String()) {
		return s
	}
	return strings.ReplaceAll(s, structValType.String(), "struct")
}
