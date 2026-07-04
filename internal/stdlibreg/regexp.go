package stdlibreg

import "regexp"

func init() {
	Register(&Package{Name: "regexp", Symbols: map[string]any{
		"MatchString": regexp.MatchString,
		"Compile":     regexp.Compile,
		"MustCompile": regexp.MustCompile,
		"QuoteMeta":   regexp.QuoteMeta,
	}})
}
