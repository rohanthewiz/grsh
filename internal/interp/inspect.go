package interp

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// inspectMaxItems bounds how many slice/map entries the inspector prints
// before eliding — a REPL convenience must never dump megabytes.
const inspectMaxItems = 20

// inspectMaxWidth bounds how WIDE one printed row gets.
//
// The item cap alone bounds only the row COUNT, which is half a budget:
// twenty rows of a megabyte each is still twenty megabytes. Both bounds
// are needed for the "never dump megabytes" intent above to hold, and
// every rendering path below goes through one of the two helpers that
// enforce this one.
const inspectMaxWidth = 60

// inspectMaxString is the wider budget for a string inspected AS the
// top-level value (`?s`), where seeing the content is the whole point of
// asking. It is not unbounded, because a variable holding a fetched page
// or a read file is exactly the case that would scroll the terminal into
// oblivion. The `(len N)` already printed alongside makes the elision
// unambiguous: the reader can tell how much was withheld.
const inspectMaxString = 2048

// Inspect pretty-prints a top-level variable's type and value for the
// REPL's `?name` command. Only the interpreter has typed Go values at a
// prompt, so this is grsh's counterpart to a debugger's variable view.
func (in *Interp) Inspect(name string) (string, bool) {
	v, ok := in.globals.Get(name)
	if !ok {
		return "", false
	}
	return inspectValue(name, v), true
}

func inspectValue(name string, v Value) string {
	switch t := v.(type) {
	case nil:
		return name + " = nil"
	case *Closure:
		return name + " = " + t.String() + signatureOf(t)
	case *StructVal:
		if t == nil {
			return name + " = nil"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s {\n", name, t.Type.Name)
		w := maxWidth(t.Type.Fields)
		for i, f := range t.Type.Fields {
			fmt.Fprintf(&b, "  %-*s  %s\n", w, f, oneLine(t.Vals[i]))
		}
		b.WriteString("}")
		return b.String()
	case string:
		return fmt.Sprintf("%s: string (len %d) = %s", name, len(t), quoteBounded(t, inspectMaxString))
	case error:
		return fmt.Sprintf("%s: error = %v", name, t)
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s (len %d) [\n", name, displayType(v), rv.Len())
		n := rv.Len()
		if n > inspectMaxItems {
			n = inspectMaxItems
		}
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "  %3d: %s\n", i, oneLine(fromStore(rv.Index(i))))
		}
		if rv.Len() > n {
			fmt.Fprintf(&b, "  ... %d more\n", rv.Len()-n)
		}
		b.WriteString("]")
		return b.String()
	case reflect.Map:
		keys := rv.MapKeys()
		strs := make([]string, len(keys))
		for i, k := range keys {
			strs[i] = oneLine(k.Interface())
		}
		sort.Sort(&keySorter{strs, keys})
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s (len %d) {\n", name, displayType(v), rv.Len())
		w := maxWidth(strs)
		n := len(keys)
		if n > inspectMaxItems {
			n = inspectMaxItems
		}
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "  %-*s: %s\n", w, strs[i], oneLine(fromStore(rv.MapIndex(keys[i]))))
		}
		if len(keys) > n {
			fmt.Fprintf(&b, "  ... %d more\n", len(keys)-n)
		}
		b.WriteString("}")
		return b.String()
	}
	return fmt.Sprintf("%s: %s = %v", name, displayType(v), v)
}

// displayType names a value's type the way the script spells it.
//
// A script struct has no reflect.Type of its own (see TypeDesc), so a
// []Job arrives here as []*interp.StructVal — grsh's own internals, which
// a variable view must never print. The name is recovered from an
// ELEMENT, since every instance carries its StructType; an empty
// container is the one case where nothing on the value knows, and it
// falls back to the neutral word.
//
// One level of nesting is handled, because that is what the inspector
// prints a header for. A [][]Job still falls through to %T: deeper
// shapes are rendered row by row below, where each row is a value that
// does know its own name.
//
// The element side is now answered by the TYPE, since a container holds
// its struct's own minted type (see store.go) -- so an EMPTY []Job reads
// as []Job rather than the neutral word it used to fall back to. Reading
// the name off an instance survives as the fallback for a container that
// still holds the bare erasure, which convertTo can still produce.
func displayType(v Value) string {
	if sv, ok := v.(*StructVal); ok {
		if sv == nil {
			return "struct"
		}
		return sv.Type.Name
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if name, ok := elemStructName(rv, rv.Type().Elem()); ok {
			return "[]" + name
		}
	case reflect.Map:
		kt, et := rv.Type().Key(), rv.Type().Elem()
		val, valIsStruct := elemStructName(rv, et)
		if kt == structKeyType || valIsStruct {
			key := kt.String()
			if kt == structKeyType {
				key = structNameInKeys(rv)
			}
			if !valIsStruct {
				val = scriptTypeName(et)
			}
			return "map[" + key + "]" + val
		}
	}
	return fmt.Sprintf("%T", v)
}

// elemStructName names the script struct a container's elements hold, and
// reports whether they hold one at all.
func elemStructName(rv reflect.Value, et reflect.Type) (string, bool) {
	if st := storeOwnerOf(et); st != nil {
		return st.Name, true
	}
	if et == structValType {
		return structNameIn(rv), true
	}
	return "", false
}

// structNameIn reads the struct type's name off the first non-nil entry
// of a container whose element type has erased to *StructVal.
func structNameIn(rv reflect.Value) string {
	elems := func(yield func(reflect.Value) bool) {
		if rv.Kind() == reflect.Map {
			for _, k := range rv.MapKeys() {
				if !yield(rv.MapIndex(k)) {
					return
				}
			}
			return
		}
		for i := 0; i < rv.Len(); i++ {
			if !yield(rv.Index(i)) {
				return
			}
		}
	}
	name := "struct"
	elems(func(e reflect.Value) bool {
		if sv, ok := fromStore(e).(*StructVal); ok && sv != nil {
			name = sv.Type.Name
			return false
		}
		return true
	})
	return name
}

// structNameInKeys is structNameIn for the KEY side of a struct-keyed
// map. The name is read off a key rather than off the map's type for the
// same reason: every script struct erases to the one StructKey type, and
// only an instance knows which struct it came from.
func structNameInKeys(rv reflect.Value) string {
	for _, k := range rv.MapKeys() {
		if sk, ok := k.Interface().(StructKey); ok && sk.T != nil {
			return sk.T.Name
		}
	}
	return "struct"
}

// signatureOf renders a closure's parameter list from its AST.
func signatureOf(c *Closure) string {
	if c.Fn == nil || c.Fn.Type == nil {
		return "()"
	}
	var parts []string
	for _, p := range flattenParams(c.Fn.Type.Params) {
		n := p.name
		if p.variadic {
			n = "..." + n
		}
		parts = append(parts, n)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// oneLine renders a value compactly for table rows. Every branch ends in
// a width bound: strings through quoteBounded, everything else through
// truncated.
func oneLine(v Value) string {
	switch t := v.(type) {
	case nil:
		return "nil"
	case string:
		return quoteBounded(t, inspectMaxWidth)
	case *StructVal:
		return truncated(t.String(), inspectMaxWidth)
	case *Closure:
		return truncated(t.String(), inspectMaxWidth)
	}
	return truncated(fmt.Sprint(v), inspectMaxWidth)
}

// truncated bounds an already-rendered value to max runes, spending
// three of them on the "..." that marks the cut.
//
// Rune-aware, not byte-aware: a cut landing mid-rune renders as U+FFFD,
// which reads as corrupted data rather than as elision — the inspector
// would be lying about the value in the one place it is meant to be
// describing it.
func truncated(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s { // ranging a string yields rune START offsets
		if n == max-3 {
			return s[:i] + "..."
		}
		n++
	}
	return s
}

// quoteBounded renders s quoted and no wider than max runes.
//
// The cut is taken in the CONTENT and the result re-quoted, so the
// closing quote always survives:
//
//	"abcdef"...    the string starts like this, and there is more
//	"abcd\x        what cutting the QUOTED form can leave: a split
//	               escape and an open quote, unreadable either way
//
// Where the content has to be cut is measured rather than computed,
// because quoting expands by a factor that depends on the bytes (one
// byte can become the four of \xff). The starting prefix is already
// bounded by max, so this shrinks at most max times over a string of at
// most max runes — microseconds, once per row, at a prompt.
func quoteBounded(s string, max int) string {
	q := strconv.Quote(s)
	if utf8.RuneCountInString(q) <= max {
		return q
	}
	rs := []rune(s)
	if len(rs) > max {
		rs = rs[:max]
	}
	for len(rs) > 0 {
		q = strconv.Quote(string(rs))
		if utf8.RuneCountInString(q)+len("...") <= max {
			break
		}
		rs = rs[:len(rs)-1]
	}
	return q + "..."
}

// maxWidth is the padding width for a column of rendered strings.
//
// Counted in RUNES because that is what fmt's `%-*s` pads in; measuring
// in bytes over-pads any row whose key is not pure ASCII, which is the
// alignment the map rendering exists to guarantee.
func maxWidth(ss []string) int {
	w := 0
	for _, s := range ss {
		if n := utf8.RuneCountInString(s); n > w {
			w = n
		}
	}
	return w
}

// keySorter sorts map keys by their rendered form, keeping the reflect
// values in step so output is deterministic.
type keySorter struct {
	strs []string
	keys []reflect.Value
}

func (k *keySorter) Len() int { return len(k.strs) }
func (k *keySorter) Swap(i, j int) {
	k.strs[i], k.strs[j] = k.strs[j], k.strs[i]
	k.keys[i], k.keys[j] = k.keys[j], k.keys[i]
}
func (k *keySorter) Less(i, j int) bool { return k.strs[i] < k.strs[j] }
