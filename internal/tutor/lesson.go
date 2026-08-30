package tutor

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
)

// Step is one graded exercise.
type Step struct {
	ID       string   // stable slug, used by progress records and tests
	Prose    []string // the task, one entry per rendered line
	Try      string   // optional literal to show as a starting point
	Hints    []string // revealed one at a time as attempts pile up
	Solution string   // canonical answer; must pass Verify (content test)
	Verify   Verifier
}

// Lesson is one chapter: an ordered list of steps plus its display title.
type Lesson struct {
	ID    string
	Title string
	Steps []Step
}

// content holds the curriculum. Chapters are data, not Go source, so
// writing a lesson never means touching the engine — the whole point of
// the format below. The pattern takes only the numbered chapter files, so
// FORMAT.md (the authoring guide) can live beside them.
//
//go:embed content/[0-9]*.md
var content embed.FS

// lessons parses the embedded chapters once, in filename order.
//
// A parse failure panics rather than degrading: the content is compiled
// into the binary and covered by TestContentParses, so a bad file cannot
// reach a user — but if one somehow did, a half-loaded curriculum that
// silently skipped chapter 5 would be far worse than a loud failure.
var lessons = sync.OnceValue(func() []Lesson {
	names, err := fs.Glob(content, "content/*.md")
	if err != nil {
		panic("tutor: " + err.Error())
	}
	// fs.Glob sorts, and the files are zero-padded (01-…, 02-…), so
	// filename order is chapter order with no index to keep in sync.
	out := make([]Lesson, 0, len(names))
	for _, name := range names {
		b, err := content.ReadFile(name)
		if err != nil {
			panic("tutor: " + err.Error())
		}
		l, err := parseLesson(lessonID(name), string(b))
		if err != nil {
			panic(fmt.Sprintf("tutor: %s: %v", name, err))
		}
		out = append(out, l)
	}
	return out
})

// lessonID turns "content/03-go.md" into "03-go". The ID is what progress
// records are keyed by, so it must not drift: renaming a chapter file
// resets that chapter's saved place, which is the honest outcome — the
// steps inside it were renumbered too.
func lessonID(name string) string {
	return strings.TrimSuffix(path.Base(name), ".md")
}

// parseLesson reads one chapter file.
//
// The format is markdown-shaped so a chapter reads as prose in an editor,
// with a single structural rule: each step is a `## step: <id>` heading,
// its prose runs to a `---` line, and directives follow.
//
//	# It's just a shell
//
//	## step: pipe-count
//	Count the Go files here with a pipe.
//	---
//	verify: output-regexp (?m)^\s*3\s*$
//	hint: `ls *.go` lists them; pipe into `wc -l` as in bash.
//	solution: ls *.go | wc -l
//
// Directives repeat rather than growing a nested syntax: several `hint:`
// lines are the ordered hint list, and several `verify:` lines are
// conjoined with All — a step that must demand the mechanism AND the
// result ("capture with $(...) and print 17") says so with two lines and
// no new grammar.
//
// A directive with an empty value opens an indented block, which is how a
// multi-line answer survives the format:
//
//	solution:
//	  for _, f := range glob("*.go") {
//	      fmt.Println(f)
//	  }
//
// The block is dedented by its first line's indent, so the lesson file
// stays readable while the student sees the answer at column zero.
func parseLesson(id, src string) (Lesson, error) {
	l := Lesson{ID: id}
	lines := strings.Split(src, "\n")

	var cur *Step
	var specs []string // this step's verify lines, conjoined at flush
	inProse := false   // before the `---` separator
	var block *string  // directive currently taking indented lines
	var blockLines []string
	blockIndent := ""

	// flushBlock closes an open indented directive block.
	flushBlock := func() {
		if block == nil {
			return
		}
		// Blank lines at the edges belong to the file's layout, not to the
		// answer: a block ends at the next step heading, and the blank line
		// that separates them would otherwise become a trailing newline the
		// student sees echoed back as part of the solution.
		*block = strings.Join(trimBlankEdges(blockLines), "\n")
		block, blockLines, blockIndent = nil, nil, ""
	}
	// flush finishes the step under construction.
	flush := func() error {
		flushBlock()
		if cur == nil {
			return nil
		}
		if len(specs) == 0 {
			return fmt.Errorf("step %q: no verify line", cur.ID)
		}
		vs := make([]Verifier, len(specs))
		for i, s := range specs {
			v, err := ParseVerifier(s)
			if err != nil {
				return fmt.Errorf("step %q: %w", cur.ID, err)
			}
			vs[i] = v
		}
		cur.Verify = All(vs...)
		cur.Prose = trimBlankEdges(cur.Prose)
		l.Steps = append(l.Steps, *cur)
		cur, specs = nil, nil
		return nil
	}

	for n, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")

		// An open block swallows indented lines (and blank ones, which a
		// multi-line answer may contain) until something at column zero
		// ends it.
		if block != nil {
			if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				if blockIndent == "" && line != "" {
					blockIndent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
				}
				blockLines = append(blockLines, strings.TrimPrefix(line, blockIndent))
				continue
			}
			flushBlock()
		}

		switch {
		case strings.HasPrefix(line, "# "):
			if cur != nil {
				return l, fmt.Errorf("line %d: chapter title inside step %q", n+1, cur.ID)
			}
			l.Title = strings.TrimSpace(line[2:])
			continue

		case strings.HasPrefix(line, "## step:"):
			if err := flush(); err != nil {
				return l, err
			}
			stepID := strings.TrimSpace(line[len("## step:"):])
			if stepID == "" {
				return l, fmt.Errorf("line %d: step needs an id", n+1)
			}
			cur = &Step{ID: stepID}
			inProse = true
			continue

		case line == "---" && cur != nil && inProse:
			inProse = false
			continue
		}

		if cur == nil {
			// Front matter and any prose between steps is commentary for
			// whoever edits the file; the engine has no place to show it.
			continue
		}
		if inProse {
			cur.Prose = append(cur.Prose, line)
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue // blank lines separate directives readably
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return l, fmt.Errorf("line %d: step %q: not a directive (want `key: value`): %q", n+1, cur.ID, line)
		}
		val = strings.TrimSpace(val)
		switch strings.TrimSpace(key) {
		case "verify":
			specs = append(specs, val)
		case "hint":
			cur.Hints = append(cur.Hints, val)
		case "solution":
			if val == "" {
				block = &cur.Solution
				continue
			}
			cur.Solution = val
		case "try":
			if val == "" {
				block = &cur.Try
				continue
			}
			cur.Try = val
		default:
			return l, fmt.Errorf("line %d: step %q: unknown directive %q", n+1, cur.ID, key)
		}
	}
	if err := flush(); err != nil {
		return l, err
	}
	if l.Title == "" {
		return l, fmt.Errorf("chapter has no `# Title` line")
	}
	if len(l.Steps) == 0 {
		return l, fmt.Errorf("chapter has no steps")
	}
	return l, nil
}

// trimBlankEdges drops leading and trailing blank prose lines so the
// panel's own spacing is the only spacing, however the file was laid out.
func trimBlankEdges(ls []string) []string {
	for len(ls) > 0 && strings.TrimSpace(ls[0]) == "" {
		ls = ls[1:]
	}
	for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}
