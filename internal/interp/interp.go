// Package interp is grsh's tree-walking evaluator for the Go subset.
// It executes the __main body produced by the transform stage; shell
// fragments surface as __shell(n)/__capture(n) calls into shellexec.
package interp

import (
	"fmt"
	"go/ast"
	"go/token"

	"github.com/rohanthewiz/grsh/internal/shellexec"
	"github.com/rohanthewiz/grsh/internal/shellparse"
	"github.com/rohanthewiz/serr"
)

type Interp struct {
	fset    *token.FileSet
	sh      *shellexec.State
	stdio   shellexec.Stdio
	tab     []*shellparse.CmdList
	globals *Env
	frames  []*frame

	// callChain is the live stack of script-level function names, one
	// entry per active closure call -- so its length IS the call depth,
	// which is what the runaway-recursion limit tests. errChain holds
	// that stack rendered for an error already in flight; see
	// callClosure for why it is captured and attached at different
	// points.
	callChain []string
	errChain  string

	// exprCache holds parsed {expr} interpolation fragments keyed by
	// (src, line). Entries carry positions in fset, so the cache is valid
	// only for the current Run and is reset whenever fset is replaced.
	// It is bounded at exprCacheMax entries (see call.go).
	exprCache map[string]ast.Expr
}

// frame holds per-function-call state (deferred calls).
type frame struct {
	defers []deferredCall
}

type deferredCall struct {
	node ast.Node
	fn   Value
	args []Value
}

func (in *Interp) pushFrame() *frame {
	f := &frame{}
	in.frames = append(in.frames, f)
	return f
}

// popFrame runs the frame's deferred calls in LIFO order. The first defer
// error surfaces unless the function body already failed.
func (in *Interp) popFrame(bodyErr error) error {
	f := in.frames[len(in.frames)-1]
	in.frames = in.frames[:len(in.frames)-1]
	err := bodyErr
	// A deferred call that fails while the body has ALSO failed has its
	// own error dropped below -- so it must not leave behind a call chain
	// captured for an error nobody will ever see. Restoring what was in
	// flight keeps the surviving error's chain its own.
	if bodyErr != nil {
		defer func(saved string) { in.errChain = saved }(in.errChain)
	}
	for i := len(f.defers) - 1; i >= 0; i-- {
		d := f.defers[i]
		var callErr error
		if cl, ok := d.fn.(*Closure); ok {
			_, callErr = in.callClosure(d.node, cl, d.args)
		} else {
			_, callErr = in.callReflect(d.node, d.fn, d.args, "deferred call")
		}
		if err == nil {
			err = callErr
		}
	}
	return err
}

// Closure is a script-defined function value.
type Closure struct {
	Name string
	Fn   *ast.FuncLit
	Env  *Env
}

func (c *Closure) String() string {
	if c.Name != "" {
		return "func " + c.Name
	}
	return "func literal"
}

type ctlKind int

const (
	ctlNone ctlKind = iota
	ctlBreak
	ctlContinue
	ctlReturn
)

type control struct {
	kind ctlKind
	vals []Value
}

func New(sh *shellexec.State, stdio shellexec.Stdio, builtinFns map[string]any) *Interp {
	g := NewEnv(nil)
	for k, v := range builtinFns {
		g.Define(k, v)
	}
	return &Interp{sh: sh, stdio: stdio, globals: g}
}

// AddTab appends shell fragments to the side table, returning the base
// index they were registered at (transform emits absolute indices).
func (in *Interp) AddTab(frags []*shellparse.CmdList) int {
	base := len(in.tab)
	in.tab = append(in.tab, frags...)
	return base
}

// Run executes the __main body of a transformed file against the global
// scope. Top-level `name := func(...)` statements are hoisted first so
// forward references and mutual recursion work.
func (in *Interp) Run(fset *token.FileSet, f *ast.File) error {
	in.fset = fset
	in.exprCache = nil // fragment positions belong to the previous fset
	in.errChain = ""   // nothing from a previous unit is in flight
	var body *ast.BlockStmt
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "__main" {
			body = fd.Body
		}
	}
	if body == nil {
		return serr.New("internal: transformed file has no __main")
	}
	for _, st := range body.List {
		if name, fl, ok := topFuncAssign(st); ok {
			in.globals.Define(name, &Closure{Name: name, Fn: fl, Env: in.globals})
		}
	}
	in.pushFrame()
	var runErr error
	for _, st := range body.List {
		if _, _, ok := topFuncAssign(st); ok {
			continue
		}
		ctl, err := in.evalStmt(in.globals, st)
		if err != nil {
			runErr = err
			break
		}
		if ctl.kind == ctlReturn {
			break // top-level return ends the script
		}
	}
	return in.popFrame(runErr)
}

// topFuncAssign matches `name := func(...) {...}` at the top level.
func topFuncAssign(st ast.Stmt) (string, *ast.FuncLit, bool) {
	as, ok := st.(*ast.AssignStmt)
	if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
		return "", nil, false
	}
	id, ok := as.Lhs[0].(*ast.Ident)
	if !ok {
		return "", nil, false
	}
	fl, ok := as.Rhs[0].(*ast.FuncLit)
	if !ok {
		return "", nil, false
	}
	return id.Name, fl, true
}

func (in *Interp) pos(n ast.Node) string {
	p := in.fset.Position(n.Pos())
	return fmt.Sprintf("%s:%d:%d", p.Filename, p.Line, p.Column)
}

func (in *Interp) errAt(n ast.Node, msg string, kv ...string) error {
	return serr.New(msg, append([]string{"loc", in.pos(n)}, kv...)...)
}

func (in *Interp) wrapAt(n ast.Node, err error, kv ...string) error {
	return serr.Wrap(err, append([]string{"loc", in.pos(n)}, kv...)...)
}

func (in *Interp) evalStmt(env *Env, st ast.Stmt) (control, error) {
	switch n := st.(type) {
	case *ast.EmptyStmt:
		return control{}, nil

	case *ast.ExprStmt:
		if call, ok := n.X.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "__shell" {
				return in.runShellStmt(env, call)
			}
		}
		_, err := in.evalExpr(env, n.X)
		return control{}, err

	case *ast.AssignStmt:
		return control{}, in.assign(env, n)

	case *ast.IncDecStmt:
		cur, err := in.eval1(env, n.X)
		if err != nil {
			return control{}, err
		}
		op := token.ADD
		if n.Tok == token.DEC {
			op = token.SUB
		}
		nv, err := in.binaryOp(n, op, cur, 1)
		if err != nil {
			return control{}, err
		}
		return control{}, in.setLValue(env, n.X, nv)

	case *ast.IfStmt:
		// The extra scope exists solely to hold `if v := f(); ...`, so it
		// is built only when there is an init to hold. A condition cannot
		// declare anything, so with no init the enclosing scope serves.
		scope := env
		if n.Init != nil {
			scope = NewEnv(env)
			if _, err := in.evalStmt(scope, n.Init); err != nil {
				return control{}, err
			}
		}
		cond, err := in.evalBool(scope, n.Cond)
		if err != nil {
			return control{}, err
		}
		// Body is always an *ast.BlockStmt and Else is always a block or
		// another if -- go/parser admits nothing else -- and each opens a
		// scope of its own on the way in. Wrapping here too added a link
		// that every identifier lookup below had to walk past and that
		// nothing was ever defined into.
		if cond {
			return in.evalStmt(scope, n.Body)
		}
		if n.Else != nil {
			return in.evalStmt(scope, n.Else)
		}
		return control{}, nil

	case *ast.BlockStmt:
		scope := NewEnv(env)
		for _, s := range n.List {
			ctl, err := in.evalStmt(scope, s)
			if err != nil || ctl.kind != ctlNone {
				return ctl, err
			}
		}
		return control{}, nil

	case *ast.ForStmt:
		return in.evalFor(env, n)

	case *ast.RangeStmt:
		return in.evalRange(env, n)

	case *ast.BranchStmt:
		switch n.Tok {
		case token.BREAK:
			return control{kind: ctlBreak}, nil
		case token.CONTINUE:
			return control{kind: ctlContinue}, nil
		}
		return control{}, in.errAt(n, n.Tok.String()+" is not supported yet")

	case *ast.ReturnStmt:
		var vals []Value
		if len(n.Results) == 1 {
			vs, err := in.evalExpr(env, n.Results[0])
			if err != nil {
				return control{}, err
			}
			vals = vs
		} else {
			for _, r := range n.Results {
				v, err := in.eval1(env, r)
				if err != nil {
					return control{}, err
				}
				vals = append(vals, v)
			}
		}
		return control{kind: ctlReturn, vals: vals}, nil

	case *ast.DeclStmt:
		return control{}, in.evalDecl(env, n)

	case *ast.SwitchStmt:
		return in.evalSwitch(env, n)

	case *ast.DeferStmt:
		// Go semantics: callee and arguments evaluate now, call runs at
		// frame exit.
		fnV, err := in.eval1(env, n.Call.Fun)
		if err != nil {
			return control{}, err
		}
		args, err := in.evalArgs(env, n.Call)
		if err != nil {
			return control{}, err
		}
		if len(in.frames) == 0 {
			return control{}, in.errAt(n, "internal: defer outside a frame")
		}
		f := in.frames[len(in.frames)-1]
		f.defers = append(f.defers, deferredCall{node: n.Call, fn: fnV, args: args})
		return control{}, nil

	default:
		return control{}, in.errAt(st, fmt.Sprintf("%T is not supported yet", st))
	}
}

func (in *Interp) evalFor(env *Env, n *ast.ForStmt) (control, error) {
	// The clause scope holds the loop variable from `for i := 0; ...`. It
	// is built once and lives for the whole loop, which is precisely why
	// every closure created in the body observes the same i -- Go's
	// pre-1.22 semantics, pinned in scope_test.go. A condition-only or
	// bare for declares nothing, so it needs no scope of its own.
	scope := env
	if n.Init != nil {
		scope = NewEnv(env)
		if _, err := in.evalStmt(scope, n.Init); err != nil {
			return control{}, err
		}
	}
	for i := 0; ; i++ {
		if i > 100_000_000 {
			return control{}, in.errAt(n, "loop iteration limit exceeded")
		}
		if n.Cond != nil {
			ok, err := in.evalBool(scope, n.Cond)
			if err != nil {
				return control{}, err
			}
			if !ok {
				break
			}
		}
		// n.Body is an *ast.BlockStmt, so evalStmt opens the fresh
		// per-iteration scope itself. Wrapping here as well allocated a
		// second Env -- and its map -- on EVERY iteration, for a scope no
		// declaration could ever reach: a `:=` in the body lands in the
		// block's scope, one level in.
		ctl, err := in.evalStmt(scope, n.Body)
		if err != nil {
			return control{}, err
		}
		if ctl.kind == ctlBreak {
			break
		}
		if ctl.kind == ctlReturn {
			return ctl, nil
		}
		if n.Post != nil {
			if _, err := in.evalStmt(scope, n.Post); err != nil {
				return control{}, err
			}
		}
	}
	return control{}, nil
}

func (in *Interp) evalRange(env *Env, n *ast.RangeStmt) (control, error) {
	x, err := in.eval1(env, n.X)
	if err != nil {
		return control{}, err
	}
	// iterate runs one iteration body; the bool result means "keep going".
	var ret control
	// Whether the clause declares decides whether each iteration needs a
	// scope, and it cannot change between iterations -- so decide once,
	// outside the loop, rather than re-deriving it from the AST every
	// time round.
	declares := rangeVarsDeclare(n)
	iterate := func(k, v Value) (bool, error) {
		// A fresh scope per iteration is what gives `for _, v := range`
		// closures a v of their own (Go 1.22 semantics, and the deliberate
		// asymmetry with the for-clause case above; both are pinned in
		// scope_test.go). It earns that allocation only when there is a
		// binding to put in it: the `for i, v = range` form assigns to
		// bindings that already exist further out, and `for range xs`
		// binds nothing at all.
		scope := env
		if declares {
			scope = NewEnv(env)
		}
		if err := in.bindRangeVar(scope, n.Key, n.Tok, k); err != nil {
			return false, err
		}
		if err := in.bindRangeVar(scope, n.Value, n.Tok, v); err != nil {
			return false, err
		}
		ctl, err := in.evalStmt(scope, n.Body)
		if err != nil {
			return false, err
		}
		switch ctl.kind {
		case ctlBreak:
			return false, nil
		case ctlReturn:
			ret = ctl
			return false, nil
		}
		return true, nil
	}
	if err := in.rangeOver(n, x, iterate); err != nil {
		return control{}, err
	}
	return ret, nil
}

func (in *Interp) bindRangeVar(scope *Env, e ast.Expr, tok token.Token, v Value) error {
	if e == nil {
		return nil
	}
	id, ok := e.(*ast.Ident)
	if !ok {
		return in.errAt(e, "range variable must be an identifier")
	}
	if id.Name == "_" {
		return nil
	}
	if tok == token.DEFINE {
		scope.Define(id.Name, v)
		return nil
	}
	if !scope.Set(id.Name, v) {
		return in.errAt(e, "undefined: "+id.Name)
	}
	return nil
}

// rangeVarsDeclare reports whether a range clause introduces a binding:
// `:=` with at least one variable that is not blank. The `=` form targets
// existing bindings and `for range x` names nothing, and neither needs a
// scope to put a name in.
func rangeVarsDeclare(n *ast.RangeStmt) bool {
	if n.Tok != token.DEFINE {
		return false
	}
	return namesABinding(n.Key) || namesABinding(n.Value)
}

// namesABinding is true for a real identifier -- not nil, not blank.
func namesABinding(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name != "_"
}
