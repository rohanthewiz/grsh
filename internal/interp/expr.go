package interp

import (
	"bytes"
	"cmp"
	"fmt"
	"go/ast"
	"go/token"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// evalExpr evaluates an expression to its values (calls and __capture are
// natively multi-valued).
func (in *Interp) evalExpr(env *Env, e ast.Expr) ([]Value, error) {
	switch n := e.(type) {
	case *ast.BasicLit:
		v, err := in.basicLit(n)
		return []Value{v}, err

	case *ast.Ident:
		if v, ok := env.Get(n.Name); ok {
			return []Value{v}, nil
		}
		switch n.Name {
		case "true":
			return []Value{true}, nil
		case "false":
			return []Value{false}, nil
		case "nil":
			return []Value{nil}, nil
		}
		return nil, in.errAt(n, "undefined: "+n.Name)

	case *ast.ParenExpr:
		return in.evalExpr(env, n.X)

	case *ast.UnaryExpr:
		v, err := in.eval1(env, n.X)
		if err != nil {
			return nil, err
		}
		out, err := in.unaryOp(n, n.Op, v)
		return []Value{out}, err

	case *ast.BinaryExpr:
		return in.evalBinary(env, n)

	case *ast.CallExpr:
		return in.evalCall(env, n)

	case *ast.SelectorExpr:
		v, err := in.evalSelector(env, n)
		return []Value{v}, err

	case *ast.IndexExpr:
		v, err := in.evalIndex(env, n)
		return []Value{v}, err

	case *ast.SliceExpr:
		v, err := in.evalSlice(env, n)
		return []Value{v}, err

	case *ast.CompositeLit:
		v, err := in.evalComposite(env, n)
		return []Value{v}, err

	case *ast.FuncLit:
		return []Value{&Closure{Fn: n, Env: env}}, nil

	case *ast.TypeAssertExpr:
		if n.Type == nil {
			return nil, in.errAt(n, "type switches are not supported yet")
		}
		v, err := in.eval1(env, n.X)
		if err != nil {
			return nil, err
		}
		d, err := in.typeOf(env, n.Type)
		if err != nil {
			return nil, err
		}
		// Every script struct erases to the same *StructVal, so
		// AssignableTo would answer yes for x.(P) whatever struct x
		// actually holds. Compare the declared type itself instead.
		if d.IsStruct() {
			if sv, ok := v.(*StructVal); ok && sv != nil && sv.Type == d.ST {
				return []Value{v, true}, nil
			}
			return []Value{d.Zero(), false}, nil
		}
		// A container answers on its TYPE alone, at both leaves: its
		// element type and its key type are each minted per struct, so
		// []P and []Q — and map[P]int and map[Q]int — are genuinely
		// different reflect.Types. This used to need a walk over the
		// values to decide the key leaf, which could not answer for an
		// EMPTY map; the type answers for one now.
		if v != nil && reflect.TypeOf(v).AssignableTo(d.RT) {
			return []Value{v, true}, nil
		}
		return []Value{d.Zero(), false}, nil

	default:
		return nil, in.errAt(e, fmt.Sprintf("%T expression is not supported yet", e))
	}
}

// eval1 evaluates to exactly one value (extra values, e.g. the error from
// $(...), are dropped in single-value contexts).
func (in *Interp) eval1(env *Env, e ast.Expr) (Value, error) {
	vals, err := in.evalExpr(env, e)
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, in.errAt(e, "expression has no value")
	}
	// A failed single-value type assertion is an error (Go panics here).
	if ta, ok := e.(*ast.TypeAssertExpr); ok && len(vals) == 2 {
		if okv, isBool := vals[1].(bool); isBool && !okv {
			return nil, in.errAt(ta, "type assertion failed")
		}
	}
	return vals[0], nil
}

func (in *Interp) evalBool(env *Env, e ast.Expr) (bool, error) {
	v, err := in.eval1(env, e)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, in.errAt(e, fmt.Sprintf("condition must be bool, got %s", valTypeName(v)))
	}
	return b, nil
}

func (in *Interp) basicLit(n *ast.BasicLit) (Value, error) {
	switch n.Kind {
	case token.INT:
		i, err := strconv.ParseInt(n.Value, 0, 64)
		if err != nil {
			return nil, in.wrapAt(n, err)
		}
		return int(i), nil
	case token.FLOAT:
		f, err := strconv.ParseFloat(n.Value, 64)
		if err != nil {
			return nil, in.wrapAt(n, err)
		}
		return f, nil
	case token.STRING:
		s, err := strconv.Unquote(n.Value)
		if err != nil {
			return nil, in.wrapAt(n, err)
		}
		return s, nil
	case token.CHAR:
		s, err := strconv.Unquote(n.Value)
		if err != nil {
			return nil, in.wrapAt(n, err)
		}
		return []rune(s)[0], nil
	}
	return nil, in.errAt(n, "unsupported literal "+n.Value)
}

func (in *Interp) evalBinary(env *Env, n *ast.BinaryExpr) ([]Value, error) {
	// Short-circuit logic.
	if n.Op == token.LAND || n.Op == token.LOR {
		l, err := in.evalBool(env, n.X)
		if err != nil {
			return nil, err
		}
		if (n.Op == token.LAND && !l) || (n.Op == token.LOR && l) {
			return []Value{l}, nil
		}
		r, err := in.evalBool(env, n.Y)
		return []Value{r}, err
	}
	x, err := in.eval1(env, n.X)
	if err != nil {
		return nil, err
	}
	y, err := in.eval1(env, n.Y)
	if err != nil {
		return nil, err
	}
	v, err := in.binaryOp(n, n.Op, x, y)
	return []Value{v}, err
}

func (in *Interp) unaryOp(n ast.Node, op token.Token, v Value) (Value, error) {
	switch op {
	case token.NOT:
		b, ok := v.(bool)
		if !ok {
			return nil, in.errAt(n, fmt.Sprintf("operator ! requires bool, got %s", valTypeName(v)))
		}
		return !b, nil
	case token.SUB:
		if i, ok := toI64(v); ok {
			return int(-i), nil
		}
		if f, ok := toF64(v); ok {
			return -f, nil
		}
		return nil, in.errAt(n, fmt.Sprintf("operator - requires a number, got %s", valTypeName(v)))
	case token.ADD:
		return v, nil
	case token.XOR:
		// Bitwise complement: ^x on integers.
		if i, ok := toI64(v); ok {
			return int(^i), nil
		}
		return nil, in.errAt(n, fmt.Sprintf("operator ^ requires an integer, got %s", valTypeName(v)))
	}
	return nil, in.errAt(n, "unary operator "+op.String()+" is not supported")
}

// binaryOp implements arithmetic, comparison and string ops over native
// values. ints stay int; any float operand promotes to float64.
func (in *Interp) binaryOp(n ast.Node, op token.Token, x, y Value) (Value, error) {
	// String ops.
	xs, xok := x.(string)
	ys, yok := y.(string)
	if xok && yok {
		switch op {
		case token.ADD:
			return xs + ys, nil
		case token.EQL:
			return xs == ys, nil
		case token.NEQ:
			return xs != ys, nil
		case token.LSS:
			return xs < ys, nil
		case token.LEQ:
			return xs <= ys, nil
		case token.GTR:
			return xs > ys, nil
		case token.GEQ:
			return xs >= ys, nil
		}
		return nil, in.errAt(n, "operator "+op.String()+" is not defined on strings")
	}

	// Bool equality.
	if xb, ok := x.(bool); ok {
		if yb, ok := y.(bool); ok {
			switch op {
			case token.EQL:
				return xb == yb, nil
			case token.NEQ:
				return xb != yb, nil
			}
		}
	}

	// Numeric ops: pure-int stays int; otherwise promote to float64.
	xi, xIsInt := toI64(x)
	yi, yIsInt := toI64(y)
	if xIsInt && yIsInt {
		return intOp(in, n, op, xi, yi)
	}
	xf, xIsNum := toF64(x)
	yf, yIsNum := toF64(y)
	if xIsNum && yIsNum {
		return floatOp(in, n, op, xf, yf)
	}

	// Script structs compare FIELD-WISE, so they have to be intercepted
	// before the fallback below -- a *StructVal is a pointer, and the
	// fallback's `x == y` would compare two identities and call every
	// separately-built pair of equal structs unequal.
	//
	// Ordering: this sits last among the typed cases because a struct is
	// none of the kinds above, so no hot path pays for the two assertions.
	// Non-equality operators fall THROUGH to the message at the bottom;
	// there is no ordering on structs, in Go or here.
	_, xIsStruct := x.(*StructVal)
	_, yIsStruct := y.(*StructVal)
	if (xIsStruct || yIsStruct) && (op == token.EQL || op == token.NEQ) {
		eq, err := in.valuesEqual(n, x, y)
		if err != nil {
			return nil, err
		}
		return eq == (op == token.EQL), nil
	}

	// Fallback equality for everything else (nil, errors, slices...).
	switch op {
	case token.EQL:
		return safeEqual(x, y), nil
	case token.NEQ:
		return !safeEqual(x, y), nil
	}
	// %T would print grsh's own *interp.StructVal here, at the one place
	// the message is meant to describe the script's own values.
	return nil, in.errAt(n, fmt.Sprintf("operator %s is not defined on %s and %s",
		op, valTypeName(x), valTypeName(y)))
}

func intOp(in *Interp, n ast.Node, op token.Token, x, y int64) (Value, error) {
	switch op {
	case token.ADD:
		return int(x + y), nil
	case token.SUB:
		return int(x - y), nil
	case token.MUL:
		return int(x * y), nil
	case token.QUO:
		if y == 0 {
			return nil, in.errAt(n, "integer division by zero")
		}
		return int(x / y), nil
	case token.REM:
		if y == 0 {
			return nil, in.errAt(n, "integer modulo by zero")
		}
		return int(x % y), nil
	case token.EQL:
		return x == y, nil
	case token.NEQ:
		return x != y, nil
	case token.LSS:
		return x < y, nil
	case token.LEQ:
		return x <= y, nil
	case token.GTR:
		return x > y, nil
	case token.GEQ:
		return x >= y, nil
	case token.AND:
		return int(x & y), nil
	case token.OR:
		return int(x | y), nil
	case token.XOR:
		return int(x ^ y), nil
	case token.SHL:
		if y < 0 {
			return nil, in.errAt(n, "negative shift amount")
		}
		return int(x << uint(y)), nil
	case token.SHR:
		if y < 0 {
			return nil, in.errAt(n, "negative shift amount")
		}
		return int(x >> uint(y)), nil
	case token.AND_NOT:
		return int(x &^ y), nil
	}
	return nil, in.errAt(n, "operator "+op.String()+" is not supported on integers")
}

func floatOp(in *Interp, n ast.Node, op token.Token, x, y float64) (Value, error) {
	switch op {
	case token.ADD:
		return x + y, nil
	case token.SUB:
		return x - y, nil
	case token.MUL:
		return x * y, nil
	case token.QUO:
		return x / y, nil
	case token.EQL:
		return x == y, nil
	case token.NEQ:
		return x != y, nil
	case token.LSS:
		return x < y, nil
	case token.LEQ:
		return x <= y, nil
	case token.GTR:
		return x > y, nil
	case token.GEQ:
		return x >= y, nil
	}
	return nil, in.errAt(n, "operator "+op.String()+" is not supported on floats")
}

// valTypeName is %T for a script-facing message, with the struct erasure
// undone: `operator < is not defined on P and P`, not on two
// *interp.StructVal, at the one place whose whole job is describing the
// script's own values.
//
// The NAME is read off the instance, since reflect.TypeOf sees only the
// erased storage type and could never say which struct it is. A typed nil
// has no instance to read, and falls back to scriptTypeName's neutral
// word.
func valTypeName(v Value) string {
	if v == nil {
		return "<nil>"
	}
	if sv, ok := v.(*StructVal); ok && sv != nil {
		return sv.Type.Name
	}
	return scriptTypeName(reflect.TypeOf(v))
}

// safeEqual is `==` for everything binaryOp's typed cases did not claim:
// nil, errors, slices, maps, and whatever a Go builtin handed back.
func safeEqual(x, y Value) bool {
	if x == nil || y == nil {
		// The `nil` literal arrives as an untyped Go nil, but a nil MAP,
		// SLICE, FUNC or CHAN does NOT: `var n map[string]int` stores
		// what reflect.Zero hands back, which is a non-nil interface
		// wrapping a nil header. Comparing the two interfaces alone
		// therefore answers false for `n == nil` -- so the other side is
		// unwrapped and asked whether it is nil directly.
		return isNilRef(x) && isNilRef(y)
	}
	defer func() { recover() }() //nolint: uncomparable types compare unequal
	return x == y
}

// errorInterface is `error` as a reflect.Type, for the one question
// isNilRef has to ask about a pointer.
var errorInterface = reflect.TypeOf((*error)(nil)).Elem()

// isNilRef reports whether v counts as nil against the `nil` literal:
// either an untyped nil, or one of the kinds whose nil the interpreter
// materializes as a non-nil interface wrapping a nil.
//
// A STRUCT is not among them and does not need to be: a *StructVal never
// reaches safeEqual through `==`, because binaryOp routes it to
// valuesEqual, which already answers true for a typed nil.
func isNilRef(v Value) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return rv.IsNil()

	case reflect.Pointer:
		// A nil pointer is nil, and scripts do get handed one:
		// `re, err := regexp.Compile("(")` leaves re a nil
		// *regexp.Regexp beside its error, and Go calls that nil.
		//
		// UNLESS the pointer's type implements error. Then this is the
		// value grsh's own error rule acts on -- assignRHS and the
		// statement-position abort both detect a failure with
		// `.(error)`, an assertion that SUCCEEDS on a typed nil and
		// treats it as live. Calling it nil here would split the script
		// from its own runtime over a single value: `if err != nil`
		// would step past an error that a one-value call would have
		// aborted the script on. Go's interface rule reaches the same
		// answer, from its own direction.
		//
		// The cut is exactly `.(error)`'s, not a judgement about which
		// pointers matter -- that is what keeps the two in agreement no
		// matter what a callee returns.
		return !rv.Type().Implements(errorInterface) && rv.IsNil()
	}
	return false
}

// toI64 accepts integer-kinded values (int, byte, rune, int64, ...).
func toI64(v Value) (int64, bool) {
	switch t := v.(type) {
	case int:
		return int64(t), true
	case int64:
		return t, true
	case int32:
		return int64(t), true
	case byte:
		return int64(t), true
	case int8, int16, uint, uint16, uint32, uint64:
		rv := reflect.ValueOf(v)
		if rv.CanInt() {
			return rv.Int(), true
		}
		return int64(rv.Uint()), true
	}
	return 0, false
}

func toF64(v Value) (float64, bool) {
	if i, ok := toI64(v); ok {
		return float64(i), true
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	}
	return 0, false
}

// ---- assignment ----

func (in *Interp) assign(env *Env, as *ast.AssignStmt) error {
	// Compound assignment: x += y and friends.
	if op, ok := assignOp(as.Tok); ok {
		if len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return in.errAt(as, "compound assignment needs exactly one operand")
		}
		cur, err := in.eval1(env, as.Lhs[0])
		if err != nil {
			return err
		}
		rhs, err := in.eval1(env, as.Rhs[0])
		if err != nil {
			return err
		}
		nv, err := in.binaryOp(as, op, cur, rhs)
		if err != nil {
			return err
		}
		return in.setLValue(env, as.Lhs[0], nv)
	}

	vals, err := in.assignRHS(env, as)
	if err != nil {
		return err
	}
	if len(vals) != len(as.Lhs) {
		return in.errAt(as, fmt.Sprintf("assignment mismatch: %d variables but %d values", len(as.Lhs), len(vals)))
	}
	for i, lhs := range as.Lhs {
		if as.Tok == token.DEFINE {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				return in.errAt(lhs, ":= target must be an identifier")
			}
			if id.Name == "_" {
				continue
			}
			// A struct RHS is copied into the new binding: `b := a` must
			// give b a struct of its own (copyOnStore).
			env.Define(id.Name, copyOnStore(vals[i]))
			continue
		}
		if err := in.setLValue(env, lhs, vals[i]); err != nil {
			return err
		}
	}
	return nil
}

// assignRHS evaluates the right side, applying the multi-value rules:
//   - out := $(cmd)        → capture string only (status via status())
//   - out, err := $(cmd)   → both
//   - v := f()             → f's (T, error): non-nil error aborts
//   - v, err := f()        → caller handles the error
func (in *Interp) assignRHS(env *Env, as *ast.AssignStmt) ([]Value, error) {
	if len(as.Rhs) != 1 {
		var vals []Value
		for _, r := range as.Rhs {
			v, err := in.eval1(env, r)
			if err != nil {
				return nil, err
			}
			vals = append(vals, v)
		}
		return vals, nil
	}

	rhs := as.Rhs[0]

	// Single-target type assertion: failure is an error, and the ok value
	// must not leak into the assignment.
	if _, isAssert := rhs.(*ast.TypeAssertExpr); isAssert && len(as.Lhs) == 1 {
		v, err := in.eval1(env, rhs)
		if err != nil {
			return nil, err
		}
		return []Value{v}, nil
	}

	// Comma-ok on map lookup: v, ok := m[k]
	if idx, ok := rhs.(*ast.IndexExpr); ok && len(as.Lhs) == 2 {
		container, err := in.eval1(env, idx.X)
		if err != nil {
			return nil, err
		}
		rv := reflect.ValueOf(container)
		if rv.Kind() == reflect.Map {
			key, err := in.eval1(env, idx.Index)
			if err != nil {
				return nil, err
			}
			kv, cerr := convertTo(key, rv.Type().Key())
			if cerr != nil {
				return nil, in.wrapAt(idx, cerr)
			}
			out := rv.MapIndex(kv)
			if out.IsValid() {
				return []Value{fromStore(out), true}, nil
			}
			return []Value{zeroInSlot(rv.Type().Elem()), false}, nil
		}
	}

	vals, err := in.evalExpr(env, rhs)
	if err != nil {
		return nil, err
	}
	if len(vals) == len(as.Lhs) {
		return vals, nil
	}
	if len(vals) == len(as.Lhs)+1 {
		last := vals[len(vals)-1]
		if isCaptureCall(rhs) {
			// $(...) in single-value context never aborts; check status().
			return vals[:len(vals)-1], nil
		}
		if lastErr, ok := last.(error); ok || last == nil {
			if ok && lastErr != nil {
				return nil, in.wrapAt(rhs, lastErr)
			}
			return vals[:len(vals)-1], nil
		}
	}
	return vals, nil
}

func isCaptureCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "__capture"
}

func assignOp(tok token.Token) (token.Token, bool) {
	switch tok {
	case token.ADD_ASSIGN:
		return token.ADD, true
	case token.SUB_ASSIGN:
		return token.SUB, true
	case token.MUL_ASSIGN:
		return token.MUL, true
	case token.QUO_ASSIGN:
		return token.QUO, true
	case token.REM_ASSIGN:
		return token.REM, true
	case token.AND_ASSIGN:
		return token.AND, true
	case token.OR_ASSIGN:
		return token.OR, true
	case token.XOR_ASSIGN:
		return token.XOR, true
	case token.SHL_ASSIGN:
		return token.SHL, true
	case token.SHR_ASSIGN:
		return token.SHR, true
	case token.AND_NOT_ASSIGN:
		return token.AND_NOT, true
	}
	return tok, false
}

// setLValue writes v to an assignable target. Every target here is a
// storage location, so the value is copied on the way in for all three
// of them -- a name, a container slot, and a struct field.
func (in *Interp) setLValue(env *Env, lhs ast.Expr, v Value) error {
	v = copyOnStore(v)
	switch t := lhs.(type) {
	case *ast.Ident:
		if t.Name == "_" {
			return nil
		}
		if !env.Set(t.Name, v) {
			return in.errAt(t, "undefined: "+t.Name+" (use := to declare)")
		}
		return nil
	case *ast.IndexExpr:
		container, err := in.eval1(env, t.X)
		if err != nil {
			return err
		}
		idx, err := in.eval1(env, t.Index)
		if err != nil {
			return err
		}
		return in.setIndexed(t, container, idx, v)
	case *ast.SelectorExpr:
		recv, err := in.eval1(env, t.X)
		if err != nil {
			return err
		}
		if sv, ok := recv.(*StructVal); ok {
			return in.setStructField(t, sv, t.Sel.Name, v)
		}
		return in.errAt(lhs, fmt.Sprintf("cannot assign to field of %s", valTypeName(recv)))
	default:
		return in.errAt(lhs, fmt.Sprintf("cannot assign to %T yet", lhs))
	}
}

func (in *Interp) setIndexed(n ast.Node, container, idx, v Value) error {
	rv := reflect.ValueOf(container)
	switch rv.Kind() {
	case reflect.Map:
		// SetMapIndex panics on a typed nil map (`var m map[string]int`);
		// surface Go's runtime message as a positioned error instead.
		if rv.IsNil() {
			return in.errAt(n, "assignment to entry in nil map (use make or a literal first)")
		}
		kv, err := convertTo(idx, rv.Type().Key())
		if err != nil {
			return in.wrapAt(n, err)
		}
		vv, err := convertTo(v, rv.Type().Elem())
		if err != nil {
			return in.wrapAt(n, err)
		}
		rv.SetMapIndex(kv, vv)
		return nil
	case reflect.Slice:
		i, ok := toI64(idx)
		if !ok {
			return in.errAt(n, "slice index must be an integer")
		}
		if i < 0 || int(i) >= rv.Len() {
			return in.errAt(n, fmt.Sprintf("index out of range [%d] with length %d", i, rv.Len()))
		}
		vv, err := convertTo(v, rv.Type().Elem())
		if err != nil {
			return in.wrapAt(n, err)
		}
		rv.Index(int(i)).Set(vv)
		return nil
	}
	return in.errAt(n, fmt.Sprintf("cannot index-assign into %s", valTypeName(container)))
}

// ---- indexing / slicing / selectors ----

func (in *Interp) evalIndex(env *Env, n *ast.IndexExpr) (Value, error) {
	container, err := in.eval1(env, n.X)
	if err != nil {
		return nil, err
	}
	idx, err := in.eval1(env, n.Index)
	if err != nil {
		return nil, err
	}
	rv := reflect.ValueOf(container)
	switch rv.Kind() {
	case reflect.Map:
		kv, err := convertTo(idx, rv.Type().Key())
		if err != nil {
			return nil, in.wrapAt(n, err)
		}
		out := rv.MapIndex(kv)
		if !out.IsValid() {
			// A missing entry yields the element type's zero, and for a
			// script struct that is Go's zero STRUCT -- not nil. The slot
			// has no value to read a StructType off, so this works only
			// because the element TYPE names one: see zeroInSlot.
			return zeroInSlot(rv.Type().Elem()), nil
		}
		return fromStore(out), nil
	case reflect.Slice, reflect.Array, reflect.String:
		i, ok := toI64(idx)
		if !ok {
			return nil, in.errAt(n, "index must be an integer")
		}
		if i < 0 || int(i) >= rv.Len() {
			return nil, in.errAt(n, fmt.Sprintf("index out of range [%d] with length %d", i, rv.Len()))
		}
		return fromStore(rv.Index(int(i))), nil
	}
	return nil, in.errAt(n, fmt.Sprintf("cannot index %s", valTypeName(container)))
}

func (in *Interp) evalSlice(env *Env, n *ast.SliceExpr) (Value, error) {
	container, err := in.eval1(env, n.X)
	if err != nil {
		return nil, err
	}
	rv := reflect.ValueOf(container)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.String && rv.Kind() != reflect.Array {
		return nil, in.errAt(n, fmt.Sprintf("cannot slice %s", valTypeName(container)))
	}
	lo, hi := 0, rv.Len()
	if n.Low != nil {
		v, err := in.eval1(env, n.Low)
		if err != nil {
			return nil, err
		}
		i, ok := toI64(v)
		if !ok {
			return nil, in.errAt(n.Low, "slice bound must be an integer")
		}
		lo = int(i)
	}
	if n.High != nil {
		v, err := in.eval1(env, n.High)
		if err != nil {
			return nil, err
		}
		i, ok := toI64(v)
		if !ok {
			return nil, in.errAt(n.High, "slice bound must be an integer")
		}
		hi = int(i)
	}
	if lo < 0 || hi > rv.Len() || lo > hi {
		return nil, in.errAt(n, fmt.Sprintf("slice bounds out of range [%d:%d] with length %d", lo, hi, rv.Len()))
	}
	return rv.Slice(lo, hi).Interface(), nil
}

func (in *Interp) evalSelector(env *Env, n *ast.SelectorExpr) (Value, error) {
	// Package symbol: fmt.Println, time.Second, ...
	if pkg, ok := n.X.(*ast.Ident); ok {
		if _, bound := env.Get(pkg.Name); !bound {
			if v, ok := in.lookupPkg(pkg.Name, n.Sel.Name); ok {
				return v, nil
			}
		}
	}
	// Struct field access on a value.
	v, err := in.eval1(env, n.X)
	if err != nil {
		return nil, err
	}
	if sv, ok := v.(*StructVal); ok {
		return in.structField(n, sv, n.Sel.Name)
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Struct {
		f := rv.FieldByName(n.Sel.Name)
		if f.IsValid() && f.CanInterface() {
			return f.Interface(), nil
		}
	}
	return nil, in.errAt(n, fmt.Sprintf("unknown selector %s on %s", n.Sel.Name, valTypeName(v)))
}

// ---- range ----

func (in *Interp) rangeOver(n *ast.RangeStmt, x Value, iterate func(k, v Value) (bool, error)) error {
	if x == nil {
		return nil
	}
	rv := reflect.ValueOf(x)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			cont, err := iterate(i, fromStore(rv.Index(i)))
			if err != nil || !cont {
				return err
			}
		}
	case reflect.String:
		for i, r := range x.(string) {
			cont, err := iterate(i, r)
			if err != nil || !cont {
				return err
			}
		}
	case reflect.Map:
		// Sort keys when possible so scripts are deterministic -- and,
		// for a struct-keyed map, in the same order fmt prints them.
		keys := rv.MapKeys()
		// sortMapKeys hands back the script's own keys for a struct-keyed
		// map, because ordering had to decode every one of them anyway.
		// Decoding again in this loop would do that work twice per key.
		decoded := sortMapKeys(keys)
		for i, k := range keys {
			// The key erasure is undone by that decode: a struct-keyed
			// map stores a minted key wrapping a StructKey, and the
			// script must range over its own P. Each key's struct was
			// rebuilt fresh and is used by exactly one iteration, so
			// assigning to the range variable's fields can reach neither
			// the key inside the map nor another iteration.
			var key Value
			if decoded != nil {
				key = decoded[i]
			} else {
				key = k.Interface()
			}
			cont, err := iterate(key, fromStore(rv.MapIndex(k)))
			if err != nil || !cont {
				return err
			}
		}
	case reflect.Int, reflect.Int64:
		max := rv.Int()
		for i := int64(0); i < max; i++ {
			cont, err := iterate(int(i), nil)
			if err != nil || !cont {
				return err
			}
		}
	default:
		return in.errAt(n, fmt.Sprintf("cannot range over %s", valTypeName(x)))
	}
	return nil
}

// sortMapKeys orders a map's keys so a range over it is deterministic,
// and returns the SCRIPT-facing key for each slot, in the sorted order,
// or nil when the keys need no decoding.
//
// The two jobs are one function because for a struct-keyed map they are
// one piece of work: ordering needs every key decoded, and the decoded
// struct is exactly what the range variable must be. Kept apart, every
// key of every range was decoded twice.
//
// THE ORDER IS fmt's, for every key type a script can spell. A
// struct-keyed map is ordered field by field by keyCmp -- the same
// comparator appendStructKeyedMap uses to reproduce internal/fmtsort --
// and everything else goes to orderScalarKeys, so `for k := range m` and
// `fmt.Println(m)` visit the entries in the same order.
//
// Neither was true before. A struct-keyed map was ordered by each key's
// RENDERED TEXT, which puts P{N: 10} before P{N: 2} because '0' < '}',
// while fmt compares the fields themselves and puts 2 first: two
// deterministic orders, neither wrong, that disagreed. And a map keyed by
// anything other than a string or a struct -- an int-keyed map, the
// commonest map there is after a string-keyed one -- was not ordered at
// all, so it ranged differently on every run while fmt printed it in
// numeric order. Both are closed here.
//
// DROPPING THE RENDER IS WHY IT ALSO GOT FASTER, and the trade is a
// SHAPE trade rather than a free win. The text order had to render every
// key before the first comparison -- n renders of text nothing would ever
// read -- where keyCmp reads the decoded fields the range variable is
// made of anyway. But a render is paid n times and a comparison n log n
// times, so what was removed grows more slowly than what replaced it.
// BenchmarkSortMapKeys, ns per key, Apple M3, minimum of six runs:
//
//	keys              4      16      64     256    1024
//	 struct, 1 field
//	  text        104.8    84.2    70.0    71.9    80.2
//	  fields       45.8    48.3    54.9    74.6    95.0
//	  allocs      8->6    9->6    9->6   10->6   15->12
//	 struct, 10 fields
//	  text        181.7   166.0   180.3   194.3   208.5
//	  fields       50.6    49.5    66.7    85.6   122.2
//	  allocs     12->6   12->6   12->6  18->12  36->30
//
// The allocations that go are the render's -- the slab and the doubling
// steps it took, the bounds, and the slice of views cut into it. What is
// left is the arena's, and it still steps rather than tracking n.
//
// A ONE-FIELD KEY PAST ~256 IS THE ONE PLACE THIS LOSES: 18% at a
// thousand. One int field makes a comparison almost as cheap as
// bytes.Compare on the rendered text, so removing n renders no longer
// pays for the n log n comparisons that remain. Ten fields move the
// crossover past every count measured, because the comparison still
// stops at the first field while the render has ten of them to write --
// the wider the key, the further out it goes. A shell ranging a
// thousand-key map is not the case worth tuning for, and the four-key map
// it does range is 2.3x faster, so the trade is taken as it stands.
//
// Read the table knowing the benchmark's keys differ in their FIRST
// field, which is the comparator's best case and the render's worst. Keys
// that tie for a few fields before they differ would move the crossover
// in.
//
// A DECLINED MAP FALLS BACK TO THE TEXT ORDER. keyCmp refuses to invent
// an answer wherever fmtsort's own comes from a machine address -- two
// keys carrying different *StructTypes, one field holding two different
// dynamic types, a field type it has no case for -- and in exactly those
// cases fmt has no order to match: its answer changes between runs. So
// the fallback's job is not parity but DETERMINISM, and the rendered text
// is the order that already had that property.
//
// THE DECLINE IS FOUND BY THE SORT, not by a pass over the keys first,
// and that is deliberate rather than lazy. A pre-pass would have to
// declare a whole field position declined the moment two keys disagree
// about its type, including when no comparison ever reaches that field --
// P{A: 1, B: 1} and P{A: 2, B: "x"} are settled by A, by fmt as well as
// here, so declining them would give up a map the two agree on. Finding
// it in the comparator gives up only the pairs actually asked about.
//
// What makes that safe is that a pair the sort SKIPS cannot be a pair the
// answer depends on. A correct comparison sort has to compare every pair
// that ends up adjacent in its output, so a skipped pair x,y has some z
// between them with x < z < y; those two comparisons did not decline, so
// fmt orders them the same way, and fmt therefore puts x before y for the
// same reason this does. The pairs whose order rests on an address are
// exactly the adjacent ones, and those are exactly the ones compared.
func sortMapKeys(keys []reflect.Value) []*StructVal {
	n := len(keys)
	if n == 0 {
		return nil
	}
	// The type is asked, not the value: every key in a map shares one
	// type, so one lookup settles the whole slice. It is hoisted above
	// everything else because the struct case needs the type itself and
	// not merely the fact that there is one; a non-struct key kind leaves
	// keyOwnerOf on its first line, so the hoist costs the others nothing.
	kt := keyOwnerOf(keys[0].Type())
	// A MAP OF ONE KEY IS ALREADY ORDERED. Everything below exists to
	// establish an order, so a single key skips straight to the decode.
	//
	// For a STRUCT key that shortcut is worth 16-18ns a key, which is the
	// decode it still has to do minus the ordering pass it does not. For
	// a scalar key it is worth almost nothing -- a one-element
	// slices.SortFunc is a length check -- and no test pins it, because
	// removing that half changes no answer. It is written as one branch
	// because the two questions are one question: is there an order to
	// establish.
	if n == 1 {
		if kt != nil {
			return []*StructVal{decodeMintedKey(keys[0], nil)}
		}
		return nil
	}
	if kt == nil {
		// Every non-struct key: nothing to decode, so the sort is the
		// whole job and the caller reads the map's own keys.
		orderScalarKeys(keys)
		// nil: the script's keys are the map's own, so the caller has
		// nothing to read here.
		return nil
	}

	// One arena for the whole map. Every key is decoded HERE, before the
	// caller's loop body runs even once, so all of them are alive
	// together whether they came from an arena or not -- the slab changes
	// where they live, not for how long.
	//
	// Maps of two keys ask for none: two slabs cost more than the two
	// fused blocks they would replace, and there is too little to
	// amortise them over. Three is where it turns, and BenchmarkMapKeyArena
	// is where that was measured.
	var arena *keyArena
	if n > 2 {
		arena = newKeyArena(kt, n)
	}
	// Held as the *StructVal it is, so a nil key stays the typed nil the
	// script sees everywhere else. keyCmp and appendTo both answer for it.
	decoded := make([]*StructVal, n)
	for i, k := range keys {
		decoded[i] = decodeMintedKey(k, arena)
	}

	pair := &keyOrder{keys: keys, decoded: decoded}
	// The comparator records a decline rather than returning one, which
	// is what lets it mirror fmtsort's own recursive shape instead of
	// threading an error through it. A declined sort leaves a meaningless
	// order behind, which the fallback below overwrites entirely.
	var c keyCmp
	sort.Sort(&fieldOrder{keyOrder: pair, cmp: &c})
	if !c.declined {
		return decoded
	}

	// THE FALLBACK. Render every key once and order by that text.
	//
	// ord holds one VIEW per key of that text; buf holds all of it end to
	// end. Views cannot be taken while buf is still growing -- an append
	// that reallocates would leave them pointing at the old array -- so
	// the render records bounds and the views are cut once, after.
	ord := make([][]byte, n)
	bounds := make([]int, n+1)
	var buf []byte
	for i, sv := range decoded {
		buf = sv.appendTo(buf)
		bounds[i+1] = len(buf)
		if i == 0 {
			buf = growForRest(buf, n)
		}
	}
	for i := range ord {
		ord[i] = buf[bounds[i]:bounds[i+1]]
	}
	sort.Sort(&textOrder{keys: keys, decoded: decoded, ord: ord})
	return decoded
}

// orderScalarKeys puts a map's keys in fmt's order when the key type is
// one fmt orders reproducibly, and leaves them alone when it is not.
//
// This is the whole job for a scalar-keyed map. There is nothing to
// decode -- a script's int key IS the map's int key -- so the keys are
// sorted in place, the caller reads them directly, and no memory is
// touched at all. BenchmarkSortMapKeys, ns per key, Apple M3, minimum of
// six runs, zero allocations at every count:
//
//	keys        4    16    64   256  1024
//	 int      8.2  13.0  20.9  26.6  39.2
//	 string   8.2  13.9  27.1  35.0  50.2
//
// which is the price of a determinism an int-keyed map did not have
// before, and it is the sort and nothing else. The string row is the
// comparison worth making: same shape, same sort, one Int() load against
// one String() header load.
//
// IT SWITCHES ON KIND, WHICH IS THE OPPOSITE OF WHAT mapValRender DOES,
// and the two are both right. Rendering a named integer type has to go by
// TYPE identity, because %v calls its String method and prints a
// time.Duration as "3s". ORDERING never calls String: fmtsort switches on
// reflect.Kind and compares the numbers, so two time.Durations sort 1
// before 2 however they print. Kind is exactly the question here.
//
// The kinds are grouped as families rather than as the types a script can
// actually spell -- typeIdents offers int, int64, rune, byte, float64,
// bool, string and any -- because a map does not have to come from a
// script's own type expression. A stdlib call can hand back a map[uint32]T
// to range, and fmtsort orders one; there is no reason to be narrower
// than the thing being reproduced.
//
// WHAT IS LEFT UNORDERED is what fmt orders by a MACHINE ADDRESS or by
// nothing this package can render: pointer, channel and unsafe.Pointer
// keys, whose text IS an address and so cannot be made deterministic
// either, and complex, array and native-struct keys, which fmtsort
// compares element-wise and no script can spell. Those range in the map's
// own randomised order, exactly as every scalar key did before this.
func orderScalarKeys(keys []reflect.Value) {
	switch keys[0].Kind() {
	case reflect.String:
		// A STRING KEY IS ALREADY THE TEXT IT SORTS BY, so this touches
		// no memory at all -- reflect.Value.String on a string key is a
		// header load -- and it was already fmt's order: fmtsort compares
		// string keys with strings.Compare, which is this.
		//
		// Two other shapes were written first. All three in one binary,
		// minimum of eight runs at a fixed iteration count, Apple M3, ns
		// per key:
		//
		//	keys                            4    16    64    256    1024
		//	 insertion sort on a []string 10.3  22.3  58.2  179.5   779.9   1 alloc
		//	 sort.Sort on a []string      16.3  16.1  27.1   34.4    46.7   2 allocs
		//	 this                          6.4  12.9  25.9   34.8    51.5   0 allocs
		//
		// The []string wins at a thousand keys because it calls String
		// once per key where this calls it n log n times -- but it pays
		// an allocation for the slice and another for the sorter, which
		// escapes into sort.Sort's interface, and those two ARE what a
		// four-key map costs. Small string-keyed maps are the common case
		// in a shell. One allocation-free path that leads everywhere up
		// to 256 keys and trails by 10% at 1024 beat two paths and a
		// threshold between them.
		slices.SortFunc(keys, func(a, b reflect.Value) int {
			return strings.Compare(a.String(), b.String())
		})
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		slices.SortFunc(keys, func(a, b reflect.Value) int {
			return cmp.Compare(a.Int(), b.Int())
		})
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		slices.SortFunc(keys, func(a, b reflect.Value) int {
			return cmp.Compare(a.Uint(), b.Uint())
		})
	case reflect.Float32, reflect.Float64:
		// cmp.Compare puts NaN below every other float and calls two NaNs
		// equal, which is fmtsort's floatCompare exactly. Two NaN keys are
		// still distinct map entries whose relative order neither fmt nor
		// this fixes -- the only tie two distinct scalar keys can produce.
		slices.SortFunc(keys, func(a, b reflect.Value) int {
			return cmp.Compare(a.Float(), b.Float())
		})
	case reflect.Bool:
		// false before true, which is fmtsort's rule and Go's own
		// convention everywhere else.
		slices.SortFunc(keys, func(a, b reflect.Value) int {
			switch {
			case a.Bool() == b.Bool():
				return 0
			case a.Bool():
				return 1
			default:
				return -1
			}
		})
	case reflect.Interface:
		orderInterfaceKeys(keys)
	}
}

// orderInterfaceKeys orders the keys of a map[any]V or a map[error]V.
//
// An interface key is the SAME SHAPE OF PROBLEM as a struct key's field:
// fmtsort puts a nil interface first, then orders by the address of the
// dynamic TYPE, then by the value. So this is keyCmp.field, unchanged and
// already tested -- a map[any]int whose keys are all ints is ordered
// numerically, which is reproducibly what fmt does, and one holding an
// int and a string declines, because fmt's answer there is which of int
// and string was linked at the lower address.
//
// A DECLINE STILL HAS TO BE DETERMINISTIC, which is why the fallback
// renders. It is the same fallback a declined struct-keyed map takes, and
// it is reached for the same reason: fmt has an answer, it is an address,
// and the only thing left worth having is that the script sees the same
// order every run.
//
// The render is worth nothing to the accepted path, so it is not built
// until the decline is known -- a map[any]int of plain ints pays one
// wasted sort and no memory.
func orderInterfaceKeys(keys []reflect.Value) {
	var c keyCmp
	slices.SortFunc(keys, func(a, b reflect.Value) int {
		return c.field(a.Interface(), b.Interface())
	})
	if !c.declined {
		return
	}
	n := len(keys)
	ord := make([][]byte, n)
	bounds := make([]int, n+1)
	var buf []byte
	for i, k := range keys {
		buf = appendValue(buf, k.Interface())
		bounds[i+1] = len(buf)
		if i == 0 {
			buf = growForRest(buf, n)
		}
	}
	for i := range ord {
		ord[i] = buf[bounds[i]:bounds[i+1]]
	}
	sort.Sort(&textOrder{keys: keys, ord: ord})
}

// growForRest sizes the render slab from its first entry, so a map of a
// thousand keys grows its buffer once instead of ten times.
//
// The first key is a good estimator for the rest and a harmless one when
// it is not: its one caller renders n instances of ONE struct type, so
// the only spread is in how many digits a field takes, and a guess that
// comes up short simply falls back on append's own doubling. The eighth
// is headroom for that spread; over-guessing costs a slab that is dropped
// when the sort returns.
func growForRest(buf []byte, n int) []byte {
	return slices.Grow(buf, (n-1)*len(buf)+len(buf)/8+16)
}

// keyOrder is the pair of slices any ordering of a struct-keyed map has
// to keep in step: the map's own keys, which the caller reads values
// with, and the decoded structs it hands the script as the range
// variable. A Swap that moved one and not the other would still produce
// sorted output -- and would pair every key with another key's value.
//
// It carries no Less. fieldOrder supplies that, and is the only thing
// that embeds it; the text order below keeps its own slices, because it
// serves a caller that has no decoded keys at all.
//
// It is deliberately not inspect.go's keySorter. That one sorts two
// slices by a []string it keeps and PRINTS afterwards; this one sorts by
// something that is scratch either way and is dropped on return. Merging
// them would force one caller to carry the other's cost.
//
// The receivers are POINTERS so Less and Swap do not copy slice headers
// on every call of a sort's inner loop.
type keyOrder struct {
	keys    []reflect.Value
	decoded []*StructVal
}

func (s *keyOrder) Len() int { return len(s.keys) }

func (s *keyOrder) Swap(i, j int) {
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
	s.decoded[i], s.decoded[j] = s.decoded[j], s.decoded[i]
}

// fieldOrder is fmt's order: keyCmp over the decoded structs, which is
// internal/fmtsort's comparison rewritten over keys that have already
// been decoded. cmp is shared with the caller, which reads its decline
// flag once the sort is done.
type fieldOrder struct {
	*keyOrder
	cmp *keyCmp
}

func (s *fieldOrder) Less(i, j int) bool { return s.cmp.keys(s.decoded[i], s.decoded[j]) < 0 }

// textOrder is the fallback both declining paths take: the keys ordered
// as the text they render to, which is what a struct-keyed map was
// ordered by before keyCmp existed and is still the only deterministic
// answer for a map fmt itself orders by an address.
//
// It does not embed keyOrder, because its two callers do not agree on
// what there is to keep in step: a declined struct-keyed map has decoded
// structs to move with the keys, and a declined map[any]V has nothing
// but the keys. decoded is nil for the second, and Swap says so rather
// than making the struct path's own sort carry the check.
//
// Ties -- two distinct keys with identical text, an int 1 and a string
// "1" -- keep whatever order the declined sort before this left them in,
// which is the map's own randomised one. That was true of the text order
// before this too: the sort was never stable.
type textOrder struct {
	keys    []reflect.Value
	decoded []*StructVal
	ord     [][]byte
}

func (s *textOrder) Len() int { return len(s.keys) }

func (s *textOrder) Less(i, j int) bool { return bytes.Compare(s.ord[i], s.ord[j]) < 0 }

func (s *textOrder) Swap(i, j int) {
	s.ord[i], s.ord[j] = s.ord[j], s.ord[i]
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
	if s.decoded != nil {
		s.decoded[i], s.decoded[j] = s.decoded[j], s.decoded[i]
	}
}

// ---- declarations & switch ----

func (in *Interp) evalDecl(env *Env, ds *ast.DeclStmt) error {
	gd, ok := ds.Decl.(*ast.GenDecl)
	if !ok {
		return in.errAt(ds, "unsupported declaration")
	}
	switch gd.Tok {
	case token.VAR, token.CONST:
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				return in.errAt(spec, "unsupported declaration")
			}
			for i, name := range vs.Names {
				var v Value
				switch {
				case i < len(vs.Values):
					var err error
					v, err = in.eval1(env, vs.Values[i])
					if err != nil {
						return err
					}
				case vs.Type != nil:
					// TypeDesc.Zero covers `var p P` and `var xs []P`
					// alike: a script struct zeroes to a fresh instance,
					// a container holding one to reflect's own zero.
					d, err := in.typeOf(env, vs.Type)
					if err != nil {
						return err
					}
					v = d.Zero()
				default:
					return in.errAt(name, "declaration needs a type or a value")
				}
				if name.Name != "_" {
					// `var b = a` stores, exactly as `b := a` does. The
					// zero-value branches above build a fresh value, so
					// the copy only ever bites on the value branch.
					env.Define(name.Name, copyOnStore(v))
				}
			}
		}
		return nil
	case token.TYPE:
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				return in.errAt(spec, "unsupported type declaration")
			}
			if err := in.declareType(env, ts); err != nil {
				return err
			}
		}
		return nil
	}
	return in.errAt(ds, "unsupported declaration "+gd.Tok.String())
}

func (in *Interp) evalSwitch(env *Env, n *ast.SwitchStmt) (control, error) {
	// As with if: this scope carries `switch x := f(); x` and nothing
	// else, since a case expression cannot declare. The per-case body
	// scope in runCaseBody is a separate matter and stays -- a CaseClause
	// body is a bare statement list, so nothing else wraps it.
	scope := env
	if n.Init != nil {
		scope = NewEnv(env)
		if _, err := in.evalStmt(scope, n.Init); err != nil {
			return control{}, err
		}
	}
	var tag Value = true
	hasTag := n.Tag != nil
	if hasTag {
		var err error
		tag, err = in.eval1(scope, n.Tag)
		if err != nil {
			return control{}, err
		}
	}

	var deflt *ast.CaseClause
	for _, stmt := range n.Body.List {
		cc := stmt.(*ast.CaseClause)
		if cc.List == nil {
			deflt = cc
			continue
		}
		for _, ce := range cc.List {
			v, err := in.eval1(scope, ce)
			if err != nil {
				return control{}, err
			}
			match := false
			if hasTag {
				eq, err := in.binaryOp(ce, token.EQL, tag, v)
				if err != nil {
					return control{}, err
				}
				match = eq == true
			} else {
				b, ok := v.(bool)
				if !ok {
					return control{}, in.errAt(ce, "switch case must be bool")
				}
				match = b
			}
			if match {
				return in.runCaseBody(scope, cc)
			}
		}
	}
	if deflt != nil {
		return in.runCaseBody(scope, deflt)
	}
	return control{}, nil
}

func (in *Interp) runCaseBody(env *Env, cc *ast.CaseClause) (control, error) {
	scope := NewEnv(env)
	for _, s := range cc.Body {
		ctl, err := in.evalStmt(scope, s)
		if err != nil {
			return control{}, err
		}
		switch ctl.kind {
		case ctlBreak:
			return control{}, nil // break leaves the switch
		case ctlReturn, ctlContinue:
			return ctl, nil
		}
	}
	return control{}, nil
}

// stringOf renders a value for interpolation into shell words.
func stringOf(v Value) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
