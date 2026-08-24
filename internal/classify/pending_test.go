package classify

import (
	"strings"
	"testing"
)

// TestPendingConstructs: the speculative continuation state must report
// depth and the construct breadcrumb without mutating the classifier.
func TestPendingConstructs(t *testing.T) {
	tests := []struct {
		name      string
		src       string
		needsMore bool
		depth     int
		crumbs    string // "▸"-joined Constructs, "" for none
	}{
		{"complete shell", "echo hi", false, 0, ""},
		{"open func", "func greet(name string) {", true, 1, "func greet"},
		{"func then for", "func greet(n string) {\nfor i := range 3 {", true, 2, "func greet▸for"},
		{"if inside func", "func f() {\nif true {", true, 2, "func f▸if"},
		{"closed again", "func f() {\nif true {\n}\n}", false, 0, ""},
		{"closure", "handler := func() {", true, 1, "func handler"},
		{"bare block", "{", true, 1, "{"},
		{"switch", "switch x := 1; x {", true, 1, "switch"},
		{"else branch", "if true {\nx := 1\n} else {", true, 1, "else"},
		{"shell continuation", "ls |", true, 0, ""},
		{"mid-statement go", "x := (1 +", true, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := New(nil)
			c.Predeclare("x")
			info := c.Pending(tc.src)
			if info.NeedsMore != tc.needsMore {
				t.Errorf("NeedsMore = %v, want %v", info.NeedsMore, tc.needsMore)
			}
			if info.Depth != tc.depth {
				t.Errorf("Depth = %d, want %d", info.Depth, tc.depth)
			}
			got := strings.Join(info.Constructs, "▸")
			if got != tc.crumbs {
				t.Errorf("Constructs = %q, want %q", got, tc.crumbs)
			}
			// Non-mutating: a second identical call sees the same state.
			again := c.Pending(tc.src)
			if again.Depth != info.Depth || len(again.Constructs) != len(info.Constructs) {
				t.Error("Pending mutated classifier state")
			}
		})
	}
}
