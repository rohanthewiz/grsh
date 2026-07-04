package interp

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"reflect"
	"strconv"

	"github.com/rohanthewiz/grsh/internal/shellexec"
	"github.com/rohanthewiz/grsh/internal/shellparse"
	"github.com/rohanthewiz/grsh/internal/stdlibreg"
	"github.com/rohanthewiz/serr"
)

const maxCallDepth = 10_000

// ---- the shell bridge ----

// runShellStmt executes a statement-position __shell(n): stdio inherited,
// errexit honored.
func (in *Interp) runShellStmt(env *Env, call *ast.CallExpr) (control, error) {
	list, err := in.tabEntry(call)
	if err != nil {
		return control{}, err
	}
	status, err := shellexec.Run(in.sh, list, &wordEval{in: in, env: env}, in.stdio)
	if err != nil {
		return control{}, err // ExitErr and internal errors pass through
	}
	if in.sh.ErrExit && status != 0 {
		// bash `set -e` semantics: exit silently with the failing status.
		return control{}, shellexec.ExitErr{Code: status}
	}
	return control{}, nil
}

// evalCapture runs $(...): buffered stdout, trailing newlines trimmed.
// Yields (output, error) — single-value contexts drop the error.
func (in *Interp) evalCapture(env *Env, call *ast.CallExpr) ([]Value, error) {
	list, err := in.tabEntry(call)
	if err != nil {
		return nil, err
	}
	out, status, err := shellexec.Capture(in.sh, list, &wordEval{in: in, env: env})
	if err != nil {
		return nil, err
	}
	var cmdErr Value
	if status != 0 {
		cmdErr = serr.New("command failed", "status", strconv.Itoa(status), "loc", in.pos(call))
	}
	return []Value{out, cmdErr}, nil
}

// tabEntry resolves __shell(n)/__capture(n) to its side-table fragment.
func (in *Interp) tabEntry(call *ast.CallExpr) (*shellparse.CmdList, error) {
	if len(call.Args) != 1 {
		return nil, in.errAt(call, "internal: malformed shell reference")
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return nil, in.errAt(call, "internal: malformed shell reference")
	}
	i, err := strconv.Atoi(lit.Value)
	if err != nil || i < 0 || i >= len(in.tab) {
		return nil, in.errAt(call, "internal: shell table index out of range")
	}
	return in.tab[i], nil
}

// wordEval lets shellexec evaluate {expr} interpolations in the
// environment current at the moment the shell fragment runs.
type wordEval struct {
	in  *Interp
	env *Env
}

func (w *wordEval) EvalGoExpr(src string) ([]string, error) {
	e, err := goparser.ParseExpr(src)
	if err != nil {
		return nil, serr.Wrap(err, "in", "{"+src+"}")
	}
	v, err := w.in.eval1(w.env, e)
	if err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case string:
		return []string{t}, nil
	case []string:
		return t, nil
	default:
		return []string{stringOf(v)}, nil
	}
}

// ---- calls ----

func (in *Interp) evalCall(env *Env, call *ast.CallExpr) ([]Value, error) {
	switch fn := call.Fun.(type) {
	case *ast.ParenExpr:
		inner := *call
		inner.Fun = fn.X
		return in.evalCall(env, &inner)

	case *ast.Ident:
		// Interpreter intrinsics.
		switch fn.Name {
		case "__shell":
			ctl, err := in.runShellStmt(env, call)
			_ = ctl
			return []Value{in.sh.LastStatus}, err
		case "__capture":
			return in.evalCapture(env, call)
		case "__import":
			return nil, in.evalImport(call)
		}
		// Go builtins and conversions apply unless shadowed.
		if _, shadowed := env.Get(fn.Name); !shadowed {
			if vals, handled, err := in.goBuiltin(env, fn.Name, call); handled {
				return vals, err
			}
			if v, handled, err := in.conversion(env, fn.Name, call); handled {
				return []Value{v}, err
			}
		}
		v, ok := env.Get(fn.Name)
		if !ok {
			return nil, in.errAt(fn, "undefined: "+fn.Name)
		}
		return in.callValue(env, call, v, fn.Name)

	case *ast.SelectorExpr:
		// Package function: fmt.Println(...)
		if pkg, ok := fn.X.(*ast.Ident); ok {
			if _, bound := env.Get(pkg.Name); !bound {
				if sym, ok := in.lookupPkg(pkg.Name, fn.Sel.Name); ok {
					return in.callValue(env, call, sym, pkg.Name+"."+fn.Sel.Name)
				}
				if stdlibreg.Has(pkg.Name) {
					return nil, in.errAt(fn, fmt.Sprintf("%s.%s is not in the grsh registry", pkg.Name, fn.Sel.Name),
						"hint", "the curated stdlib surface grows per milestone")
				}
			}
		}
		// Method on a value via reflection.
		recv, err := in.eval1(env, fn.X)
		if err != nil {
			return nil, err
		}
		m := reflect.ValueOf(recv).MethodByName(fn.Sel.Name)
		if !m.IsValid() {
			return nil, in.errAt(fn, fmt.Sprintf("unknown method %s on %T", fn.Sel.Name, recv))
		}
		return in.callValue(env, call, m.Interface(), fn.Sel.Name)

	case *ast.FuncLit:
		return in.callValue(env, call, &Closure{Fn: fn, Env: env}, "func literal")

	default:
		return nil, in.errAt(call, fmt.Sprintf("cannot call %T", call.Fun))
	}
}

func (in *Interp) evalImport(call *ast.CallExpr) error {
	if len(call.Args) != 1 {
		return in.errAt(call, "import needs one path")
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok {
		return in.errAt(call, "import needs a string path")
	}
	path, _ := strconv.Unquote(lit.Value)
	// Accept both "path/filepath" and "filepath" spellings.
	short := path
	if i := lastSlash(path); i >= 0 {
		short = path[i+1:]
	}
	if !stdlibreg.Has(short) {
		return in.errAt(call, "package "+path+" is not available in grsh",
			"hint", "available: "+fmt.Sprint(stdlibreg.Names()))
	}
	return nil
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func (in *Interp) lookupPkg(pkg, sym string) (any, bool) {
	if v, ok := stdlibreg.LookupBound(pkg, sym, in.stdio.Out, in.stdio.Err); ok {
		return v, true
	}
	return stdlibreg.Lookup(pkg, sym)
}

func (in *Interp) evalArgs(env *Env, call *ast.CallExpr) ([]Value, error) {
	var args []Value
	for _, a := range call.Args {
		v, err := in.eval1(env, a)
		if err != nil {
			return nil, err
		}
		args = append(args, v)
	}
	return args, nil
}

func (in *Interp) callValue(env *Env, call *ast.CallExpr, fn Value, name string) ([]Value, error) {
	if call.Ellipsis.IsValid() {
		return nil, in.errAt(call, "spread calls (xs...) are not supported yet")
	}
	args, err := in.evalArgs(env, call)
	if err != nil {
		return nil, err
	}
	if cl, ok := fn.(*Closure); ok {
		return in.callClosure(call, cl, args)
	}
	return in.callReflect(call, fn, args, name)
}

func (in *Interp) callClosure(call ast.Node, cl *Closure, args []Value) ([]Value, error) {
	if in.depth >= maxCallDepth {
		return nil, in.errAt(call, "call depth exceeded (runaway recursion?)")
	}
	in.depth++
	defer func() { in.depth-- }()

	scope := NewEnv(cl.Env)
	params := flattenParams(cl.Fn.Type.Params)
	variadic := len(params) > 0 && params[len(params)-1].variadic

	fixed := len(params)
	if variadic {
		fixed--
	}
	if (!variadic && len(args) != len(params)) || (variadic && len(args) < fixed) {
		return nil, in.errAt(call, fmt.Sprintf("%s expects %d argument(s), got %d",
			nameOrFunc(cl), len(params), len(args)))
	}
	for i := 0; i < fixed; i++ {
		scope.Define(params[i].name, args[i])
	}
	if variadic {
		rest := make([]Value, 0, len(args)-fixed)
		rest = append(rest, args[fixed:]...)
		scope.Define(params[len(params)-1].name, rest)
	}

	in.pushFrame()
	ctl, err := in.evalStmt(scope, cl.Fn.Body)
	if err = in.popFrame(err); err != nil {
		// Accumulate a script-level call chain in the error fields.
		if _, isExit := err.(shellexec.ExitErr); !isExit {
			err = serr.Wrap(err, "in_func", nameOrFunc(cl))
		}
		return nil, err
	}
	if ctl.kind == ctlReturn {
		return ctl.vals, nil
	}
	return nil, nil
}

func nameOrFunc(cl *Closure) string {
	if cl.Name != "" {
		return cl.Name
	}
	return "function"
}

type param struct {
	name     string
	variadic bool
}

func flattenParams(fl *ast.FieldList) []param {
	var out []param
	if fl == nil {
		return out
	}
	for _, f := range fl.List {
		_, variadic := f.Type.(*ast.Ellipsis)
		if len(f.Names) == 0 {
			out = append(out, param{name: "_", variadic: variadic})
			continue
		}
		for _, n := range f.Names {
			out = append(out, param{name: n.Name, variadic: variadic})
		}
	}
	return out
}

// callReflect is the single boundary through which registered Go
// functions are invoked.
func (in *Interp) callReflect(call ast.Node, fn Value, args []Value, name string) (vals []Value, err error) {
	fv := reflect.ValueOf(fn)
	if !fv.IsValid() || fv.Kind() != reflect.Func {
		return nil, in.errAt(call, name+" is not callable")
	}
	t := fv.Type()
	numIn := t.NumIn()
	if t.IsVariadic() {
		if len(args) < numIn-1 {
			return nil, in.errAt(call, fmt.Sprintf("%s expects at least %d argument(s), got %d", name, numIn-1, len(args)))
		}
	} else if len(args) != numIn {
		return nil, in.errAt(call, fmt.Sprintf("%s expects %d argument(s), got %d", name, numIn, len(args)))
	}

	rargs := make([]reflect.Value, len(args))
	for i, a := range args {
		var pt reflect.Type
		if t.IsVariadic() && i >= numIn-1 {
			pt = t.In(numIn - 1).Elem()
		} else {
			pt = t.In(i)
		}
		rv, cerr := convertTo(a, pt)
		if cerr != nil {
			return nil, in.wrapAt(call, cerr, "arg", strconv.Itoa(i+1), "func", name)
		}
		rargs[i] = rv
	}

	defer func() {
		if r := recover(); r != nil {
			err = in.errAt(call, fmt.Sprintf("panic in %s: %v", name, r))
		}
	}()
	outs := fv.Call(rargs)
	vals = make([]Value, len(outs))
	for i, o := range outs {
		vals[i] = o.Interface()
	}
	return vals, nil
}

// convertTo adapts a script value to a Go parameter type.
func convertTo(v Value, pt reflect.Type) (reflect.Value, error) {
	if v == nil {
		return reflect.Zero(pt), nil
	}
	rv := reflect.ValueOf(v)
	if rv.Type().AssignableTo(pt) {
		return rv, nil
	}
	// int → string would produce a rune-string surprise; refuse it.
	if rv.Kind() != reflect.String && pt.Kind() == reflect.String && rv.CanInt() {
		return reflect.Value{}, serr.New(fmt.Sprintf("cannot use %v (%T) as string (use strconv.Itoa)", v, v))
	}
	if rv.Type().ConvertibleTo(pt) {
		return rv.Convert(pt), nil
	}
	// []any → []T for variadic-ish flexibility.
	if rv.Kind() == reflect.Slice && pt.Kind() == reflect.Slice {
		out := reflect.MakeSlice(pt, rv.Len(), rv.Len())
		for i := 0; i < rv.Len(); i++ {
			ev, err := convertTo(rv.Index(i).Interface(), pt.Elem())
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(ev)
		}
		return out, nil
	}
	return reflect.Value{}, serr.New(fmt.Sprintf("cannot use %v (%T) as %s", v, v, pt))
}

// ---- Go builtins ----

func (in *Interp) goBuiltin(env *Env, name string, call *ast.CallExpr) ([]Value, bool, error) {
	switch name {
	case "len", "cap":
		if len(call.Args) != 1 {
			return nil, true, in.errAt(call, name+" expects one argument")
		}
		v, err := in.eval1(env, call.Args[0])
		if err != nil {
			return nil, true, err
		}
		if v == nil {
			return []Value{0}, true, nil
		}
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.String, reflect.Slice, reflect.Array, reflect.Map, reflect.Chan:
			if name == "cap" && (rv.Kind() == reflect.String || rv.Kind() == reflect.Map) {
				return nil, true, in.errAt(call, "cap is not defined on "+rv.Kind().String())
			}
			if name == "cap" {
				return []Value{rv.Cap()}, true, nil
			}
			return []Value{rv.Len()}, true, nil
		}
		return nil, true, in.errAt(call, fmt.Sprintf("%s is not defined on %T", name, v))

	case "append":
		if len(call.Args) < 1 {
			return nil, true, in.errAt(call, "append expects at least one argument")
		}
		args, err := in.evalArgs(env, call)
		if err != nil {
			return nil, true, err
		}
		base := args[0]
		if base == nil {
			base = []Value{}
		}
		rv := reflect.ValueOf(base)
		if rv.Kind() != reflect.Slice {
			return nil, true, in.errAt(call, fmt.Sprintf("append target must be a slice, got %T", args[0]))
		}
		for _, a := range args[1:] {
			ev, cerr := convertTo(a, rv.Type().Elem())
			if cerr != nil {
				return nil, true, in.wrapAt(call, cerr)
			}
			rv = reflect.Append(rv, ev)
		}
		return []Value{rv.Interface()}, true, nil

	case "delete":
		if len(call.Args) != 2 {
			return nil, true, in.errAt(call, "delete expects a map and a key")
		}
		args, err := in.evalArgs(env, call)
		if err != nil {
			return nil, true, err
		}
		rv := reflect.ValueOf(args[0])
		if rv.Kind() != reflect.Map {
			return nil, true, in.errAt(call, "delete target must be a map")
		}
		kv, cerr := convertTo(args[1], rv.Type().Key())
		if cerr != nil {
			return nil, true, in.wrapAt(call, cerr)
		}
		rv.SetMapIndex(kv, reflect.Value{})
		return nil, true, nil

	case "copy":
		if len(call.Args) != 2 {
			return nil, true, in.errAt(call, "copy expects two slices")
		}
		args, err := in.evalArgs(env, call)
		if err != nil {
			return nil, true, err
		}
		dst, src := reflect.ValueOf(args[0]), reflect.ValueOf(args[1])
		if dst.Kind() != reflect.Slice || src.Kind() != reflect.Slice {
			return nil, true, in.errAt(call, "copy arguments must be slices")
		}
		return []Value{reflect.Copy(dst, src)}, true, nil

	case "min", "max":
		if len(call.Args) == 0 {
			return nil, true, in.errAt(call, name+" expects at least one argument")
		}
		args, err := in.evalArgs(env, call)
		if err != nil {
			return nil, true, err
		}
		best := args[0]
		for _, a := range args[1:] {
			op := token.LSS
			if name == "max" {
				op = token.GTR
			}
			better, berr := in.binaryOp(call, op, a, best)
			if berr != nil {
				return nil, true, berr
			}
			if better == true {
				best = a
			}
		}
		return []Value{best}, true, nil

	case "make":
		if len(call.Args) < 1 {
			return nil, true, in.errAt(call, "make expects a type")
		}
		t, err := in.typeOf(call.Args[0])
		if err != nil {
			return nil, true, err
		}
		var n0, n1 int
		if len(call.Args) > 1 {
			v, err := in.eval1(env, call.Args[1])
			if err != nil {
				return nil, true, err
			}
			i, ok := toI64(v)
			if !ok {
				return nil, true, in.errAt(call, "make length must be an integer")
			}
			n0 = int(i)
		}
		n1 = n0
		if len(call.Args) > 2 {
			v, err := in.eval1(env, call.Args[2])
			if err != nil {
				return nil, true, err
			}
			i, ok := toI64(v)
			if !ok {
				return nil, true, in.errAt(call, "make capacity must be an integer")
			}
			n1 = int(i)
		}
		switch t.Kind() {
		case reflect.Slice:
			return []Value{reflect.MakeSlice(t, n0, n1).Interface()}, true, nil
		case reflect.Map:
			return []Value{reflect.MakeMap(t).Interface()}, true, nil
		}
		return nil, true, in.errAt(call, "make supports slices and maps in grsh v1")
	}
	return nil, false, nil
}

// ---- conversions ----

func (in *Interp) conversion(env *Env, name string, call *ast.CallExpr) (Value, bool, error) {
	var target reflect.Type
	switch name {
	case "int":
		target = reflect.TypeFor[int]()
	case "int64":
		target = reflect.TypeFor[int64]()
	case "float64":
		target = reflect.TypeFor[float64]()
	case "rune":
		target = reflect.TypeFor[rune]()
	case "byte":
		target = reflect.TypeFor[byte]()
	case "string":
		target = reflect.TypeFor[string]()
	default:
		return nil, false, nil
	}
	if len(call.Args) != 1 {
		return nil, true, in.errAt(call, name+" conversion expects one argument")
	}
	v, err := in.eval1(env, call.Args[0])
	if err != nil {
		return nil, true, err
	}
	if name == "string" {
		switch t := v.(type) {
		case string:
			return t, true, nil
		case rune:
			return string(t), true, nil
		case byte:
			return string(t), true, nil
		case []byte:
			return string(t), true, nil
		case int:
			return string(rune(t)), true, nil
		}
		return nil, true, in.errAt(call, fmt.Sprintf("cannot convert %T to string (use fmt.Sprint or strconv)", v))
	}
	rv := reflect.ValueOf(v)
	if rv.IsValid() && rv.Type().ConvertibleTo(target) && rv.Kind() != reflect.String {
		return rv.Convert(target).Interface(), true, nil
	}
	return nil, true, in.errAt(call, fmt.Sprintf("cannot convert %T to %s (use strconv)", v, name))
}

// ---- types & composites ----

var typeIdents = map[string]reflect.Type{
	"int":     reflect.TypeFor[int](),
	"int64":   reflect.TypeFor[int64](),
	"float64": reflect.TypeFor[float64](),
	"string":  reflect.TypeFor[string](),
	"bool":    reflect.TypeFor[bool](),
	"byte":    reflect.TypeFor[byte](),
	"rune":    reflect.TypeFor[rune](),
	"any":     reflect.TypeFor[any](),
	"error":   reflect.TypeFor[error](),
}

func (in *Interp) typeOf(e ast.Expr) (reflect.Type, error) {
	switch t := e.(type) {
	case *ast.Ident:
		if rt, ok := typeIdents[t.Name]; ok {
			return rt, nil
		}
		return nil, in.errAt(e, "unknown type "+t.Name)
	case *ast.ArrayType:
		if t.Len != nil {
			return nil, in.errAt(e, "fixed-size arrays are not supported (use a slice)")
		}
		el, err := in.typeOf(t.Elt)
		if err != nil {
			return nil, err
		}
		return reflect.SliceOf(el), nil
	case *ast.MapType:
		k, err := in.typeOf(t.Key)
		if err != nil {
			return nil, err
		}
		v, err := in.typeOf(t.Value)
		if err != nil {
			return nil, err
		}
		return reflect.MapOf(k, v), nil
	case *ast.InterfaceType:
		if len(t.Methods.List) == 0 {
			return typeIdents["any"], nil
		}
	case *ast.ParenExpr:
		return in.typeOf(t.X)
	}
	return nil, in.errAt(e, fmt.Sprintf("unsupported type %T in grsh v1", e))
}

func (in *Interp) evalComposite(env *Env, n *ast.CompositeLit) (Value, error) {
	if n.Type == nil {
		return nil, in.errAt(n, "composite literal needs an explicit type here")
	}
	if st, ok := lookupStructType(env, n.Type); ok {
		return in.structComposite(env, st, n)
	}
	t, err := in.typeOf(n.Type)
	if err != nil {
		return nil, err
	}
	switch t.Kind() {
	case reflect.Slice:
		out := reflect.MakeSlice(t, 0, len(n.Elts))
		for _, el := range n.Elts {
			v, err := in.eval1(env, el)
			if err != nil {
				return nil, err
			}
			ev, cerr := convertTo(v, t.Elem())
			if cerr != nil {
				return nil, in.wrapAt(el, cerr)
			}
			out = reflect.Append(out, ev)
		}
		return out.Interface(), nil
	case reflect.Map:
		out := reflect.MakeMapWithSize(t, len(n.Elts))
		for _, el := range n.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				return nil, in.errAt(el, "map literal elements need key: value")
			}
			k, err := in.eval1(env, kv.Key)
			if err != nil {
				return nil, err
			}
			v, err := in.eval1(env, kv.Value)
			if err != nil {
				return nil, err
			}
			rk, cerr := convertTo(k, t.Key())
			if cerr != nil {
				return nil, in.wrapAt(kv.Key, cerr)
			}
			rv, cerr := convertTo(v, t.Elem())
			if cerr != nil {
				return nil, in.wrapAt(kv.Value, cerr)
			}
			out.SetMapIndex(rk, rv)
		}
		return out.Interface(), nil
	}
	return nil, in.errAt(n, "unsupported composite literal type in grsh v1")
}
