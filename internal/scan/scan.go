// Package scan splits .grsh source into logical lines.
// A logical line may span several physical lines via continuations:
//   - a trailing backslash joins the next line (backslash removed)
//   - a trailing |, &&, or || joins the next line (shell pipelines)
//
// Blank and comment-only lines are preserved (with empty/comment text) so
// downstream stages keep accurate line numbers.
package scan

import "strings"

// Line is one logical line of source.
type Line struct {
	Text string // joined text of the logical line
	Num  int    // 1-based physical line number where it starts
}

// Lines splits src into logical lines.
func Lines(src string) []Line {
	phys := strings.Split(src, "\n")
	var out []Line
	for i := 0; i < len(phys); i++ {
		start := i
		text := phys[i]
		for {
			trimmed := strings.TrimRight(text, " \t")
			if joined, ok := continues(trimmed); ok && i+1 < len(phys) {
				i++
				next := strings.TrimLeft(phys[i], " \t")
				text = joined + " " + next
				continue
			}
			break
		}
		out = append(out, Line{Text: text, Num: start + 1})
	}
	return out
}

// continues reports whether a line (right-trimmed) continues onto the next
// physical line, returning the text to join with.
func continues(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if joined, ok := strings.CutSuffix(s, "\\"); ok {
		return strings.TrimRight(joined, " \t"), true
	}
	// Don't treat a comment line's operators as continuations.
	if strings.HasPrefix(strings.TrimLeft(s, " \t"), "#") {
		return "", false
	}
	for _, op := range []string{"&&", "||", "|"} {
		if strings.HasSuffix(s, op) {
			return s, true
		}
	}
	return "", false
}

// IsBlankOrComment reports whether a logical line has nothing to execute.
func IsBlankOrComment(text string) bool {
	t := strings.TrimSpace(text)
	return t == "" || strings.HasPrefix(t, "#")
}
