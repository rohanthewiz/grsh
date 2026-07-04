package interp

import (
	"fmt"
	"go/ast"
	"go/token"
	"reflect"
	"strconv"
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
			return nil, in.errAt(n, "type switches are not supported in grsh v1")
		}
		v, err := in.eval1(env, n.X)
		if err != nil {
			return nil, err
		}
		t, err := in.typeOf(n.Type)
		if err != nil {
			return nil, err
		}
		if v != nil && reflect.TypeOf(v).AssignableTo(t) {
			return []Value{v, true}, nil
		}
		return []Value{reflect.Zero(t).Interface(), false}, nil

	default:
		return nil, in.errAt(e, fmt.Sprintf("%T expression is not supported in grsh v1", e))
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
		return false, in.errAt(e, fmt.Sprintf("condition must be bool, got %T", v))
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
			return nil, in.errAt(n, fmt.Sprintf("operator ! requires bool, got %T", v))
		}
		return !b, nil
	case token.SUB:
		if i, ok := toI64(v); ok {
			return int(-i), nil
		}
		if f, ok := toF64(v); ok {
			return -f, nil
		}
		return nil, in.errAt(n, fmt.Sprintf("operator - requires a number, got %T", v))
	case token.ADD:
		return v, nil
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

	// Fallback equality for everything else (nil, errors, slices...).
	switch op {
	case token.EQL:
		return safeEqual(x, y), nil
	case token.NEQ:
		return !safeEqual(x, y), nil
	}
	return nil, in.errAt(n, fmt.Sprintf("operator %s is not defined on %T and %T", op, x, y))
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
		return int(x << uint(y)), nil
	case token.SHR:
		return int(x >> uint(y)), nil
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

func safeEqual(x, y Value) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	defer func() { recover() }() //nolint: uncomparable types compare unequal
	return x == y
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
			env.Define(id.Name, vals[i])
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
				return []Value{out.Interface(), true}, nil
			}
			return []Value{reflect.Zero(rv.Type().Elem()).Interface(), false}, nil
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
	}
	return tok, false
}

func (in *Interp) setLValue(env *Env, lhs ast.Expr, v Value) error {
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
		return in.errAt(lhs, fmt.Sprintf("cannot assign to field of %T", recv))
	default:
		return in.errAt(lhs, fmt.Sprintf("cannot assign to %T in grsh v1", lhs))
	}
}

func (in *Interp) setIndexed(n ast.Node, container, idx, v Value) error {
	rv := reflect.ValueOf(container)
	switch rv.Kind() {
	case reflect.Map:
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
	return in.errAt(n, fmt.Sprintf("cannot index-assign into %T", container))
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
			return reflect.Zero(rv.Type().Elem()).Interface(), nil
		}
		return out.Interface(), nil
	case reflect.Slice, reflect.Array, reflect.String:
		i, ok := toI64(idx)
		if !ok {
			return nil, in.errAt(n, "index must be an integer")
		}
		if i < 0 || int(i) >= rv.Len() {
			return nil, in.errAt(n, fmt.Sprintf("index out of range [%d] with length %d", i, rv.Len()))
		}
		return rv.Index(int(i)).Interface(), nil
	}
	return nil, in.errAt(n, fmt.Sprintf("cannot index %T", container))
}

func (in *Interp) evalSlice(env *Env, n *ast.SliceExpr) (Value, error) {
	container, err := in.eval1(env, n.X)
	if err != nil {
		return nil, err
	}
	rv := reflect.ValueOf(container)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.String && rv.Kind() != reflect.Array {
		return nil, in.errAt(n, fmt.Sprintf("cannot slice %T", container))
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
	return nil, in.errAt(n, fmt.Sprintf("unknown selector %s on %T", n.Sel.Name, v))
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
			cont, err := iterate(i, rv.Index(i).Interface())
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
		// Sort keys when possible so scripts are deterministic.
		keys := rv.MapKeys()
		sortMapKeys(keys)
		for _, k := range keys {
			cont, err := iterate(k.Interface(), rv.MapIndex(k).Interface())
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
		return in.errAt(n, fmt.Sprintf("cannot range over %T", x))
	}
	return nil
}

func sortMapKeys(keys []reflect.Value) {
	if len(keys) == 0 || keys[0].Kind() != reflect.String {
		return
	}
	strs := make([]string, len(keys))
	for i, k := range keys {
		strs[i] = k.String()
	}
	// insertion sort keeps this dependency-free
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && strs[j] < strs[j-1]; j-- {
			strs[j], strs[j-1] = strs[j-1], strs[j]
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
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
					if st, ok := lookupStructType(env, vs.Type); ok {
						v = st.newZero()
						break
					}
					t, err := in.typeOf(vs.Type)
					if err != nil {
						return err
					}
					v = reflect.Zero(t).Interface()
				default:
					return in.errAt(name, "declaration needs a type or a value")
				}
				if name.Name != "_" {
					env.Define(name.Name, v)
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
	scope := NewEnv(env)
	if n.Init != nil {
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
