package stdlibreg

import (
	"fmt"
	"io"
)

func init() {
	Register(&Package{
		Name: "fmt",
		Symbols: map[string]any{
			"Sprintf":  fmt.Sprintf,
			"Sprint":   fmt.Sprint,
			"Sprintln": fmt.Sprintln,
			"Errorf":   fmt.Errorf,
			"Fprintln": fmt.Fprintln,
			"Fprintf":  fmt.Fprintf,
			"Fprint":   fmt.Fprint,
		},
		Bound: map[string]func(stdout, stderr io.Writer) any{
			"Println": func(out, _ io.Writer) any {
				return func(a ...any) (int, error) { return fmt.Fprintln(out, a...) }
			},
			"Print": func(out, _ io.Writer) any {
				return func(a ...any) (int, error) { return fmt.Fprint(out, a...) }
			},
			"Printf": func(out, _ io.Writer) any {
				return func(format string, a ...any) (int, error) { return fmt.Fprintf(out, format, a...) }
			},
		},
	})
}
