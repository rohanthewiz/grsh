package classify

import "testing"

func TestNeedsMore(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"", false},
		{"# just a comment", false},
		{"ls", false},
		{"echo hi \\", true},
		{"ls |", true},
		{"ls &&", true},
		{"true ||", true},
		{"ls | wc -l", false},
		{"x := 1", false},
		{"x := 1 +", true},
		{"x := (1 +", true},
		{"if true {", true},
		{"if true {\n\tfmt.Println(1)", true},
		{"if true {\n}", false},
		{"for i := 0; i < 3; i++ {\n\ti\n}", false},
		{"func add(a, b int) int {", true},
		{"func add(a, b int) int {\n\treturn a + b\n}", false},
		{"m := map[string]int{", true},
		{"m := map[string]int{\n\t\"a\": 1,\n}", false},
		// Unbalanced shell quote is NOT a continuation — matches script
		// pipeline behavior (shellparse reports it at eval time).
		{"echo \"foo", false},
		// Heredocs keep reading until the delimiter line.
		{"cat <<EOF", true},
		{"cat <<EOF\nhello", true},
		{"cat <<EOF\nhello\nEOF", false},
		{"cat <<-EOF\n\thello\n\tEOF", false},
		{"cat <<'STOP'\n$X\nSTOP", false},
		{"cat <<A <<B\none\nA", true},
		{"cat <<A <<B\none\nA\ntwo\nB", false},
		{"cat <<EOF | wc -l\nbody", true},
		{"cat <<EOF | wc -l\nbody\nEOF", false},
		// Not heredocs: quoted, herestring-ish, Go interpolation shift.
		{"echo '<<EOF'", false},
		{"echo {x << 2}", false},
		{"echo hi # <<EOF", false},
	}
	for _, tc := range cases {
		c := New([]string{"fmt"})
		if got := c.NeedsMore(tc.src); got != tc.want {
			t.Errorf("NeedsMore(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// A predeclared ident with an open call must continue — this exercises the
// clone carrying scope state.
func TestNeedsMorePredeclared(t *testing.T) {
	c := New(nil)
	c.Predeclare("glob")
	if !c.NeedsMore("glob(\"*.go\",") {
		t.Error("open call on predeclared ident should need more input")
	}
}

// NeedsMore must not leak declarations into the real classifier.
func TestNeedsMoreDoesNotMutate(t *testing.T) {
	c := New(nil)
	if c.NeedsMore("y := 1") {
		t.Fatal("complete define reported as incomplete")
	}
	if kind, _ := c.classifyLine("y = 2"); kind != Shell {
		t.Error("speculative classification leaked 'y' into live scope")
	}
	if c.depth != 0 {
		t.Errorf("depth mutated: %d", c.depth)
	}
}

func TestNames(t *testing.T) {
	c := New([]string{"fmt"})
	c.Predeclare("glob", "status")
	if _, err := c.File("count := 1"); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range c.Names() {
		got[n] = true
	}
	for _, want := range []string{"glob", "status", "count", "fmt"} {
		if !got[want] {
			t.Errorf("Names() missing %q", want)
		}
	}
}
