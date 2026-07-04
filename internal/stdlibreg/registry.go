// Package stdlibreg is the curated registry of Go standard-library (and
// selected third-party) symbols available to grsh scripts. One file per
// package; symbols are plain Go values called through the interpreter's
// single reflect boundary.
package stdlibreg

import (
	"io"
	"sort"
)

type Package struct {
	Name    string // selector name used in scripts, e.g. "fmt"
	Symbols map[string]any
	// Bound symbols depend on the session's stdio (e.g. fmt.Println must
	// write to the session stdout, not the process stdout, so captures
	// and redirection see Go-side output too).
	Bound map[string]func(stdout, stderr io.Writer) any
}

var packages = map[string]*Package{}

func Register(p *Package) { packages[p.Name] = p }

func Lookup(pkg, sym string) (any, bool) {
	p, ok := packages[pkg]
	if !ok {
		return nil, false
	}
	v, ok := p.Symbols[sym]
	return v, ok
}

// LookupBound resolves a stdio-dependent symbol against concrete streams.
func LookupBound(pkg, sym string, stdout, stderr io.Writer) (any, bool) {
	p, ok := packages[pkg]
	if !ok || p.Bound == nil {
		return nil, false
	}
	f, ok := p.Bound[sym]
	if !ok {
		return nil, false
	}
	return f(stdout, stderr), true
}

func Has(pkg string) bool { _, ok := packages[pkg]; return ok }

// Names returns registered package names (for the classifier's rule 6b).
func Names() []string {
	out := make([]string, 0, len(packages))
	for n := range packages {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
