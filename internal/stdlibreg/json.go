package stdlibreg

import "encoding/json"

// The json surface is adapted for scripting: Marshal returns string, and
// Parse replaces Unmarshal (scripts have no pointers to pass).
func init() {
	Register(&Package{Name: "json", Symbols: map[string]any{
		"Marshal": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
		"MarshalIndent": func(v any) (string, error) {
			b, err := json.MarshalIndent(v, "", "  ")
			return string(b), err
		},
		"Parse": func(s string) (any, error) {
			var v any
			err := json.Unmarshal([]byte(s), &v)
			return v, err
		},
		"Valid": func(s string) bool { return json.Valid([]byte(s)) },
	}})
}
