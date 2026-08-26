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
//
// Run is RE-ENTRANT. `source` inside a running script calls back into it
// through the session (shellexec's SourceFn -> Session.RunFile), on this
// same interpreter -- which is the point: the sourced file defines into
// the same globals. What must not be shared is the state keyed to a
// particular parse:
//
//	fset       positions in the outer file mean nothing in another fset
//	exprCache  entries carry positions in fset, so it moves with it
//	errChain   an error unwinding through the outer script is in flight
//	           while a defer that sources runs
//
// Each is saved and restored around the nested run. Before that, a
// sourced file left the caller pointing at the SUB-file's fileset for the
// rest of the script, and every position reported after the source --
// errAt, and the {expr} line remap -- resolved against a file the node
// did not come from, printing `loc[:0:1]`.
//
// The frame stack and the call chain need no such care: both are pushed
// and popped symmetrically, and the recursion limit counting across a
// source is right -- those frames are genuinely on the stack.
func (in *Interp) Run(fset *token.FileSet, f *ast.File) error {
	outer, outerCache, outerChain := in.fset, in.exprCache, in.errChain
	defer func() {
		in.fset, in.exprCache, in.errChain = outer, outerCache, outerChain
	}()

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
		call, isCall := n.X.(*ast.CallExpr)
		if isCall {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "__shell" {
				return in.runShellStmt(env, call)
			}
		}
		vals, err := in.evalExpr(env, n.X)
		if err != nil {
			return control{}, err
		}
		// A call in statement position discards its results, and that used
		// to include its error -- so `mayFail()` succeeded silently while
		// `v := mayFail()` on the same function aborted the script.
		//
		// The inconsistency was with grsh's OWN rule, not with Go's. Go
		// discards it here too, but Go also does not abort on the
		// assignment form; grsh does, deliberately, because a shell that
		// keeps going after a failure is the thing shells are criticised
		// for. Adding a variable to a call should not be what decides
		// whether a failure is noticed.
		//
		//	mayFail()          aborts
		//	v := mayFail()     aborts
		//	_ = mayFail()      ignored — naming it is the opt-out
		//	err := mayFail()   binds err, script decides
		//
		// $(...) is exempt for the reason it always was: a non-zero exit
		// is data there, reported through status().
		//
		// The cost of the rule is that a call whose PURPOSE is to build an
		// error -- a bare `errors.New("x")`, which does nothing either way
		// -- now aborts. There is no way to tell that apart from a call
		// that failed, and the shape is meaningless code in both languages.
		if isCall && !isCaptureCall(n.X) && len(vals) > 0 {
			// Mirrors assignRHS: a nil error arrives as an untyped nil
			// Value, so a successful assertion already means non-nil.
			if last, ok := vals[len(vals)-1].(error); ok && last != nil {
				return control{}, in.wrapAt(n.X, last)
			}
		}
		return control{}, nil

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

// clauseVars returns the names `for i := 0; ...` DECLARES, or nil.
//
// Only `:=` declares. `for i = 0; ...` assigns to a binding that already
// exists further out, and there is nothing per-iteration about a variable
// the loop does not own -- Go treats that form the old way too.
func clauseVars(n *ast.ForStmt) []string {
	as, ok := n.Init.(*ast.AssignStmt)
	if !ok || as.Tok != token.DEFINE {
		return nil
	}
	var names []string
	for _, lhs := range as.Lhs {
		if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
			names = append(names, id.Name)
		}
	}
	return names
}

// loopCaptures reports whether anything in the loop could still be
// holding a clause variable's cell after its iteration ends.
//
// A *ast.FuncLit is the only way, and that is a property of this
// interpreter rather than a guess: a value copied into a container is
// copied (structs included, since Round 6), and the only thing that
// retains an *Env is a Closure. grsh has no address-of operator, so
// there is no `&i` to smuggle a cell out either -- see unaryOp.
//
// The whole ForStmt is scanned, not just the body, because a literal in
// the cond or the post captures exactly as one in the body does.
func loopCaptures(n *ast.ForStmt) bool {
	found := false
	ast.Inspect(n, func(node ast.Node) bool {
		if found {
			return false
		}
		if _, ok := node.(*ast.FuncLit); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

func (in *Interp) evalFor(env *Env, n *ast.ForStmt) (control, error) {
	// The clause scope holds the loop variable from `for i := 0; ...`. A
	// condition-only or bare for declares nothing, so it needs no scope.
	//
	// Under Go 1.22 semantics this scope is no longer where the body
	// reads i from -- it is the HOLDER that carries i between iterations,
	// each of which gets a cell of its own. See below.
	scope := env
	if n.Init != nil {
		scope = NewEnv(env)
		if _, err := in.evalStmt(scope, n.Init); err != nil {
			return control{}, err
		}
	}

	// Go 1.22 made the clause variable per-iteration, so three closures
	// made in three iterations observe three values. grsh used to declare
	// it once and share it -- pre-1.22 semantics, and an asymmetry with
	// range, which has always built a fresh scope per iteration.
	//
	// The copy is only taken when a closure could actually observe it.
	// With no func literal anywhere in the loop, nothing can outlive the
	// iteration that made it, so the shared cell is indistinguishable
	// from per-iteration cells -- and skipping the copy keeps the plain
	// loop at the allocation count TestLoopAllocationShape pins.
	var vars []string
	if n.Init != nil && loopCaptures(n) {
		vars = clauseVars(n)
	}

	// first gates the post statement, which under this scheme runs at the
	// TOP of every iteration but the first rather than at the bottom of
	// each. That is not a stylistic rearrangement -- it is the whole
	// point. Running `i++` at the bottom would advance the cell the
	// iteration's own closures captured, and print 1 2 3 where Go prints
	// 0 1 2. Each iteration must increment ITS OWN copy, before its
	// condition is tested against it:
	//
	//	holder ─copy─> iter ─post─> ─cond─> ─body─> ─copy back─> holder
	//	                 ^                     ^
	//	                 the cell closures made here capture; it is
	//	                 never touched again once the body returns
	//
	// On the shared-cell path the two orders are identical: the sequence
	// is cond, body, post, cond, body, post either way.
	first := true
	for i := 0; ; i++ {
		if i > 100_000_000 {
			return control{}, in.errAt(n, "loop iteration limit exceeded")
		}

		// iter parents to env rather than to scope because it re-declares
		// everything scope holds: scope is built fresh and only Init runs
		// in it, and Init declares only through the `:=` clauseVars reads.
		// So the lookup chain stays the depth it was.
		iter := scope
		if len(vars) > 0 {
			iter = NewEnv(env)
			for _, name := range vars {
				v, _ := scope.Get(name)
				iter.Define(name, v)
			}
		}

		if !first && n.Post != nil {
			if _, err := in.evalStmt(iter, n.Post); err != nil {
				return control{}, err
			}
		}
		first = false

		if n.Cond != nil {
			ok, err := in.evalBool(iter, n.Cond)
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
		ctl, err := in.evalStmt(iter, n.Body)
		if err != nil {
			return control{}, err
		}
		if ctl.kind == ctlBreak {
			break
		}
		if ctl.kind == ctlReturn {
			return ctl, nil
		}

		// The copy back is what carries the iteration's final value into
		// the next one. It sits after the break and return checks because
		// the clause variable does not outlive the loop, so a value copied
		// back on the way out could never be read. `continue` DOES reach
		// it -- a continued iteration still advances the loop.
		for _, name := range vars {
			v, _ := iter.Get(name)
			scope.Set(name, v)
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
		// closures a v of their own -- Go 1.22 semantics, which the
		// for-clause above now matches too. Range got there first, and the
		// two disagreeing was the pin that Round 6 settled; both are still
		// pinned in scope_test.go, now for agreeing.
		//
		// It earns that allocation only when there is a binding to put in
		// it: the `for i, v = range` form assigns to bindings that already
		// exist further out, and `for range xs` binds nothing at all.
		//
		// Note the gate is NOT the for-clause's capture scan. Range always
		// pays, because it declares into the scope on every iteration
		// anyway -- there is no cheaper shared-cell path here to fall back
		// to, which is why this side never had the question.
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
	// The range variable is a storage location too: `for _, v := range xs`
	// over a slice of structs binds a COPY each iteration, so writing to
	// v.Field does not reach back into xs -- which is what Go does, and
	// the reason `for i := range xs { xs[i].F = 1 }` is the spelling that
	// mutates.
	v = copyOnStore(v)
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
