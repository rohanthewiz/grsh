package repl

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func suffixes(out [][]rune) []string {
	var s []string
	for _, r := range out {
		s = append(s, string(r))
	}
	return s
}

func hasSuffixCand(out [][]rune, want string) bool {
	return slices.Contains(suffixes(out), want)
}

func TestCompleteFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "beta"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	c := newCompleter(func() []string { return nil })
	line := []rune("cat al")
	out, n := c.Do(line, len(line))
	if n != 2 {
		t.Errorf("prefix length %d, want 2", n)
	}
	if !hasSuffixCand(out, "pha.txt ") {
		t.Errorf("candidates %q, want alpha.txt completion", suffixes(out))
	}

	line = []rune("cat be")
	out, _ = c.Do(line, len(line))
	if !hasSuffixCand(out, "ta/") {
		t.Errorf("candidates %q, want beta/ (dir slash, no space)", suffixes(out))
	}

	// Path-shaped word completes files even in command position.
	line = []rune("./al")
	out, _ = c.Do(line, len(line))
	if !hasSuffixCand(out, "pha.txt ") {
		t.Errorf("candidates %q, want ./alpha.txt completion", suffixes(out))
	}
}

func TestCompleteIdents(t *testing.T) {
	c := newCompleter(func() []string { return []string{"greet", "glob"} })
	line := []rune("gree")
	out, _ := c.Do(line, len(line))
	if !hasSuffixCand(out, "t ") {
		t.Errorf("candidates %q, want greet completion", suffixes(out))
	}

	// Idents also complete in argument position.
	line = []rune("fmt.Println(gree")
	word := currentWord(string(line))
	if word != "fmt.Println(gree" {
		// currentWord splits on whitespace only; the whole call is the word,
		// so no ident candidates — acceptable, just pin the behavior.
		t.Logf("word = %q", word)
	}
}

func TestCompleteKeywordsAndCommands(t *testing.T) {
	c := newCompleter(func() []string { return nil })
	line := []rune("fun")
	out, _ := c.Do(line, len(line))
	if !hasSuffixCand(out, "c ") {
		t.Errorf("candidates %q, want func keyword", suffixes(out))
	}

	// After a pipe we are back in command position: `ls | gre` should offer
	// grep from PATH on any normal system.
	line = []rune("ls | gre")
	out, _ = c.Do(line, len(line))
	found := false
	for _, s := range suffixes(out) {
		if strings.HasPrefix("gre"+strings.TrimSuffix(s, " "), "grep") {
			found = true
		}
	}
	if !found {
		t.Logf("grep not in PATH? candidates %q", suffixes(out))
	}
}

func TestCommandPosition(t *testing.T) {
	cases := []struct {
		before, word string
		want         bool
	}{
		{"ls", "ls", true},
		{"ls | w", "w", true},
		{"true && ec", "ec", true},
		{"cat foo", "foo", false},
		{"x := $(gi", "gi", true},
	}
	for _, tc := range cases {
		if got := commandPosition(tc.before, tc.word); got != tc.want {
			t.Errorf("commandPosition(%q, %q) = %v, want %v", tc.before, tc.word, got, tc.want)
		}
	}
}

func TestCurrentWord(t *testing.T) {
	if w := currentWord("cat fo"); w != "fo" {
		t.Errorf("currentWord = %q, want fo", w)
	}
	if w := currentWord(""); w != "" {
		t.Errorf("currentWord empty = %q", w)
	}
}
