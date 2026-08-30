package tutor

import (
	"strings"
	"testing"
)

// TestParseLessonShape covers the format's whole surface in one file:
// title, prose, the `---` separator, repeated directives, and an indented
// block.
func TestParseLessonShape(t *testing.T) {
	src := `# A Chapter

Editor notes before the first step are not rendered anywhere.

## step: one
First line.

Second paragraph.
---
try: echo hi
verify: output-regexp (?m)^hi$
hint: first
hint: second
solution: echo hi

## step: two
Body.
---
verify: used-construct range
verify: output-regexp x
solution:
  for i := range 3 {
      fmt.Println(i)
  }
`
	l, err := parseLesson("07-x", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if l.ID != "07-x" || l.Title != "A Chapter" {
		t.Fatalf("id/title = %q/%q", l.ID, l.Title)
	}
	if len(l.Steps) != 2 {
		t.Fatalf("%d steps, want 2", len(l.Steps))
	}

	one := l.Steps[0]
	// Prose keeps its internal blank line (the panel renders it) but not
	// the blank the `---` line leaves behind.
	if got := strings.Join(one.Prose, "|"); got != "First line.||Second paragraph." {
		t.Errorf("prose = %q", got)
	}
	if one.Try != "echo hi" {
		t.Errorf("try = %q", one.Try)
	}
	if len(one.Hints) != 2 || one.Hints[1] != "second" {
		t.Errorf("hints = %q", one.Hints)
	}

	// An indented block becomes a real multi-line answer, dedented by its
	// own first line so the student sees it at column zero.
	want := "for i := range 3 {\n    fmt.Println(i)\n}"
	if l.Steps[1].Solution != want {
		t.Errorf("block solution =\n%q\nwant\n%q", l.Steps[1].Solution, want)
	}
}

// TestParseLessonConjoinsVerifies: repeated `verify:` lines are the format's
// answer to "this step must demand the mechanism AND the result", with no
// extra grammar to learn.
func TestParseLessonConjoinsVerifies(t *testing.T) {
	l, err := parseLesson("x", "# T\n\n## step: s\np\n---\nverify: used-construct \\$\\(\nverify: output-regexp ^ok$\nsolution: echo ok\n")
	if err != nil {
		t.Fatal(err)
	}
	v := l.Steps[0].Verify
	if !strings.Contains(v.Spec(), "&&") {
		t.Fatalf("two verify lines did not conjoin: %s", v.Spec())
	}
	// The result alone is not enough — that is the whole point.
	if v.Verify(Attempt{Input: "echo ok", Output: "ok"}) {
		t.Error("passed without the required construct")
	}
	if !v.Verify(Attempt{Input: `x := $(echo ok)`, Output: "ok"}) {
		t.Error("failed with both clauses satisfied")
	}
}

// TestParseLessonRejects: every malformed chapter must be a build failure,
// since lessons() panics rather than shipping a half-loaded curriculum.
func TestParseLessonRejects(t *testing.T) {
	cases := map[string]string{
		"no title":       "## step: s\np\n---\nverify: any-input\n",
		"no steps":       "# T\n\njust prose\n",
		"no verify":      "# T\n\n## step: s\np\n---\nsolution: echo hi\n",
		"bad verify":     "# T\n\n## step: s\np\n---\nverify: no-such-kind x\n",
		"unknown key":    "# T\n\n## step: s\np\n---\nverify: any-input\nnote: hello\n",
		"anonymous step": "# T\n\n## step:\np\n---\nverify: any-input\n",
	}
	for name, src := range cases {
		if _, err := parseLesson("x", src); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

// TestInlineCodeSpans: prose is markdown-shaped, and backticks are its one
// piece of inline markup. With color off they stay literal — they are the
// only emphasis a NO_COLOR terminal has.
func TestInlineCodeSpans(t *testing.T) {
	line := "pipe into `wc -l` like in bash"
	if got := newStyle(false).inline(line); got != line {
		t.Errorf("color off changed the line: %q", got)
	}
	// Stars are the other half of the rule: emphasis has no plain-text
	// convention, so with color off the marks are dropped rather than left
	// as noise around the word.
	if got := newStyle(false).inline("it is **never** split"); got != "it is never split" {
		t.Errorf("color off kept the stars: %q", got)
	}
	if got := newStyle(true).inline("it is **never** split"); !strings.Contains(got, "\x1b[1mnever\x1b[0m") {
		t.Errorf("stars not bolded: %q", got)
	}
	got := newStyle(true).inline(line)
	if !strings.Contains(got, "\x1b[1;36mwc -l\x1b[0m") {
		t.Errorf("span not colored: %q", got)
	}
	if strings.Contains(got, "`") {
		t.Errorf("backticks survived the colored render: %q", got)
	}
	// An unpaired backtick is left exactly as written rather than eating
	// the rest of the line.
	odd := "a ` b"
	if got := newStyle(true).inline(odd); got != odd {
		t.Errorf("unpaired backtick mangled: %q", got)
	}
}
