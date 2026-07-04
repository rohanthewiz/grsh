package stdlibreg

import "errors"

func init() {
	Register(&Package{Name: "errors", Symbols: map[string]any{
		"New":    errors.New,
		"Is":     errors.Is,
		"Unwrap": errors.Unwrap,
		"Join":   errors.Join,
	}})
}
