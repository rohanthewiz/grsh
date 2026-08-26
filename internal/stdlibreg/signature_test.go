package stdlibreg

import (
	"strings"
	"testing"
)

// TestSignature pins the rendering of each shape a registry entry can take.
// The expected strings are what the REPL's hint lane shows, so they are
// written out in full rather than pattern-matched.
func TestSignature(t *testing.T) {
	cases := []struct {
		pkg, sym string
		want     string
	}{
		// Plain func: params and a single result, no parens around the result.
		{"strings", "Split", "strings.Split(string, string) []string"},
		// Multiple results are parenthesized, as in Go source.
		{"strings", "Cut", "strings.Cut(string, string) (string, string, bool)"},
		{"strings", "Contains", "strings.Contains(string, string) bool"},
		// Variadic: the final slice parameter is spelled with dots, and the
		// empty interface is spelled `any` (reflect says "interface {}").
		{"fmt", "Sprintf", "fmt.Sprintf(string, ...any) string"},
		// Bound symbol: resolved against discard streams for its type alone.
		{"fmt", "Printf", "fmt.Printf(string, ...any) (int, error)"},
		{"fmt", "Println", "fmt.Println(...any) (int, error)"},
		// Registry entries that are closures over a stdlib call describe the
		// closure — which is the signature scripts actually call.
		{"os", "WriteFile", "os.WriteFile(string, string) error"},
		// Non-func symbol: type and value, like a var declaration.
		{"math", "Pi", "math.Pi float64 = 3.141592653589793"},
		{"math", "MaxInt", "math.MaxInt int = 9223372036854775807"},
	}
	for _, c := range cases {
		got, ok := Signature(c.pkg, c.sym)
		if !ok {
			t.Errorf("Signature(%q, %q) not found", c.pkg, c.sym)
			continue
		}
		if got != c.want {
			t.Errorf("Signature(%q, %q) =\n  %q\nwant\n  %q", c.pkg, c.sym, got, c.want)
		}
	}
}

// TestSignatureMisses: an unknown package or symbol reports false, so callers
// can use Signature as the existence check (it covers bound symbols, which
// Lookup does not).
func TestSignatureMisses(t *testing.T) {
	for _, c := range [][2]string{
		{"nosuchpkg", "Split"},
		{"strings", "NoSuchSymbol"},
		{"strings", ""},
		{"", ""},
	} {
		if got, ok := Signature(c[0], c[1]); ok {
			t.Errorf("Signature(%q, %q) = %q, want not found", c[0], c[1], got)
		}
	}
	// Bound-only symbols are invisible to Lookup but must have signatures.
	if _, ok := Lookup("fmt", "Println"); ok {
		t.Fatal("test premise changed: fmt.Println is no longer bound-only")
	}
	if _, ok := Signature("fmt", "Println"); !ok {
		t.Error("Signature must cover bound symbols, not just plain ones")
	}
}

// TestSignatureCoversRegistry is the guard against a symbol shape that
// crashes or renders emptily: every registered symbol in every package must
// produce a non-empty signature.
func TestSignatureCoversRegistry(t *testing.T) {
	for _, pkg := range Names() {
		for _, sym := range Members(pkg) {
			sig, ok := Signature(pkg, sym)
			if !ok || sig == "" {
				t.Errorf("%s.%s: no signature", pkg, sym)
			}
		}
	}
}

// TestLiteralFlattening: a long or multi-line value is capped and flattened —
// the hint lane is one row under the input.
func TestLiteralFlattening(t *testing.T) {
	long := strings.Repeat("x", 100)
	if got := literal(long + "\ny"); len([]rune(got)) != maxLiteralRunes+1 {
		t.Errorf("literal(long) = %q (%d runes), want %d plus an ellipsis",
			got, len([]rune(got)), maxLiteralRunes)
	}
	if got := literal("a\nb"); got != "a b" {
		t.Errorf("literal(%q) = %q, want newlines flattened", "a\nb", got)
	}
}
