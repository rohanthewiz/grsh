package interp

import (
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Storage types: repairing the erasure where it actually costs something.
//
// reflect cannot mint a NAMED type at runtime, so every script struct is
// held as the same *StructVal and `[]P` used to be `[]*StructVal`. That
// erasure is invisible while a value is in hand — a *StructVal knows its
// StructType — and expensive the moment it is NOT:
//
//	m["missing"]   a map miss has no value to read the type off, so the
//	               zero could only be nil, never Go's zero struct
//	x.([]P)        []P and []Q were the SAME reflect.Type, so a []Q
//	               asserted to a []P and nothing could tell
//	append(xs, q)  a Q dropped into a []P with no complaint
//
//	map[P]int      map[P]int and map[Q]int were the SAME reflect.Type,
//	               so an EMPTY one asserted to either
//
// All four are the same missing fact: the CONTAINER does not know what
// it holds. So containers stop holding *StructVal and start holding a
// type minted per struct:
//
//	[]P              []struct{ ScriptStruct "grsh:\"P|X:int\"" }
//	map[string]P     map[string]struct{ ScriptStruct "grsh:\"P|X:int\"" }
//	map[P]int        map[struct{ ScriptKey "grsh:\"P|X:int\"" }]int
//
// A KEY is minted from its own carrier, and the split is not cosmetic: a
// map key must be Go-COMPARABLE with field-wise equality, which is what
// StructKey is for, while an element must be the *StructVal a script can
// hold. One carrier could not be both. So there are two minted types per
// struct — its element type and its key type — and they are separate
// tables minted from separate carriers.
//
// Two properties make this work where a plain wrapper would not:
//
//  1. reflect.StructOf DOES promote the methods of an EMBEDDED field, so
//     the minted type is a fmt.Stringer. A script that prints a []P still
//     sees [P{X: 1}] rather than grsh's storage, through fmt, json, and
//     every other Go library the value reaches. This is the whole reason
//     the design is affordable — a non-embedded wrapper field would have
//     no methods and would print as internals everywhere.
//
//  2. The type is one pointer wide and pointer-shaped, so a slice of
//     them is the same memory as the slice of pointers it replaces and
//     boxing one into an interface still allocates nothing.
//
// What is embedded is ScriptStruct, NOT *StructVal — and ScriptKey, NOT
// StructKey — and that indirection is load-bearing rather than tidy: see
// the warning on ScriptStruct, which applies to both carriers.
//
// The cost is a rule with no compiler to enforce it: a minted value must
// never reach script code. Every read out of a container goes through
// fromStore or decodeMintedKey, and every write in goes through
// convertTo.
// Those are the entire boundary.
//
// A type minted here is never freed — reflect.StructOf caches globally —
// so what the tag is keyed on decides whether that is a bounded cost or a
// leak. See structSig.

// storeTagKey is the struct-tag key on a minted storage type. The tag
// carries no information anyone reads back; it exists because
// reflect.StructOf interns by the FULL field list, and the tag is the
// only part of a one-embedded-field struct that can differ.
const storeTagKey = "grsh"

// ScriptStruct is the carrier a minted storage type embeds: a script
// struct, held so that String comes along with it.
//
// It exists because of a hard constraint on reflect.StructOf's method
// promotion. Promoting from an embedded type that has an UNEXPORTED
// method — which *StructVal does, copyStruct — produces a method table
// whose type offsets do not resolve, and the failure is not a panic that
// a recover could turn into a script error: it is
//
//	fatal error: runtime: type offset base pointer out of range
//
// raised from itabInit the first time the value is asserted to an
// interface, which is what fmt does to every argument. The whole process
// dies. (Reachable only when the embedded type comes from a different
// package than the minted type, which every runtime-minted type does.)
//
// So the embedded type must have NO unexported methods, and keeping
// *StructVal free of them forever is not a promise this package can make.
// ScriptStruct can: it is one field and one exported method, it embeds
// nothing so it cannot inherit one, and it has no other job.
//
// DO NOT give this type an unexported method, and do not make it embed
// anything. TestMintedTypePromotesExactlyOneMethod is the canary.
type ScriptStruct struct{ SV *StructVal }

// String is the only method, and the only reason the type exists: it is
// what a minted type promotes, and therefore what fmt, json and every
// other library finds on a []P element. *StructVal.String already answers
// for a nil receiver, so the zero carrier renders as <nil> rather than
// panicking inside fmt.
func (s ScriptStruct) String() string { return s.SV.String() }

// ScriptKey is the carrier a minted KEY type embeds: the comparable
// stand-in a script struct becomes in map-key position, held so that
// String comes along with it.
//
// It is a separate carrier from ScriptStruct because the two positions
// want different things from a value. An element must come back out as
// the *StructVal the script wrote, so ScriptStruct holds one. A key must
// be hashed and compared by GO, field-wise, which a *StructVal cannot be
// — so ScriptKey holds the StructKey encoding instead.
//
// StructKey is held as a NAMED field rather than embedded, and that is
// the same load-bearing indirection ScriptStruct describes: StructKey has
// an unexported method (structVal), and promoting from an embedded type
// that has one kills the process. A named field promotes nothing, so the
// unexported method is out of reach and String is supplied here instead.
//
// DO NOT give this type an unexported method, and do not make it embed
// anything. TestMintedTypePromotesExactlyOneMethod covers it.
type ScriptKey struct{ K StructKey }

// String is the only method, and the reason the carrier exists: it is
// what a minted key type promotes, so `fmt.Println(m)` on a struct-keyed
// map renders map[P{X: 1}:v] rather than grsh's storage. StructKey.String
// already answers for the zero key, which is what a nil struct key
// encodes to.
func (k ScriptKey) String() string { return k.K.String() }

// carrierType and keyCarrierType are the carriers' own reflect.Types,
// named once because minting embeds them by name: an embedded field's
// Name must be its type's.
var (
	carrierType    = reflect.TypeFor[ScriptStruct]()
	keyCarrierType = reflect.TypeFor[ScriptKey]()
)

var (
	// storeMu guards every table and the replacer below. It is taken only
	// when a struct type is DECLARED, never on a read.
	storeMu sync.Mutex

	// storeTypes and keyTypes intern minted types by struct signature, so
	// re-running the same `type P struct{...}` — in a loop, in a function
	// called repeatedly, at each REPL line — reuses one type instead of
	// leaking a fresh one per execution. Two tables because a struct has
	// two minted types, one per position, and they are different types.
	storeTypes = map[string]reflect.Type{}
	keyTypes   = map[string]reflect.Type{}

	// storeOwners and keyOwners map a minted type back to the struct it
	// stores, which is what lets a map MISS build a real zero: the slot
	// has no value to read a StructType off, but its TYPE now names one.
	//
	// They stay SEPARATE rather than merging into one table, because the
	// answer is used to decide how to unwrap: an element type unwraps two
	// hops to a *StructVal, a key type unwraps to a StructKey and decodes.
	// One table would make storeOwnerOf answer yes for a key type and
	// fromStore would then read a StructKey as a *StructVal.
	//
	// Copy-on-write behind an atomic pointer: writes happen once per
	// distinct struct shape in the program, reads happen on every element
	// pulled out of a container, and a plain map read beats any lock.
	storeOwners atomic.Pointer[map[reflect.Type]*StructType]
	keyOwners   atomic.Pointer[map[reflect.Type]*StructType]

	// storeNames rewrites minted type names back to the script's own in
	// any message built from a reflect.Type. Rebuilt whenever a type is
	// minted, which is rare enough that rebuilding beats scanning.
	storeNames atomic.Pointer[strings.Replacer]
)

// structSig is the identity a minted type is interned under: the struct's
// name, its field names, and each field's resolved type, with a nested
// script struct spelled out rather than named.
//
// Interning on the SHAPE rather than on the *StructType is what bounds
// the leak. declareType runs every time its statement executes, so a
// `type P struct{...}` inside a loop makes a fresh StructType per
// iteration; keying on those would mint a type per iteration and never
// free one. Keying on the shape means the table is bounded by the number
// of distinct struct shapes in the SOURCE.
//
// The price is stated where it lands: two separately-declared but
// identical structs share a storage type, so a []P built under one
// declaration accepts a P built under the other. They are still distinct
// StructTypes, so `p.(P)` and `p == q` tell them apart; only the
// container type does not. Spelling the nested struct out — rather than
// writing its name — is what keeps that limited to genuinely identical
// shapes: `type P struct{I Inner}` under two different Inners does not
// collide.
func structSig(t *StructType) string {
	var b strings.Builder
	b.WriteString(t.Name)
	b.WriteByte('|')
	for i, f := range t.Fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(f)
		b.WriteByte(':')
		b.WriteString(t.FieldTypes[i].sig())
	}
	return b.String()
}

// sig renders a field's type for the signature above.
//
// A container needs no special casing at EITHER leaf: its element type
// and its key type are both minted types whose NAMES contain that
// struct's own signature, so reflect's rendering is exact by
// construction. A struct map key used to be the one edge reflect could
// not spell — every key erased to the single StructKey type, so the KT
// field had to name it and the signature had to substitute it back.
// Minting keys retired both.
//
// It is exact for the same reason the field loop can call it at all: a
// field's type must already be declared, so its minted types exist and
// already carry their signatures.
func (d TypeDesc) sig() string {
	switch {
	case d.RT == nil:
		// A field type grsh does not model. Unresolved fields are
		// dynamically typed anyway, so the shape they contribute is
		// "unknown" — two structs differing only there are genuinely
		// interchangeable as far as storage is concerned.
		return "?"
	case d.IsStruct():
		return "{" + d.ST.sig + "}"
	}
	return d.RT.String()
}

// mintStoreType returns the type values of t take inside a container,
// creating it on first use.
//
// Called from declareType, so it runs once per executed declaration and
// never on a hot path.
func mintStoreType(t *StructType) reflect.Type {
	return mint(t, carrierType, storeTypes, &storeOwners)
}

// mintKeyType is mintStoreType for the KEY position, and its only
// difference is which carrier it embeds — which is the whole reason the
// two positions are separate types.
//
// declareType calls it only for a COMPARABLE struct. An incomparable one
// can never reach a map key (typeOf refuses `map[P]int` and names the
// field to blame), so minting for it would leak a type nothing can use,
// and a nil keyT is the honest record of that.
func mintKeyType(t *StructType) reflect.Type {
	kt := mint(t, keyCarrierType, keyTypes, &keyOwners)
	// intoKeyStore reinterprets a *ScriptKey as a *kt, which is sound
	// only while kt is exactly one ScriptKey at offset zero. mint builds
	// it that way today; checking here is what turns a future second
	// field into a loud failure at declaration rather than keys that
	// silently read the wrong memory.
	if kt.NumField() != 1 || kt.Field(0).Type != keyCarrierType ||
		kt.Field(0).Offset != 0 || kt.Size() != keyCarrierType.Size() {
		panic("grsh: minted key type " + kt.String() + " is not layout-identical to ScriptKey; intoKeyStore's alias is unsound")
	}
	return kt
}

// mint interns one minted type per (carrier, signature) pair and records
// the struct it belongs to.
//
// Both positions share this because everything except the carrier is the
// same: the same interning rule, the same owner bookkeeping, the same
// name rebuild. Splitting the carrier out is what keeps the two minted
// types genuinely distinct types while keeping one description of how a
// type gets minted.
func mint(t *StructType, carrier reflect.Type, table map[string]reflect.Type, owners *atomic.Pointer[map[reflect.Type]*StructType]) reflect.Type {
	storeMu.Lock()
	defer storeMu.Unlock()

	if mt, ok := table[t.sig]; ok {
		return mt
	}
	mt := reflect.StructOf([]reflect.StructField{{
		// An embedded field must be named for its type, and being
		// EMBEDDED is what promotes String onto the minted type.
		Name:      carrier.Name(),
		Type:      carrier,
		Anonymous: true,
		Tag:       reflect.StructTag(storeTagKey + `:"` + t.sig + `"`),
	}})
	table[t.sig] = mt

	next := map[reflect.Type]*StructType{}
	if cur := owners.Load(); cur != nil {
		for k, v := range *cur {
			next[k] = v
		}
	}
	next[mt] = t
	owners.Store(&next)

	rebuildNamesLocked()
	return mt
}

// rebuildNamesLocked refreshes the type-name replacer from both owner
// tables. Called with storeMu held, once per minted type, which is rare
// enough that rebuilding beats maintaining.
//
// Both tables feed ONE replacer because a single reflect.Type can contain
// both minted kinds — map[P]Q is map[keyOf(P)]storeOf(Q) — so a pass that
// knew about only one would leave the other printing as grsh's carrier.
func rebuildNamesLocked() {
	var pairs []string
	for _, owners := range [2]*atomic.Pointer[map[reflect.Type]*StructType]{&storeOwners, &keyOwners} {
		cur := owners.Load()
		if cur == nil {
			continue
		}
		for k, v := range *cur {
			pairs = append(pairs, k.String(), v.Name)
		}
	}
	storeNames.Store(strings.NewReplacer(pairs...))
}

// storeOwnerOf reports the script struct a minted type stores, or nil for
// any other type.
//
// The Kind guard is not an optimization detail worth hiding: every scalar,
// pointer, slice and map leaves on it, so the map lookup is reached only
// by struct-KIND values — which in a container are almost always ours.
func storeOwnerOf(t reflect.Type) *StructType {
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	owners := storeOwners.Load()
	if owners == nil {
		return nil
	}
	return (*owners)[t]
}

// keyOwnerOf is storeOwnerOf for the key position: it reports the script
// struct a minted KEY type stands in for, or nil for any other type.
//
// This is what closes the last leaf the erasure left. map[P]int and
// map[Q]int are different reflect.Types now, so an assertion is exact
// even on an EMPTY map, where there is no key to read a struct off.
func keyOwnerOf(t reflect.Type) *StructType {
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	owners := keyOwners.Load()
	if owners == nil {
		return nil
	}
	return (*owners)[t]
}

// fromStore reads one container slot as the script must see it: a minted
// value becomes the *StructVal it wraps, everything else is itself.
//
// The unwrap hands back the SAME pointer the container holds, not a copy,
// which is what keeps xs[0].X = 1 writing through to the element. Go's
// value semantics are applied by copyOnStore at the points where a value
// reaches a NAME, exactly as before.
//
// A nil embedded pointer unwraps to a typed nil *StructVal, which is what
// a nil element of a []P already was.
func fromStore(rv reflect.Value) Value {
	if rv.Kind() == reflect.Struct && storeOwnerOf(rv.Type()) != nil {
		// Two hops: the minted type embeds the carrier, the carrier holds
		// the struct.
		return rv.Field(0).Field(0).Interface()
	}
	return rv.Interface()
}

// zeroInSlot is reflect.Zero for a container's element type, repaired for
// a script struct.
//
// This is the map miss. reflect.Zero of a minted type is a wrapper around
// a NIL *StructVal — grsh's storage detail, not Go's zero struct — but
// the type now names its struct, so the real zero is one lookup away. The
// answer is unwrapped because it is going to the script, not to a slot.
func zeroInSlot(t reflect.Type) Value {
	if st := storeOwnerOf(t); st != nil {
		return st.newZero()
	}
	return reflect.Zero(t).Interface()
}

// decodeMintedKey reads one map KEY as the script must see it: it
// rebuilds the *StructVal a minted key encodes.
//
// It is fromStore's counterpart, and the asymmetry is StructKey's: an
// element hands back the very pointer the container holds, while a key
// hands back a FRESH struct decoded from the encoding. That is what makes
// `for k := range m { k.X = 9 }` safe — mutating the range variable
// cannot reach the key inside the map and corrupt its hashing.
//
// Unlike fromStore it takes no non-key values: the caller must have
// ESTABLISHED that rv is a minted key, and there is no guard here on
// purpose. Keys are read a whole map at a time, and every such loop
// already knows the answer for the whole map — one key type per map — so
// a guard would put a registry lookup inside the loop for nothing.
//
// The two StructKey fields are read THROUGH reflect rather than by
// lifting the whole StructKey out with Interface(). A three-word struct
// does not fit in an interface word, so boxing one out of an ADDRESSABLE
// field copies it to the heap first; reading the fields separately never
// does, because a pointer unboxes for free and the field array comes out
// of its `any` field as the two words already sitting there.
func decodeMintedKey(rv reflect.Value) *StructVal {
	// Two hops, like fromStore: the minted type embeds the carrier, the
	// carrier holds the encoding.
	sk := rv.Field(0).Field(0)
	t, _ := sk.Field(0).Interface().(*StructType)
	if t == nil {
		// The zero key, which `m[nil] = 1` puts in a map. structVal
		// answers a typed nil for it and so must this.
		return nil
	}
	// Interface() on an interface-kind Value is not the boxing case: it
	// hands back the eface already in the field rather than building a
	// new one, so this costs a two-word load and the array is not copied.
	// It is what lets decodeKeyArr take a plain `any` and so serve this
	// path and StructKey.structVal from one body.
	return decodeKeyArr(t, sk.Field(1).Interface())
}

// intoStore wraps a script struct for a container slot of minted type mt.
// It is convertTo's other half; nothing else should build one.
func intoStore(sv *StructVal, mt reflect.Type) reflect.Value {
	w := reflect.New(mt).Elem()
	if sv != nil {
		w.Field(0).Field(0).Set(reflect.ValueOf(sv))
	}
	return w
}

// intoKeyStore wraps an encoded key for a map of minted key type kt. It
// is intoStore's counterpart on the key side, and the same rule holds:
// only convertTo should build one.
//
// It builds the value by ALIASING rather than by filling it field by
// field, and that is measured rather than clever. A minted key type is
// exactly one embedded ScriptKey and nothing else, so it has ScriptKey's
// size, alignment and pointer map — the two are the same three words
// under different names, which is precisely the case reflect.NewAt
// exists for. Going through reflect.New and a Set per field produced the
// same bytes for twice the time, and this runs on every read, write,
// delete and literal entry of a struct-keyed map.
//
// The alias is sound only while the minted type keeps that shape.
// mintKeyType CHECKS it at declaration rather than trusting it, so a
// second field added to the mint would fail loudly there instead of
// silently mis-typing every key.
//
// The zero StructKey — what a nil struct key encodes to — needs no case
// of its own: a zero ScriptKey is already the zero minted value.
func intoKeyStore(k StructKey, kt reflect.Type) reflect.Value {
	return reflect.NewAt(kt, unsafe.Pointer(&ScriptKey{K: k})).Elem()
}

// eface mirrors the two-word layout of an interface holding a value too
// big to fit in a word: a type word, and a pointer to the value itself.
//
// Only the REINTERPRETATION in boxKeyArr is unsafe. Every pointer here is
// written by an ordinary assignment to this typed struct, so the write
// barrier the collector needs is emitted the usual way; storing straight
// into an `any`'s words through a pointer would skip it, which is the
// version of this trick that breaks under a concurrent mark.
type eface struct {
	typ  unsafe.Pointer
	data unsafe.Pointer
}

// boxKeyArr returns an `any` holding the [N]any that p points at, WITHOUT
// copying it.
//
// This exists because reflect cannot box an array it just built without
// paying for it twice. reflect.New allocates the array; Interface() on
// the addressable value it yields allocates a SECOND array and copies
// into it, because an addressable value could still be written through
// and an interface's contents must not change underneath it. Here nothing
// can write through it: the only alias is structKeyOf's fill loop, which
// is finished, so the array is already the immutable thing an interface
// requires and the copy is pure cost. Removing it halves both the
// allocations and the bytes of a wide key, and takes ~72% off the time.
//
// zero is a boxed value of the SAME array type, and it is here only for
// its type word. N is chosen at runtime, so no literal of that type can
// be written and the word has to be borrowed from a value that already
// carries it. StructType caches one per declared struct.
//
// mintKeyArrZero checks at declaration that this actually reproduces the
// array rather than trusting the layout, in the same spirit as
// mintKeyType's check on intoKeyStore's alias.
func boxKeyArr(zero any, p unsafe.Pointer) any {
	e := *(*eface)(unsafe.Pointer(&zero))
	e.data = p
	return *(*any)(unsafe.Pointer(&e))
}

// unboxKeyArr takes a boxed [N]any apart into the two words an interface
// is made of: the type word that says WHICH [N]any it is, and a pointer
// to the array itself. Nothing is copied, and nothing is written.
//
// It is boxKeyArr's inverse, and it exists for the decode side of the
// same problem. reflect can read the array back -- arr.Index(i) then
// Interface() -- but it pays a Value construction and a call per field,
// ~5.5ns each, where reading the words directly is a load and a store.
// See decodeKeyArr for the measurement and for what the type word is
// then used for.
//
// The read is safe in a way boxKeyArr's write is not: it produces no new
// reference the collector has to be told about, so there is no write
// barrier to skip. What it DOES claim is the layout -- two words, type
// then data -- and mintKeyArrZero probes exactly that at declaration.
func unboxKeyArr(a any) (typ, data unsafe.Pointer) {
	e := *(*eface)(unsafe.Pointer(&a))
	return e.typ, e.data
}

// mintKeyArrZero returns the boxed zero of arr for boxKeyArr to borrow a
// type word from, having first confirmed on this very type that the
// borrow works.
//
// The probe writes a distinctive value into a fresh array and reads it
// back THROUGH the box, so it fails if the type word is wrong, if the
// data word is wrong, or if an interface ever stops being two words in
// this order. It runs once per declared struct, on the declaration path,
// which is cold.
//
// It probes the DECODE direction on the same box, because unboxKeyArr
// makes the same claim about the same layout and decodeKeyArr reads a
// live map key through it. Checking both here means one failing check
// wherever the claim stops holding, rather than a silent misread.
func mintKeyArrZero(arr reflect.Type) any {
	zero := reflect.Zero(arr).Interface()
	probe := reflect.New(arr)
	const mark = "grsh key array probe"
	if arr.Len() > 0 {
		probe.Elem().Index(0).Set(reflect.ValueOf(any(mark)))
	}
	boxed := boxKeyArr(zero, probe.UnsafePointer())
	if reflect.TypeOf(boxed) != arr {
		panic("grsh: aliasing a " + arr.String() + " produced a " + reflect.TypeOf(boxed).String() + "; boxKeyArr's view of an interface is wrong")
	}
	if arr.Len() > 0 && reflect.ValueOf(boxed).Index(0).Interface() != any(mark) {
		panic("grsh: aliasing a " + arr.String() + " did not read back the array it was given; boxKeyArr's view of an interface is wrong")
	}
	// The way back. decodeKeyArr identifies a key's array by comparing
	// its type word against the one carried here, so the two boxes must
	// agree on that word, and it then reads the fields straight off the
	// data word.
	typ, data := unboxKeyArr(boxed)
	if zeroTyp, _ := unboxKeyArr(zero); typ != zeroTyp {
		panic("grsh: unboxing a " + arr.String() + " did not recover the type word it was boxed with; unboxKeyArr's view of an interface is wrong")
	}
	if arr.Len() > 0 && unsafe.Slice((*any)(data), arr.Len())[0] != any(mark) {
		panic("grsh: unboxing a " + arr.String() + " did not read back the array it was given; unboxKeyArr's view of an interface is wrong")
	}
	return zero
}

// storeReplace rewrites minted type names in s to the script's own struct
// names. It runs before any other erasure, because a minted type's name
// contains grsh's own carrier and would otherwise be reported piecemeal.
func storeReplace(s string) string {
	r := storeNames.Load()
	if r == nil {
		return s
	}
	return r.Replace(s)
}
