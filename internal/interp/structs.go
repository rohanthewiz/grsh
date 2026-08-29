package interp

import (
	"cmp"
	"fmt"
	"go/ast"
	"reflect"
	"slices"
	"strconv"
	"unsafe"

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

	// keyArrZero is the zero keyArr, boxed, and it is kept for its TYPE
	// WORD alone -- boxKeyArr borrows it to name an array type whose
	// length is only known at runtime. Cached here because the general
	// encode path reads it on every crossing and building one costs an
	// allocation.
	keyArrZero any

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
	return decodeKeyArr(k.T, k.F)
}

// decodeKeyArr rebuilds a script struct from a key's BOXED field array.
//
// It takes the array as a bare `any` rather than as the StructKey that
// holds it so that decodeMintedKey can reach it WITHOUT ever materialising
// a StructKey: a key sitting in a map is read through reflect, and pulling
// a three-word struct out of a field with Interface() has to copy it to
// the heap first. One decoder, two ways in, so the two can never drift.
//
// THE FIELDS ARE READ THROUGH AN ALIAS, not through reflect. An [N]any is
// N interface words laid end to end and Value is `any`, so the array IS a
// []Value once its address is known -- and unboxKeyArr knows it without
// copying. What that replaces is arr.Index(i) plus Interface() per field,
// a Value construction and a call for what is otherwise a load and a
// store, and it is the whole per-field cost of a decode:
//
//	fields      1     2     3     6     8    10    16
//	reflect  28.3  33.0  40.1  52.5  66.8  73.8  111.4  ns
//	alias    25.3  26.5  30.1  33.6  38.9  42.4   51.5  ns
//
// Both in one binary, minimum of five runs at a fixed iteration count,
// Apple M3, one allocation on either path throughout. The slope goes from
// ~5.5ns a field to ~1.75ns, which is why the gap widens with arity: 11%
// at one field, 36% at six, 54% at sixteen. Unlike the encode side this
// needs no fanout constant and has no second path to fall off, because
// the alias does not care what N is.
//
// WHAT A SCRIPT FEELS IS MUCH SMALLER, and worth stating plainly: a
// decode is a small part of an interpreted range loop. Two builds run
// alternately, twelve container shapes, only the two that decode moved --
// range-map-key-struct by -1.3% and range-map-key-struct-10 by -2.3% --
// while the other ten sat inside a +-1% haze that includes shapes
// touching no struct at all. Both numbers are about what the microbench
// predicts: 3ns of a 505ns one-field iteration, 31ns of a 1243ns
// ten-field one. Decoding is simply not where an interpreted loop spends
// its time; this makes it cheaper without making it matter more.
//
// THE TYPE WORD IS THE GUARD. An alias cannot bounds-check, so what makes
// reading len(t.Fields) words sound is knowing the box really holds this
// struct's [len(Fields)]any -- and an array type's length is part of its
// identity, so one pointer compare against the type word cached in
// keyArrZero settles length and element type together. That is a stricter
// check than the Index() bounds check it replaces, which never looked at
// the element type at all.
//
// Anything that fails it decodes to the struct's fields all nil rather
// than panicking. The reachable case is a key whose F was never set --
// the zero StructKey has a nil F, whose type word is nil -- and all-nil
// fields is the same answer the old field loop gave for an array of nil
// interfaces.
func decodeKeyArr(t *StructType, a any) *StructVal {
	return fillKeyArr(newStructVal(t, len(t.Fields)), a)
}

// fillKeyArr writes a key's boxed field array into a StructVal that has
// already been built, and is decodeKeyArr's whole body.
//
// The split exists so the two ways of OBTAINING that StructVal -- one
// allocation each from newStructVal, or a carve from a keyArena on the
// range path -- share one decoder and can never drift. The guard reads
// sv.Type rather than taking a second *StructType parameter, because the
// only correct type to check against is the one the StructVal was built
// for; passing it separately would let the two disagree.
func fillKeyArr(sv *StructVal, a any) *StructVal {
	typ, data := unboxKeyArr(a)
	if keyTyp, _ := unboxKeyArr(sv.Type.keyArrZero); typ != keyTyp {
		return sv
	}
	for i, v := range unsafe.Slice((*any)(data), len(sv.Vals)) {
		sv.Vals[i] = fromKeyValue(v)
	}
	return sv
}

// keyArrFanout is the field count up to which structKeyOf builds the key
// array as a Go literal instead of allocating it through reflect.
//
// It is a threshold rather than a rule because the array's TYPE is chosen
// at runtime -- [len(Fields)]any -- and Go has no way to write a literal
// whose length is a variable. Enumerating the small lengths is the only
// way to reach the fast path at all, and every length past the last case
// falls back to the general path below, which is correct for any length.
//
// WHERE IT IS SET. Both paths measured against each other at every arity
// -- both in one binary with the cutoff forced either way, minimum of ten
// runs, Apple M3, one allocation on either path throughout:
//
//	fields      1     2     4     6     8    10    12
//	general  23.6  27.3  31.5  39.7  44.1  55.0  57.5  ns
//	literal  16.2  20.0  24.9  32.5  38.2  47.3  54.2  ns
//
// They do not cross: a literal is ~6-7ns cheaper at every length, being a
// stack array and one convT against reflect.New and a runtime type
// lookup. That is 31% at one field and 13% at eight, and it is the whole
// reason the enumeration exists.
//
// THE NUMBERS THIS CONSTANT WAS FIRST SET ON WERE MUCH BIGGER -- the
// general path then cost 3-5x the literal one, ~117-224ns at five to
// twelve fields, because it went through a reflect Set per field and then
// copied the whole array to box it. Rebuilding that path (see boxKeyArr)
// took both away, and the trade had to be re-taken rather than inherited:
// an enumeration justified against a path that no longer exists is an
// enumeration justified by nothing.
//
// What an added case costs is the shared buffer below, which is sized by
// this constant and zeroed whole on every call. Holding the case count
// fixed and growing only the buffer from 4 slots to 12 costs a one-field
// key 0.8ns and a four-field key 1.6ns -- about 0.18ns per unused slot.
// Holding the buffer fixed and growing only the case count from 5 to 13
// costs nothing measurable, so the switch itself is free and the buffer
// is the whole tax.
//
// 8 therefore stands, on a much narrower margin than it was set with:
// cases 5 through 8 each save a key of their own arity ~6ns and cost
// every other key ~0.7ns. Past 8 the saving stays real -- ~8ns at ten
// fields -- but it goes to arities a script hardly writes while the tax
// keeps landing on the one-field key scripts write constantly.
//
// What a SCRIPT felt when this moved from 4 to 8, two interleaved builds
// differing only in the constant: a six-field key read 19.7% cheaper and
// one allocation lighter per crossing, the one-field shapes unmoved. Both
// halves of that were measured against the OLD general path, so the same
// experiment today would show a smaller gap -- not because the cases got
// worse but because what they save you from got cheaper.
// BenchmarkStructContainer's map-key-struct-hit-6 is the shape that
// bought it and its one-field neighbours are the ones that pay.
//
// Moving it is safe in both directions, and differently so in each.
// Raising it past the last case costs speed and never correctness: the
// switch below falls through to the general path. LOWERING it below the
// last case does not compile at all, because the buffer is sized by this
// constant and the higher cases then index past its end -- which is the
// compiler enforcing what TestKeyEncodingMatchesTheDeclaredArrayType
// checks for the direction it cannot see.
const keyArrFanout = 8

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
		case 5:
			return StructKey{T: sv.Type, F: [5]any{buf[0], buf[1], buf[2], buf[3], buf[4]}}, nil
		case 6:
			return StructKey{T: sv.Type, F: [6]any{buf[0], buf[1], buf[2], buf[3], buf[4], buf[5]}}, nil
		case 7:
			return StructKey{T: sv.Type, F: [7]any{buf[0], buf[1], buf[2], buf[3], buf[4], buf[5], buf[6]}}, nil
		case 8:
			return StructKey{T: sv.Type, F: [8]any{buf[0], buf[1], buf[2], buf[3], buf[4], buf[5], buf[6], buf[7]}}, nil
		}
	}
	// The general path: reflect is the only way to ALLOCATE a value of an
	// array type chosen at runtime, and that allocation is all it is used
	// for here. Filling and boxing both go around it.
	//
	// The array is filled through a slice aliasing it rather than by a
	// reflect Set per field, which is the same trade the literal cases
	// above make: an ordinary interface assignment against reflect's
	// generic one, ~3ns a field against ~15ns. slots is sized from the
	// ARRAY, never from Vals, so a Vals longer than the struct's fields
	// -- the case this path exists to survive -- lands on a bounds check
	// exactly as arr.Index(i) used to, instead of writing past the end.
	//
	// A nil field needs no case of its own any more: assigning a nil
	// interface is just an assignment, where reflect.ValueOf(nil) is an
	// invalid Value that Set would panic on.
	p := reflect.New(sv.Type.keyArr)
	slots := unsafe.Slice((*any)(p.UnsafePointer()), sv.Type.keyArr.Len())
	for i, v := range sv.Vals {
		ev, err := keyValue(v)
		if err != nil {
			return StructKey{}, err
		}
		slots[i] = ev
	}
	// Boxed by aliasing, not by copying -- see boxKeyArr. The fill above
	// is the array's only writer and it is done, so handing the same
	// memory to an interface hands over something already immutable.
	return StructKey{T: sv.Type, F: boxKeyArr(sv.Type.keyArrZero, p.UnsafePointer())}, nil
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

// valBlock fuses a StructVal with the backing array of its Vals slice, so
// that building one instance costs ONE allocation instead of two.
//
// A is always a [N]Value. It is a type parameter only because N cannot be
// one: an array's length is part of its type, and Go has no way to name a
// type whose length is a variable. Instantiating it per length at the use
// site is what lets the array be sliced -- inside a generic function
// `vals[:]` would not compile, but at each case below A is concrete.
//
// sv sits at offset 0, so the pointer handed back is the block's own base
// pointer and Vals points just past it, into the same object. That is an
// ordinary interior pointer: it keeps the block alive exactly as long as
// the StructVal is reachable, which is the lifetime it had anyway when
// the two were separate objects.
type valBlock[A any] struct {
	sv   StructVal
	vals A
}

// valBlockFanout is the field count up to which newStructVal fuses, and
// it is the enumeration's CONTRACT rather than an input to it: the cases
// below are written out one per length, and
// TestStructValFusesUpToTheFanout walks 1..valBlockFanout asserting each
// one actually costs a single allocation. Raise the constant without
// adding a case and that test fails, which is the only way the two can be
// kept in step -- nothing in the switch itself can see the constant.
//
// It is a threshold for keyArrFanout's reason one level along: an
// enumeration is the only way to name a fixed-size array whose length is
// chosen at runtime. Every length past the last case falls through to the
// two-allocation form, which is correct for any length, so the constant
// moves in the safe direction only for SPEED.
//
// WHERE IT IS SET. Fused against unfused at every arity, both in one
// binary so they share a code layout and an allocator, minimum of five
// runs at a fixed iteration count, Apple M3. Bytes are identical on both
// paths at every arity -- a StructVal is 32 bytes and 32+16n lands in the
// same size class as 32 and 16n did apart -- so the whole difference is
// one trip through the allocator:
//
//	fields     1     2     4     6     8    10    12    14    16
//	saved   6.6   9.8  10.2   5.5  14.2   6.9   9.3   4.1  11.0  ns
//
// There is no crossover to look for and no per-call tax to trade against:
// unlike keyArrFanout, whose cases share one buffer that every call zeroes
// whole, the cases here are independent and the switch compiles to a jump
// table. So the only cost of a case is BINARY SIZE, and cases 9 through 16
// cost 976 bytes together -- 0.009% -- while buying the ten-field key
// decode 19.8% (95.1ns to 76.3ns). The one-field decode did not move
// (29.8ns to 30.2ns, inside the noise), which is the measurement that says
// the extra cases are free to the arities scripts actually write.
//
// 16 is where it stops for want of a reason to go further, not for a cost:
// a script struct with seventeen fields is rare enough that ~122 bytes of
// binary each is no longer obviously worth it.
const valBlockFanout = 16

// newStructVal returns a *StructVal with n field slots, all nil.
//
// The two-allocation form it replaces -- make([]Value, n) then
// &StructVal{} -- is what every construction of a script struct used to
// pay: newZero on every literal and every make() element, copyStruct on
// every store, and decodeKeyArr on every key a range loop yields. The
// fused block costs the SAME BYTES (a StructVal is 32 bytes and lands in
// the same size class fused as the two did apart, at every arity here)
// and one fewer trip through the allocator.
//
// n is passed rather than read off t.Fields because copyStruct sizes
// itself from the INSTANCE: a malformed value with more Vals than its
// type has fields is copied as it is, not silently truncated.
//
// WHAT A SCRIPT FEELS, two interleaved builds differing only in this
// function, six runs each, and note that the shapes divide cleanly by
// whether their loop BUILDS a struct at all:
//
//	StructZero/nested       -11.1%   allocs -25.3%
//	StructCopy/nested        -8.9%   allocs -20.2%
//	StructZero/flat          -6.6%   allocs -15.7%
//	StructCopy/flat          -5.7%   allocs -12.7%
//	range-slice-struct       -3.5%   allocs  -8.9%
//	range-map-key-struct     -3.4%   allocs -10.5%
//	map-miss-struct          -2.2%   allocs  -7.3%
//	map-hit-struct           +0.4%   allocs   ~0
//	slice-index-struct       +0.4%   allocs   ~0
//
// The two that got slower construct nothing in their loops -- they read
// fields out of structs that already exist, and their allocation counts
// did not move -- so that 0.4% is code layout, not this change.
func newStructVal(t *StructType, n int) *StructVal {
	// Each case RETURNS, so the fallthrough below covers every length
	// without a case -- including 0, which needs no fusing: a zero-length
	// make allocates nothing, so that shape already costs one allocation.
	switch n {
	case 1:
		b := &valBlock[[1]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 2:
		b := &valBlock[[2]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 3:
		b := &valBlock[[3]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 4:
		b := &valBlock[[4]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 5:
		b := &valBlock[[5]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 6:
		b := &valBlock[[6]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 7:
		b := &valBlock[[7]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 8:
		b := &valBlock[[8]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 9:
		b := &valBlock[[9]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 10:
		b := &valBlock[[10]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 11:
		b := &valBlock[[11]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 12:
		b := &valBlock[[12]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 13:
		b := &valBlock[[13]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 14:
		b := &valBlock[[14]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 15:
		b := &valBlock[[15]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	case 16:
		b := &valBlock[[16]Value]{sv: StructVal{Type: t}}
		b.sv.Vals = b.vals[:]
		return &b.sv
	}
	return &StructVal{Type: t, Vals: make([]Value, n)}
}

// keyArena carves the *StructVals for one map's decoded keys out of a
// pair of slabs instead of allocating each of them on its own.
//
// A range over a struct-keyed map decodes EVERY key before the loop body
// runs -- sortMapKeys has to render each one to order them -- so all n
// results are alive simultaneously however they were allocated. That is
// what makes a slab legitimate here rather than clever: it changes where
// the memory comes from, not how long it lives.
//
//	per key   [StructVal|[N]Value] [StructVal|[N]Value] ...   n allocations
//	arena     [StructVal StructVal ...] [Value Value ...]     2 allocations
//
// -- two per CHUNK, to be exact; see keyChunkVals, which caps how many
// keys one pair of slabs serves so that a retained key cannot pin the
// whole map. Every map small enough to fit one chunk, which is every map
// a shell is likely to range, gets exactly the two above.
//
// WHAT IT SAVES is BenchmarkMapKeyArena, minimum of twelve runs at a
// fixed iteration count, Apple M3, ns PER KEY:
//
//	keys           1     2     3     4    16    64
//	 1 field
//	  per key   30.1  26.3  27.1  23.1  20.4  20.4
//	  arena     46.3  28.7  22.3  17.5  14.4  11.2
//	10 fields
//	  per key   31.9  33.0  34.4  41.4  38.6  38.2
//	  arena     49.4  36.4  33.6  35.0  28.4  24.5
//
// Allocations go from n to 2 and stay there as n grows -- stepping to 4,
// 6 and so on only at the chunk boundaries the retention cap puts in --
// and that is the whole mechanism: past a couple of keys a decode is
// paying the allocator, not the fields. So the saving grows with the key
// count and shrinks with the field count -- 45% of a one-field decode at
// sixteen keys, 26% of a ten-field one -- because what it removes is
// per-KEY floor sitting underneath per-FIELD work it does not touch.
//
// SMALL MAPS ARE THE EXCEPTION, and are why sortMapKeys checks the length
// before building one. Two slabs are two allocations where newStructVal's
// fused block is one each, so a one-key map pays 16-18ns per key for an
// arena it never amortises and a two-key map still pays 2-3ns. Three is
// where it turns, and that is the bound sortMapKeys uses.
//
// The threshold is a TUNING choice, not a correctness one -- an arena
// built for one key decodes it perfectly well -- so it is recorded here
// against the benchmark that set it rather than pinned by a test. See
// TestRangingAMapDecodesItsKeysIntoOneArena for why no test could:
// counting allocations across the bound measures the compiler's inlining
// as much as this function.
//
// THE TRADE IS RETENTION, and keyChunkVals is what bounds it. A key that
// outlives its loop -- `for k := range m { found = k }` -- holds the slab
// it was carved from alive, where a fused block would have held only
// itself. Sizing that slab by the MAP made the bound n, which for a large
// map is an unbounded leak wearing a small function's clothes; sizing it
// by keyChunkVals makes the bound a constant. See keyChunkVals for what
// the cap costs and why 1024 slots.
type keyArena struct {
	t    *StructType
	svs  []StructVal
	vals []Value
	left int // keys not yet carved that no slab has been cut for
}

// keyChunkVals caps how many FIELD SLOTS one pair of slabs holds, and so
// caps what a single retained key can hold alive.
//
// THIS CONSTANT IS THE RETENTION BOUND and nothing else. `for k := range
// m { found = k }` keeps one decoded key past its loop, and that key
// points into the slab it was carved from, so an unchunked arena let one
// key of a 100,000-entry map hold the whole map's worth of StructVals.
// Cutting a fresh pair of slabs every chunk means the bound is a constant
// instead of the map's size.
//
// THE CAP IS IN SLOTS RATHER THAN IN KEYS because slots are what the
// memory is: a chunk of 64 keys is 3KB for a one-field struct and 12KB
// for a ten-field one, so a key count that bounds the wide struct
// over-charges the narrow one for nothing. keyChunkFor divides instead,
// charging each key for its fields AND its header, which holds both slabs
// together at 1024 slots -- 16KB -- at every arity, and leaves narrow
// structs far enough under the cap that the maps a shell ranges are
// effectively unchunked.
//
// WHAT IT COSTS is more allocations for maps bigger than a chunk, and
// nothing at all for maps smaller -- which is every map whose keys fit in
// 1024 slots, about 340 one-field keys or 85 ten-field ones.
// BenchmarkMapKeyArena, 256 keys of a ten-field struct, minimum of twelve
// runs at a fixed iteration count, Apple M3, against an arena with no cap
// at all:
//
//	slots/chunk   keys/chunk   chunks   ns/key   vs uncapped
//	   128            10         26      30.51     +23.7%
//	   256            21         13      27.15     +10.1%
//	   512            42          7      25.64      +4.0%
//	  1024            85          4      25.26      +2.4%
//	  2048           170          2      25.16      +2.0%
//	 uncapped        256          1      24.66       0.0%
//
// The refill is a fixed cost paid once per chunk, so halving the chunk
// doubles how often it is paid, and the top of the table is that doubling
// -- but the bottom of it does NOT go to zero. Two chunks still cost 2%,
// which is the branch every carve now runs to notice an empty slab; that
// part is paid whatever the cap is and is why raising it further buys
// almost nothing.
//
// 1024 slots is the knee, and it is 16KB: past it the curve is flat and
// the bound grows without limit, below it the bound tightens for a cost
// that is climbing fast. A script that holds a key past its loop pins
// 16KB it cannot notice, and the maps a shell actually ranges never cut a
// second chunk.
const keyChunkVals = 1024

// svSlots is what one StructVal costs in the same unit a field slot is
// measured in, so that one divisor can bound both slabs at once.
//
// unsafe.Sizeof is a COMPILE-TIME CONSTANT, not a pointer
// reinterpretation -- it reads a type's width and nothing else. The five
// unsafe expressions this package accounts for are the ones that alias
// memory (the NewAt in intoKeyStore, and the unsafe.Slice plus eface pair
// on each of the encode and decode paths); this is not a sixth, and
// writing 2 here instead would be the same arithmetic with the reason
// left out and a silent error waiting for whoever adds a field to
// StructVal.
const svSlots = int(unsafe.Sizeof(StructVal{}) / unsafe.Sizeof(Value(nil)))

// keyChunkFor is how many keys the next chunk serves: as many as fit in
// keyChunkVals slots once each key is charged for BOTH slabs it occupies
// -- its nf field slots and the svSlots its StructVal header costs --
// never more than are left to carve.
//
// Charging for the header is what makes the bound uniform instead of
// merely finite. A one-field key is 1 field slot and 2 header slots, so
// the header is TWICE the memory the fields are; capping on fields alone
// would let a narrow struct retain three times what a wide one does for
// the same cap. Dividing by nf+svSlots holds every arity at the same
// number of bytes.
//
// It also removes both edge cases a field-only divisor needed: nf is zero
// for `type P struct{}`, which is a legal script type and a perfectly
// good map key, and the quotient could round to zero for a struct wider
// than the whole cap. Adding svSlots makes the divisor at least 2 and the
// quotient at least 1 for any struct narrower than the cap itself; the
// floor still guards the pathological width, where one key IS the bound
// and a chunk per key is the right answer.
func keyChunkFor(nf, left int) int {
	c := keyChunkVals / (nf + svSlots)
	if c < 1 {
		c = 1
	}
	if c > left {
		c = left
	}
	return c
}

// newKeyArena sizes an arena for n keys of struct type t, cutting the
// first chunk immediately.
//
// It inlines at its one call site, so the keyArena header itself costs no
// allocation there and the slabs are the whole price — which is what
// TestRangingAMapDecodesItsKeysIntoOneArena's count of exactly two rests
// on. That is why the first chunk is cut HERE rather than by calling
// refill: refill's two makes would push this body past the inline budget
// and put the header on the heap, turning every arena's cost from two
// allocations into three.
//
// One type for all n is the map's own invariant -- a minted key type is
// minted per *StructType, so every key in a map decodes to the same
// struct -- which is what lets each field slab be one flat block rather
// than one per key, and what lets the arena keep t for its later chunks.
func newKeyArena(t *StructType, n int) *keyArena {
	c := keyChunkFor(len(t.Fields), n)
	return &keyArena{
		t:    t,
		left: n - c,
		svs:  make([]StructVal, c),
		vals: make([]Value, c*len(t.Fields)),
	}
}

// refill cuts the next chunk once the current one is spent.
//
// It is deliberately NOT inlined into newKeyArena (see there), and it is
// reached only once a whole chunk of keys is spent, so its cost is
// amortised over that chunk rather than paid per key.
func (a *keyArena) refill() {
	nf := len(a.t.Fields)
	c := keyChunkFor(nf, a.left)
	a.left -= c
	a.svs = make([]StructVal, c)
	a.vals = make([]Value, c*nf)
}

// structVal hands out the next StructVal, allocating a fresh one instead
// whenever the arena cannot serve the request.
//
// EVERY way of running out falls back rather than failing: a nil receiver
// (the single-key path, which asks for no arena at all), an exhausted
// slab, or a type wanting more fields than the arena was sized for. The
// last cannot happen while a map holds one key type, but it is two
// compares and it makes the carve TOTAL -- a future caller that sizes an
// arena wrongly gets slower decodes, not a bounds panic on a premise held
// three functions away.
func (a *keyArena) structVal(t *StructType) *StructVal {
	nf := len(t.Fields)
	if a == nil {
		return newStructVal(t, nf)
	}
	if len(a.svs) == 0 && a.left > 0 {
		// The chunk is spent and the map has keys left: cut the next one.
		// This is the only place a second chunk comes from, so a map
		// whose keys fit in one chunk never reaches it.
		a.refill()
	}
	if len(a.svs) == 0 || len(a.vals) < nf {
		return newStructVal(t, nf)
	}
	sv := &a.svs[0]
	a.svs = a.svs[1:]
	sv.Type = t
	// The capacity is capped at nf so an append to one struct's Vals
	// cannot reach into the next struct's fields. copyStruct sizes itself
	// from the instance, so a Vals that could grow is not hypothetical.
	sv.Vals = a.vals[:nf:nf]
	a.vals = a.vals[nf:]
	return sv
}

func (sv *StructVal) String() string {
	// One allocation: appendTo sizes nothing, so the only copy is the
	// string conversion at the end. The old body went through a
	// strings.Builder and an fmt.Fprintf PER FIELD, which is where this
	// path's four-allocations-at-one-field came from.
	return string(sv.appendTo(nil))
}

// appendTo renders sv into b and returns the extended buffer, producing
// byte for byte what String returns -- String is written in terms of it.
//
// IT EXISTS FOR THE ORDERING PASS. sortMapKeys renders every key of a map
// and throws the text away again, so a render that allocates is pure sort
// cost: at one field the old String cost 4 allocations and ~160ns a key,
// at ten fields 16 and ~700ns, against a decode of ~20ns. Appending into
// a caller's buffer lets that pass keep ONE slab for a whole map -- the
// same move keyArena made for the StructVals themselves.
//
// A nil instance is reachable now that []P exists: append(xs, nil)
// converts nil to a typed nil element. Render it rather than panic inside
// fmt, which would report the panic instead of the value.
func (sv *StructVal) appendTo(b []byte) []byte {
	if sv == nil {
		return append(b, "<nil>"...)
	}
	b = append(b, sv.Type.Name...)
	b = append(b, '{')
	for i, f := range sv.Type.Fields {
		if i > 0 {
			b = append(b, ',', ' ')
		}
		b = append(b, f...)
		b = append(b, ':', ' ')
		b = appendValue(b, sv.Vals[i])
	}
	return append(b, '}')
}

// appendValue writes v the way fmt's %v verb would.
//
// THE SWITCH IS A FAST PATH, NOT A SECOND DEFINITION of how a value
// prints. Every case has to produce exactly what fmt produces for that
// type, and TestAppendValueMatchesFmt holds each of them against fmt
// itself rather than against a hand-written expectation. Anything not
// listed falls through to fmt, so a Value kind this does not know about
// renders slowly, never wrongly.
//
// The cases are the types a script can put in a field: the four literal
// kinds evalExpr produces (int, float64, string, rune), bool, an unset
// field's nil, a nested struct, the int64 a stdlib call can hand back,
// and a slice of any of those -- see the note above the slice cases for
// why that list is closed rather than open-ended.
func appendValue(b []byte, v Value) []byte {
	switch x := v.(type) {
	case nil:
		return append(b, "<nil>"...)
	case string:
		return append(b, x...)
	case int:
		return strconv.AppendInt(b, int64(x), 10)
	case bool:
		return strconv.AppendBool(b, x)
	case rune: // int32, which is what a char literal evaluates to
		return strconv.AppendInt(b, int64(x), 10)
	case int64:
		return strconv.AppendInt(b, x, 10)
	case float64:
		// %v on a float is %g at the shortest precision that round-trips,
		// which is exactly this call -- including the +Inf/NaN spellings.
		return strconv.AppendFloat(b, x, 'g', -1, 64)
	case *StructVal:
		// Recursing rather than calling String saves the nested struct's
		// allocation too, and keeps a nil nested field printing "<nil>"
		// the way fmt would by reaching the same nil check.
		return x.appendTo(b)

	// ---- slices ----
	//
	// THE LIST IS CLOSED, NOT A SAMPLING. A field type is resolved by
	// typeOf, whose element names come from typeIdents -- int, int64,
	// float64, string, bool, byte, rune, any, error -- plus a script
	// struct. So `[]T` for every T a script can spell is exactly the
	// eight cases below plus []P, which is handled after the switch
	// because its element type is minted at runtime. A nested [][]T or
	// a map still falls through to fmt.
	// TestEveryScriptSliceTypeHasAFastPath fails if typeIdents grows a
	// name and this does not.
	//
	// Each case is fmt's own rendering -- elements space-separated inside
	// square brackets -- built WITHOUT boxing an element, which is where
	// fmt's cost lives: it reflects over the slice and puts every element
	// through an interface. Sixteen strings cost 551ns and 16
	// allocations through fmt against 59ns and none here.
	//
	// They are written out rather than routed through one generic helper
	// taking a per-element append func. That was measured: the indirect
	// call survives instantiation and costs 43% on []string and 13% on
	// []int, which is most of what the fast path buys.
	case []string:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = append(b, e...)
		}
		return append(b, ']')
	case []int:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = strconv.AppendInt(b, int64(e), 10)
		}
		return append(b, ']')
	case []int64:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = strconv.AppendInt(b, e, 10)
		}
		return append(b, ']')
	case []float64:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = strconv.AppendFloat(b, e, 'g', -1, 64)
		}
		return append(b, ']')
	case []bool:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = strconv.AppendBool(b, e)
		}
		return append(b, ']')
	case []byte:
		// %v on a []byte prints its NUMBERS, not its text: fmt reserves
		// the text spelling for %s. []uint8 and []int32 below are the
		// same shape as []int, and separate cases only because Go's type
		// switch matches on the exact element type.
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = strconv.AppendUint(b, uint64(e), 10)
		}
		return append(b, ']')
	case []rune:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = strconv.AppendInt(b, int64(e), 10)
		}
		return append(b, ']')
	case []any:
		// The elements are already interfaces, so recursing costs no
		// boxing -- and it is what makes a []any holding a nested struct
		// print as the struct rather than through fmt's Stringer call.
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = appendValue(b, e)
		}
		return append(b, ']')
	case []error:
		// Same shape as []any. An element still reaches fmt one at a
		// time -- an error renders through Error(), which this has no
		// fast path for -- so what this case saves is the reflect walk
		// over the slice, not the per-element format.
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ' ')
			}
			b = appendValue(b, e)
		}
		return append(b, ']')
	}

	// A []P holds P's MINTED element type (store.go), made at runtime, so
	// there can be no static case for it. fmt renders it correctly --
	// the minted type is a Stringer, which is the whole point of minting
	// it -- and pays a String() and its own boxing per element: 1043ns
	// and 32 allocations for sixteen one-field structs.
	//
	// Reaching the *StructVal directly costs neither. The two Field hops
	// are the ones fromStore takes -- minted type, then carrier, then the
	// struct -- and the Interface() is free because what it boxes is a
	// POINTER, the shape property store.go's whole design already rests
	// on. A nil element lands on appendTo's nil check and prints
	// "<nil>", which is where ScriptStruct.String would have taken it.
	//
	// The Kind guard comes first so that everything else reaching here --
	// a nested slice, a scalar-keyed map, a type from a stdlib call --
	// pays one comparison, not a map lookup, on its way to fmt.
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice:
		if storeOwnerOf(rv.Type().Elem()) != nil {
			b = append(b, '[')
			for i, n := 0, rv.Len(); i < n; i++ {
				if i > 0 {
					b = append(b, ' ')
				}
				sv, _ := rv.Index(i).Field(0).Field(0).Interface().(*StructVal)
				b = sv.appendTo(b)
			}
			return append(b, ']')
		}
	case reflect.Map:
		if out, ok := appendStructKeyedMap(b, rv); ok {
			return out
		}
	}
	return fmt.Appendf(b, "%v", v)
}

// appendStructKeyedMap renders a map[P]V the way %v would, or declines.
//
// A SCALAR-KEYED MAP IS NOT WORTH TAKING OFF fmt, and that was measured
// twice before this function was narrowed to struct keys. A reflect walk
// that renders a map[string]int end to end is 1.5x faster than fmt and
// allocates exactly as much (129 allocations at 64 entries, both), because
// reflect.Value.MapKeys mints a Value per key and MapIndex mints one per
// lookup -- the same two allocations fmt pays. Restructuring it to
// SetIterKey into one preallocated slice makes the count flat in n and
// costs about ten allocations to set up, which LOSES at three entries.
// Small maps are the common case in a shell, so both shapes were dropped.
//
// A STRUCT KEY IS A DIFFERENT SHAPE OF COST. fmt renders one by calling
// the minted type's promoted String, which decodes the key into a fresh
// StructVal and builds a string, and it ORDERS them with a comparison
// that walks reflect down four levels per compare, n log n times. Per
// entry, ns, Apple M3:
//
//	entries              3     16     64
//	 map[string]int   150.4  135.4  171.7
//	 map[P]int        301.7  609.1  931.2
//
// Everything that gap is made of is work grsh already does better: it has
// an arena that decodes a whole map's keys into one slab, and it can
// compare decoded fields directly instead of walking reflect per compare.
//
// WHAT IT MUST NOT DO IS INVENT AN ORDER. fmt sorts map keys through
// internal/fmtsort, and several of that package's rules -- pointers,
// channels, and interfaces holding different concrete types -- order by
// MACHINE ADDRESS, which no reproduction can predict. So this declines
// rather than guesses: an unmatched key type, two keys carrying different
// *StructTypes, a field holding two different dynamic types across keys,
// or a field type the comparator does not know. A decline costs a wasted
// decode and hands the whole map back to fmt, which is correct by
// definition. TestAStructKeyedMapMatchesFmt drives all four.
func appendStructKeyedMap(b []byte, rv reflect.Value) ([]byte, bool) {
	st := keyOwnerOf(rv.Type().Key())
	if st == nil {
		return b, false
	}
	n := rv.Len()
	if n == 0 {
		// Covers the nil map too: fmt prints both as map[].
		return append(b, "map[]"...), true
	}

	// PASS ONE: decode every key and render every value, both into
	// storage owned by the map rather than by the entry -- the same move
	// sortMapKeys made, for the same reason. The key TEXT is not built
	// here: unlike sortMapKeys this does not order by it, so rendering a
	// key before the order is known would be a render nothing reads.
	//
	// The two scratch Values are what keep this off reflect's per-entry
	// allocation. SetIterKey and SetIterValue write into storage the
	// caller owns; MapKeys and MapIndex would mint a fresh Value, and
	// that pair is most of what fmt pays per entry.
	var arena *keyArena
	if n > 2 {
		// Two keys ask for no arena: two slabs cost more than the two
		// fused blocks they replace. Three is where it turns, which is
		// sortMapKeys' threshold and BenchmarkMapKeyArena's measurement.
		arena = newKeyArena(st, n)
	}
	ents := make([]mapEntry, n)
	var vals []byte
	kscratch := reflect.New(rv.Type().Key()).Elem()
	vscratch := reflect.New(rv.Type().Elem()).Elem()
	vr := mapValRender(rv.Type().Elem())
	iter := rv.MapRange()
	i := 0
	for ; i < n && iter.Next(); i++ {
		kscratch.SetIterKey(iter)
		vscratch.SetIterValue(iter)
		from := len(vals)
		vals = appendMapValue(vals, vscratch, vr)
		ents[i] = mapEntry{key: decodeMintedKey(kscratch, arena), from: from, to: len(vals)}
	}
	// Len is read before the walk, so a map mutated underneath this would
	// otherwise leave zeroed entries to render as "<nil>:". fmtsort takes
	// the same care for the same reason; the runtime is what complains
	// about the mutation itself. No test reaches this -- producing the
	// race on purpose is what it would take -- so it is belt, and said to
	// be belt rather than left to look load-bearing.
	ents = ents[:i]

	// PASS TWO: order. The comparator records a decline rather than
	// returning one, which is what lets it mirror fmtsort's own recursive
	// shape instead of threading an error through it; a declined sort
	// produces a meaningless order, and the whole result is thrown away.
	var c keyCmp
	slices.SortStableFunc(ents, func(x, y mapEntry) int { return c.keys(x.key, y.key) })
	if c.declined {
		return b, false
	}

	b = append(b, "map["...)
	for i, e := range ents {
		if i > 0 {
			b = append(b, ' ')
		}
		b = e.key.appendTo(b)
		b = append(b, ':')
		b = append(b, vals[e.from:e.to]...)
	}
	return append(b, ']'), true
}

// valRender says how one map's VALUES are rendered. It is decided once
// per map, not per entry, because a map's value type does not vary.
//
// It exists to keep an interface box off the per-entry path. The general
// answer is appendValue, which needs a Value -- and turning a
// reflect.Value into one costs an allocation for anything that is not
// pointer-shaped, which is an int, a bool, a float and most of what a
// script puts in a map. Reading the scalar straight off the reflect.Value
// halves the allocations of rendering a struct-keyed map, and a minted
// struct value still goes the general way, where the box is free because
// what it boxes is a pointer.
type valRender uint8

const (
	valAny valRender = iota // appendValue, via one interface box
	valString
	valInt
	valUint
	valFloat64
	valBool
)

// mapValRender picks the renderer for a map's value type.
//
// IT MATCHES ON THE TYPE, NOT THE KIND, and that distinction is the whole
// correctness argument. %v on a NAMED integer type calls its String
// method if it has one -- fmt prints time.Duration(3e9) as "3s" and
// time.Month(3) as "March" -- so a switch on reflect.Int would render
// those as bare numbers and quietly disagree with fmt. Type identity
// admits only the predeclared types, which have no methods, and every
// named type falls through to appendValue, whose own type switch is
// exact for the same reason.
func mapValRender(t reflect.Type) valRender {
	switch t {
	case stringT:
		return valString
	case intT, int64T, runeT:
		return valInt
	case byteT:
		return valUint
	case float64T:
		return valFloat64
	case boolT:
		return valBool
	}
	return valAny
}

// The predeclared types a map value can be rendered from directly. Named
// once so mapValRender's switch is a pointer comparison per map rather
// than a reflect.TypeFor call per entry.
var (
	stringT  = reflect.TypeFor[string]()
	intT     = reflect.TypeFor[int]()
	int64T   = reflect.TypeFor[int64]()
	runeT    = reflect.TypeFor[rune]()
	byteT    = reflect.TypeFor[byte]()
	float64T = reflect.TypeFor[float64]()
	boolT    = reflect.TypeFor[bool]()
)

// appendMapValue renders one map value, and must produce byte for byte
// what appendValue produces for the same value.
// TestMapValueRenderMatchesAppendValue holds it to that.
//
// float32 is deliberately absent: %v on one is %g at the shortest
// precision that round-trips a 32-BIT float, and rv.Float() widens to 64,
// so the obvious case would print 0.1 as 0.10000000149011612. It falls
// through to appendValue, which falls through to fmt, which is right.
func appendMapValue(b []byte, rv reflect.Value, vr valRender) []byte {
	switch vr {
	case valString:
		return append(b, rv.String()...)
	case valInt:
		return strconv.AppendInt(b, rv.Int(), 10)
	case valUint:
		return strconv.AppendUint(b, rv.Uint(), 10)
	case valFloat64:
		return strconv.AppendFloat(b, rv.Float(), 'g', -1, 64)
	case valBool:
		return strconv.AppendBool(b, rv.Bool())
	}
	// fromStore is what unwraps a minted struct value; everything else is
	// itself.
	return appendValue(b, fromStore(rv))
}

// mapEntry is one decoded key and the bounds of its value's text inside
// the slab every value of the map was rendered into. Bounds rather than a
// []byte view, for the reason sortMapKeys writes out: a view taken while
// the slab is still growing would point into a reallocated array.
type mapEntry struct {
	key      *StructVal
	from, to int
}

// keyCmp orders decoded struct keys the way fmt orders the encoded ones,
// and records when it cannot.
//
// IT MIRRORS internal/fmtsort's compare, which is an unexported detail of
// the standard library with no compatibility promise. That is a real
// coupling and it is held in place by a test, not by hope:
// TestAStructKeyedMapMatchesFmt renders randomised maps both ways and
// requires the bytes to be equal, so a Go release that reorders map
// printing fails loudly here instead of quietly disagreeing with fmt.
//
// The walk fmtsort does over an encoded key is
//
//	minted struct -> ScriptKey -> StructKey{T *StructType, F any}
//
// so T, a POINTER, is compared before anything else, and F is an
// interface holding the [N]any of field values. Everything below is that
// walk rewritten over the DECODED struct, which holds the same
// information with the indirections already paid.
type keyCmp struct{ declined bool }

// keys compares two decoded keys: fmtsort's T-then-F, in that order.
func (c *keyCmp) keys(a, b *StructVal) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		// A nil key encodes the zero StructKey, whose T is the nil
		// pointer and whose F is the nil interface. fmtsort puts a nil
		// pointer at address 0 and a nil interface low, and both agree:
		// the nil key sorts first. This is the one address comparison
		// that IS predictable, because no *StructType lives at 0.
		return -1
	case b == nil:
		return 1
	case a.Type != b.Type:
		// Two live *StructTypes, ordered by their addresses. Reachable:
		// a re-declared P mints one storage type but keeps a StructType
		// per declaration, so one map can hold keys from both.
		c.declined = true
		return 0
	}
	// T ties, so F decides. Both sides are the same [N]any type -- one
	// StructType fixes N -- so fmtsort's type comparison on F ties too
	// and it goes straight to the elements.
	for i := range a.Vals {
		if r := c.field(a.Vals[i], b.Vals[i]); r != 0 {
			return r
		}
	}
	return 0
}

// field compares one field of a key: fmtsort's interface case, which is
// nil-low, then the concrete TYPE by address, then the value.
//
// The type-by-address step is why a field holding two different types
// across two keys declines. It is not a hypothetical -- a field is
// dynamically typed, so `P{A: 1}` and `P{A: "1"}` are both legal keys of
// one map -- and fmt's own order for that pair changes between builds.
func (c *keyCmp) field(a, b Value) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	// The cases are the key-position half of appendValue's: a field that
	// is part of a key has already passed keyValue's comparability check,
	// so no slice, map or func can reach here.
	switch x := a.(type) {
	case string:
		if y, ok := b.(string); ok {
			return cmp.Compare(x, y)
		}
	case int:
		if y, ok := b.(int); ok {
			return cmp.Compare(x, y)
		}
	case int64:
		if y, ok := b.(int64); ok {
			return cmp.Compare(x, y)
		}
	case rune: // int32
		if y, ok := b.(rune); ok {
			return cmp.Compare(x, y)
		}
	case byte: // uint8
		if y, ok := b.(byte); ok {
			return cmp.Compare(x, y)
		}
	case float64:
		if y, ok := b.(float64); ok {
			// cmp.Compare puts NaN below every other float, which is
			// fmtsort's rule too. Two NaN keys compare EQUAL and are
			// still distinct map keys, so their relative order is the
			// map's own randomised one -- under fmt as well. That is the
			// only tie a distinct pair of keys can produce, which makes
			// the stable sort above a mirror of fmtsort rather than
			// something behaviour can tell apart: swapping it for an
			// unstable one changes no output any test can pin.
			return cmp.Compare(x, y)
		}
	case bool:
		if y, ok := b.(bool); ok {
			switch {
			case x == y:
				return 0
			case x:
				return 1
			default:
				return -1
			}
		}
	case *StructVal:
		// A nested struct field went into the key as its own StructKey,
		// so fmtsort recurses into T-then-F exactly as at the top level.
		// A typed nil *StructVal is NOT the nil case above -- that one is
		// a nil interface -- and keys answers for it.
		if y, ok := b.(*StructVal); ok {
			return c.keys(x, y)
		}
	}
	c.declined = true
	return 0
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
	t.keyArrZero = mintKeyArrZero(t.keyArr)
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
	out := newStructVal(t, len(t.Fields))
	copy(out.Vals, t.Zero)
	if t.structFields {
		for i, v := range out.Vals {
			if sv, ok := v.(*StructVal); ok {
				out.Vals[i] = sv.copyStruct()
			}
		}
	}
	return out
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
	out := newStructVal(sv.Type, len(sv.Vals))
	for i, v := range sv.Vals {
		out.Vals[i] = copyOnStore(v)
	}
	return out
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
