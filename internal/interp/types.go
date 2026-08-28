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

// structKeyType is the storage type a script struct erases to in map-KEY
// position, and anyType is the element type of the array inside it.
var (
	structKeyType = reflect.TypeFor[StructKey]()
	anyType       = reflect.TypeFor[any]()
)

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
// A map KEY is the second place a struct can sit, and it needs its own
// field. Struct map keys used to be REFUSED, and that refusal was exactly
// what made one leaf enough; allowing them buys the feature back at the
// price of the word it saved. KT names the struct at the key edge, and a
// map is the only type that has one:
//
//	map[P]int         RT map[StructKey]int          ST nil  KT P
//	map[string][]P    RT map[string][]*StructVal    ST P    KT nil
//	map[P]Q           RT map[StructKey]*StructVal   ST Q    KT P
//
// TWO leaves is still a bounded claim rather than the start of a general
// tree, and typeOf enforces it: a key must be comparable, which rules out
// a map or slice inside a key, so a key edge is always one hop and never
// leads to another key. A struct NESTED as a field of a key is not a leaf
// here at all — the value encoder handles it (see StructKey), not the
// type descriptor.
//
// Elem drops KT, because whatever sits at the element edge has its own
// key edge or none. The cost of that is stated where it shows: a map
// nested inside another container loses the identity of its key struct,
// so `[]map[P]int{{{1}: 2}}` cannot ELIDE the key literal and wants
// `[]map[P]int{{P{1}: 2}}` instead. Only elision is affected; the map
// itself works at any depth.
//
// The descriptor is three words and travels by value: resolving a type
// still allocates nothing.
type TypeDesc struct {
	RT reflect.Type // storage type; nil only for a type that did not resolve
	ST *StructType  // the script struct at RT's element leaf, if any
	KT *StructType  // the script struct at RT's key leaf, if any
}

// IsStruct reports whether this descriptor IS a script struct — not a
// container holding one.
//
// Both storage types count. A key descriptor says StructKey because that
// is what the MAP holds, but what the script builds for that position is
// an ordinary P, and every construction path routes on this method:
// structComposite makes the *StructVal and convertTo encodes it on the
// way into the map.
// A THIRD storage type counts as of the minting in store.go: inside a
// container a struct is held as its own minted type, and TypeDesc.Elem
// hands exactly that down. Comparing against d.ST's own storeT keeps the
// answer exact without a registry lookup -- a minted type belonging to a
// DIFFERENT struct is correctly not this descriptor's struct.
func (d TypeDesc) IsStruct() bool {
	if d.ST == nil {
		return false
	}
	return d.RT == structValType || d.RT == structKeyType || d.RT == d.ST.storeT
}

// storeRT is the reflect.Type this descriptor's values take when they sit
// INSIDE a container, which is the only position where the erasure costs
// something (store.go says what and why). Everywhere else -- a variable, a
// struct field, an argument -- a script struct stays the plain *StructVal
// it has always been, so nothing outside a container slot has to know
// this type exists.
func (d TypeDesc) storeRT() reflect.Type {
	if d.RT == structValType && d.ST != nil {
		return d.ST.storeT
	}
	return d.RT
}

// Elem is the element descriptor of a slice or map type. ST rides along
// because the leaf it names is reached through element edges; KT does not,
// because a key edge belongs to one map only.
func (d TypeDesc) Elem() TypeDesc { return TypeDesc{RT: d.RT.Elem(), ST: d.ST} }

// Key is the key descriptor of a map type, and KT becomes its ST: from
// here down, the key's struct is the one that matters.
func (d TypeDesc) Key() TypeDesc { return TypeDesc{RT: d.RT.Key(), ST: d.KT} }

// Zero builds the value a `var x T` or an unset slot starts at.
//
// A script struct's zero is a fresh instance with every field zeroed, not
// the nil that reflect.Zero would hand back for the erased *StructVal —
// which is the whole reason this is a method and not a reflect call at
// each site.
//
// A KEY descriptor would return a *StructVal here rather than the
// StructKey its RT names. Nothing asks: a map key has no zero-valued slot
// to fill, so the callers (var, a struct field's zero, make's fill) never
// hold one.
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
	// A descriptor that IS a struct answers with the name directly, which
	// also settles the key case: ST names it whichever storage type RT
	// happens to be.
	if d.IsStruct() {
		return d.ST.Name
	}
	// Minted names go first and unconditionally: one CONTAINS
	// *interp.StructVal, so a pass looking for that erasure would cut a
	// minted name in half. storeReplace turns the whole thing into the
	// script's own name, which is what makes []P read as []P.
	s := storeReplace(d.RT.String())
	if d.ST != nil {
		s = strings.ReplaceAll(s, structValType.String(), d.ST.Name)
	}
	if d.KT != nil {
		s = strings.ReplaceAll(s, structKeyType.String(), d.KT.Name)
	}
	// Any erasure the descriptor could not name — the key of a map that
	// reached here through a container, which dropped KT — still must not
	// print as grsh's internals.
	return eraseNames(s, "struct")
}

// scriptTypeName renders a reflect.Type for a script-facing message where
// no TypeDesc survived to say WHICH struct — inside convertTo, for one,
// which sees only the storage type. The neutral word "struct" is all that
// is honest there; naming grsh's internal *interp.StructVal is not.
func scriptTypeName(t reflect.Type) string {
	return eraseNames(t.String(), "struct")
}

// eraseNames replaces every storage type a script struct erases to with
// the given name. Both erasures are handled together because a single
// type can contain both: map[P]Q is map[StructKey]*StructVal.
func eraseNames(s, name string) string {
	// A minted container type names its struct exactly, so it is replaced
	// with that name rather than the neutral word -- and it is replaced
	// FIRST, for the reason TypeDesc.String gives.
	s = storeReplace(s)
	for _, erased := range [2]string{structValType.String(), structKeyType.String()} {
		s = strings.ReplaceAll(s, erased, name)
	}
	return s
}

// keysMatch reports whether the struct KEYS actually present in v are the
// struct this descriptor names at its key edge.
//
// It is the one place the erasure still has to be answered by the VALUE
// rather than by the type. An element leaf no longer needs this: a
// container holds its struct's own minted type, so []P and []Q are
// different reflect.Types and AssignableTo is exact even for an empty
// slice. A KEY leaf is not minted — a map key must be Go-comparable with
// field-wise equality, which is what StructKey is for, and every script
// struct erases to that one type. So map[P]int and map[Q]int ARE the same
// reflect.Type, and the keys have to say which.
//
// What it cannot decide, it accepts:
//
//	map[P]int{}                   empty — no key names a struct
//	m[nil] = v                    the zero StructKey names none either
//	[]map[P]int                   Elem drops KT (see TypeDesc)
//
// So a container assertion is exact on its element leaf and "no key
// contradicts it" on its key leaf.
func (d TypeDesc) keysMatch(v Value) bool {
	if d.KT == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Map {
		return true
	}
	for iter := rv.MapRange(); iter.Next(); {
		sk, ok := iter.Key().Interface().(StructKey)
		if !ok {
			return false
		}
		// Compared by STORAGE type, not by StructType pointer, so the key
		// side draws the same line the element side does: two identical
		// declarations of P share a storage type and are mutually
		// acceptable (see structSig), a Q is not.
		if sk.T != nil && sk.T.storeT != d.KT.storeT {
			return false
		}
	}
	return true
}
