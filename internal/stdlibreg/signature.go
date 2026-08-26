package stdlibreg

// Signature rendering for the REPL's hint lane.
//
// The registry stores symbols as plain Go values (`"Split": strings.Split`),
// so the only description available at runtime is the reflected TYPE. That
// is a deliberate trade: no doc comments, no parameter NAMES (reflection
// cannot recover them — `strings.Split(s, sep string)` reads back as
// `strings.Split(string, string)`), but also no generated table to keep in
// sync with the registry, and every symbol added to a package file gets a
// hint for free.

import (
	"fmt"
	"io"
	"reflect"
	"strings"
)

// maxLiteralRunes caps the value shown for a non-function symbol. Constants
// are the common case (`math.Pi`), but a registry entry could hold anything,
// and the hint lane is one line under the input.
const maxLiteralRunes = 48

// Signature renders a one-line description of a registered symbol:
//
//	strings.Split(string, string) []string
//	fmt.Printf(string, ...any) (int, error)
//	math.Pi float64 = 3.141592653589793
//
// It reports false for an unregistered package or symbol, so callers can use
// it as the existence check too. Bound symbols (the stdio-dependent ones)
// are resolved against io.Discard: only their TYPE is inspected here, and
// that does not vary with the streams they close over.
func Signature(pkg, sym string) (string, bool) {
	p, ok := packages[pkg]
	if !ok {
		return "", false
	}
	v, ok := p.Symbols[sym]
	if !ok {
		if p.Bound == nil {
			return "", false
		}
		bind, bound := p.Bound[sym]
		if !bound {
			return "", false
		}
		v = bind(io.Discard, io.Discard)
	}

	t := reflect.TypeOf(v)
	if t == nil {
		// A registry entry holding an untyped nil: it exists, but there is
		// nothing to describe beyond that.
		return pkg + "." + sym, true
	}
	name := pkg + "." + sym
	if t.Kind() != reflect.Func {
		return name + " " + typeName(t) + " = " + literal(v), true
	}
	return name + funcSignature(t), true
}

// funcSignature renders the parenthesized part of a func type:
// "(string, ...any) (int, error)". Result lists are parenthesized only when
// there is more than one, matching how the same signature is written in Go.
func funcSignature(t reflect.Type) string {
	var b strings.Builder
	b.WriteByte('(')
	for i := 0; i < t.NumIn(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		if t.IsVariadic() && i == t.NumIn()-1 {
			// The final parameter of a variadic func is stored as a slice;
			// Go source spells it with the element type after the dots.
			b.WriteString("...")
			b.WriteString(typeName(t.In(i).Elem()))
			continue
		}
		b.WriteString(typeName(t.In(i)))
	}
	b.WriteByte(')')

	switch t.NumOut() {
	case 0:
	case 1:
		b.WriteByte(' ')
		b.WriteString(typeName(t.Out(0)))
	default:
		outs := make([]string, t.NumOut())
		for i := range outs {
			outs[i] = typeName(t.Out(i))
		}
		b.WriteString(" (")
		b.WriteString(strings.Join(outs, ", "))
		b.WriteByte(')')
	}
	return b.String()
}

// typeName spells a type the way a grsh script would write it. reflect
// renders the empty interface as "interface {}" (and "[]interface {}" for a
// slice of them); scripts write `any`, so translate rather than teach the
// reader two vocabularies for the same thing.
func typeName(t reflect.Type) string {
	return strings.ReplaceAll(t.String(), "interface {}", "any")
}

// literal formats a non-function symbol's value for the hint, flattened to
// one line and length-capped: the hint lane is a single row under the input,
// and a stray newline in it would cost a row of screen.
func literal(v any) string {
	s := fmt.Sprintf("%v", v)
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > maxLiteralRunes {
		s = string(r[:maxLiteralRunes]) + "…"
	}
	return s
}
