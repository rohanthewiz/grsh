package interp

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// inspectMaxItems bounds how many slice/map entries the inspector prints
// before eliding — a REPL convenience must never dump megabytes.
const inspectMaxItems = 20

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
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s {\n", name, t.Type.Name)
		w := maxWidth(t.Type.Fields)
		for i, f := range t.Type.Fields {
			fmt.Fprintf(&b, "  %-*s  %s\n", w, f, oneLine(t.Vals[i]))
		}
		b.WriteString("}")
		return b.String()
	case string:
		return fmt.Sprintf("%s: string (len %d) = %q", name, len(t), t)
	case error:
		return fmt.Sprintf("%s: error = %v", name, t)
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %T (len %d) [\n", name, v, rv.Len())
		n := rv.Len()
		if n > inspectMaxItems {
			n = inspectMaxItems
		}
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "  %3d: %s\n", i, oneLine(rv.Index(i).Interface()))
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
		fmt.Fprintf(&b, "%s: %T (len %d) {\n", name, v, rv.Len())
		w := maxWidth(strs)
		n := len(keys)
		if n > inspectMaxItems {
			n = inspectMaxItems
		}
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "  %-*s: %s\n", w, strs[i], oneLine(rv.MapIndex(keys[i]).Interface()))
		}
		if len(keys) > n {
			fmt.Fprintf(&b, "  ... %d more\n", len(keys)-n)
		}
		b.WriteString("}")
		return b.String()
	}
	return fmt.Sprintf("%s: %T = %v", name, v, v)
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

// oneLine renders a value compactly for table rows.
func oneLine(v Value) string {
	switch t := v.(type) {
	case nil:
		return "nil"
	case string:
		return strconv.Quote(t)
	case *StructVal:
		return t.String()
	case *Closure:
		return t.String()
	}
	s := fmt.Sprint(v)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func maxWidth(ss []string) int {
	w := 0
	for _, s := range ss {
		if len(s) > w {
			w = len(s)
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
