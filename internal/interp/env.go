package interp

// Value is a script value: native Go values boxed in any. The only
// interpreter-owned types are *Closure (and *StructVal in a later
// milestone); everything else flows to/from stdlib functions unwrapped.
type Value = any

// Env is a lexical scope: a chain of name → value-cell maps. Cells are
// pointers so closures share mutation with their defining scope.
type Env struct {
	parent *Env
	vars   map[string]*Value
}

func NewEnv(parent *Env) *Env {
	return &Env{parent: parent, vars: map[string]*Value{}}
}

func (e *Env) Define(name string, v Value) {
	val := v
	e.vars[name] = &val
}

func (e *Env) cell(name string) (*Value, bool) {
	for sc := e; sc != nil; sc = sc.parent {
		if c, ok := sc.vars[name]; ok {
			return c, true
		}
	}
	return nil, false
}

func (e *Env) Get(name string) (Value, bool) {
	c, ok := e.cell(name)
	if !ok {
		return nil, false
	}
	return *c, true
}

// Set assigns to an existing binding, walking outward. False if unbound.
func (e *Env) Set(name string, v Value) bool {
	c, ok := e.cell(name)
	if !ok {
		return false
	}
	*c = v
	return true
}
