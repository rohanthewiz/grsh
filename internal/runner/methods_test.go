package runner

import (
	"bytes"
	"strings"
	"testing"
)

// evalOut runs source and returns combined output; run errors are fatal.
func evalOut(t *testing.T, src string) string {
	t.Helper()
	var out bytes.Buffer
	sess := NewSession(Options{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
	if err := sess.RunSource("m.grsh", src); err != nil {
		t.Fatalf("RunSource: %v\noutput:\n%s", err, out.String())
	}
	return out.String()
}

func TestStructMethods(t *testing.T) {
	out := evalOut(t, strings.Join([]string{
		`type Point struct {`,
		`	X, Y float64`,
		`}`,
		``,
		`func (p Point) Norm2() float64 {`,
		`	return p.X*p.X + p.Y*p.Y`,
		`}`,
		``,
		`func (p *Point) Scale(f float64) {`,
		`	p.X *= f`,
		`	p.Y *= f`,
		`}`,
		``,
		`p := Point{3, 4}`,
		`fmt.Println(p.Norm2())`,
		`p.Scale(2)`,
		`fmt.Println(p.X, p.Y)`,
	}, "\n"))
	want := "25\n6 8\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// Value receivers see a copy; mutation does not leak (Go semantics).
func TestStructMethodValueReceiverCopies(t *testing.T) {
	out := evalOut(t, strings.Join([]string{
		`type T struct { N int }`,
		`func (t T) Bump() int {`,
		`	t.N = 99`,
		`	return t.N`,
		`}`,
		`v := T{1}`,
		`fmt.Println(v.Bump(), v.N)`,
	}, "\n"))
	if out != "99 1\n" {
		t.Errorf("got %q, want %q", out, "99 1\n")
	}
}

// Methods may call other methods on the same type, including ones
// declared later in the file (hoisting).
func TestStructMethodsCompose(t *testing.T) {
	out := evalOut(t, strings.Join([]string{
		`type Rect struct { W, H float64 }`,
		`func (r Rect) Describe() string {`,
		`	return "area " + fmt.Sprint(r.Area())`,
		`}`,
		`func (r Rect) Area() float64 {`,
		`	return r.W * r.H`,
		`}`,
		`r := Rect{W: 3, H: 5}`,
		`fmt.Println(r.Describe())`,
		`echo from shell: {r.Describe()}`,
	}, "\n"))
	want := "area 15\nfrom shell: area 15\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestStructMethodErrors(t *testing.T) {
	// Unknown method → helpful error.
	err := evalErr(t, "type T struct { N int }\nv := T{1}\nv.Nope()\n")
	if err == nil || !strings.Contains(err.Error(), "unknown method Nope on T") {
		t.Errorf("unknown method: got %v", err)
	}
	// Method used as a value → hint to call it.
	err = evalErr(t, strings.Join([]string{
		`type T struct { N int }`,
		`func (t T) M() int { return t.N }`,
		`v := T{1}`,
		`x := v.M`,
		`fmt.Println(x)`,
	}, "\n"))
	if err == nil || !strings.Contains(err.Error(), "M is a method of T") {
		t.Errorf("method value: got %v", err)
	}
}

// iff is the lazy ternary: only the taken branch evaluates.
func TestIff(t *testing.T) {
	out := evalOut(t, strings.Join([]string{
		`n := 3`,
		`fmt.Println(iff(n > 2, "big", "small"))`,
		`fmt.Println(iff(n > 5, "big", "small"))`,
		// The dead arm would panic/error if it were evaluated.
		`var xs []string`,
		`fmt.Println(iff(len(xs) > 0, xs[0], "empty"))`,
		`fmt.Println(iff(true, "safe", boom()))`,
		// Nesting works like a chained ternary.
		`grade := iff(n >= 90, "A", iff(n >= 3, "B", "C"))`,
		`fmt.Println(grade)`,
		// Usable inside shell interpolation too.
		`echo mode: {iff(n == 3, "three", "other")}`,
	}, "\n"))
	want := "big\nsmall\nempty\nsafe\nB\nmode: three\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestIffErrors(t *testing.T) {
	err := evalErr(t, "x := iff(1, 2, 3)\n")
	if err == nil || !strings.Contains(err.Error(), "condition must be bool") {
		t.Errorf("non-bool cond: got %v", err)
	}
	err = evalErr(t, "x := iff(true, 1)\n")
	if err == nil || !strings.Contains(err.Error(), "iff expects (condition, thenValue, elseValue)") {
		t.Errorf("arity: got %v", err)
	}
}

// A user-defined iff shadows the intrinsic, like the other Go builtins.
func TestIffShadowed(t *testing.T) {
	out := evalOut(t, strings.Join([]string{
		`func iff(a, b, c any) any {`,
		`	return "mine"`,
		`}`,
		`fmt.Println(iff(true, 1, 2))`,
	}, "\n"))
	if out != "mine\n" {
		t.Errorf("got %q, want %q", out, "mine\n")
	}
}
