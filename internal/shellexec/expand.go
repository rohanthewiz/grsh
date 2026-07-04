package shellexec

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rohanthewiz/grsh/internal/shellparse"
	"github.com/rohanthewiz/serr"
)

// ExpandWords expands a word list into argv fields: tilde, $VAR, $(...),
// {expr}, quoting, and glob.
//
// Deliberate departures from bash, chosen for safety and documented in
// LANGUAGE.md:
//   - $VAR expansion is never field-split (zsh-like); a path with spaces
//     stays one argument.
//   - Unquoted $(...) output IS field-split (so `kill $(pgrep x)` works),
//     but the resulting fields are not glob-expanded.
//   - {expr} yielding a string is exactly one field; []string splices.
func ExpandWords(st *State, words []*shellparse.Word, ev WordEvaluator) ([]string, error) {
	var argv []string
	for _, w := range words {
		fields, err := expandWord(st, w, ev)
		if err != nil {
			return nil, err
		}
		argv = append(argv, fields...)
	}
	return argv, nil
}

// field accumulates one argv field alongside a glob pattern where
// characters from quoted contexts are escaped (so `"a*"` never globs).
type field struct {
	plain   strings.Builder
	pattern strings.Builder
	hasGlob bool // an unquoted segment contributed glob metachars
	quoted  bool // word contained an explicitly quoted segment ("" counts)
}

func (f *field) addText(s string, globActive bool) {
	f.plain.WriteString(s)
	if globActive && strings.ContainsAny(s, "*?[") {
		f.hasGlob = true
		f.pattern.WriteString(s)
	} else {
		f.pattern.WriteString(globEscape(s))
	}
}

func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func expandWord(st *State, w *shellparse.Word, ev WordEvaluator) ([]string, error) {
	var done []field
	cur := &field{}

	// splice completes the current field per bash semantics: the first
	// spliced part joins the accumulated prefix, the last part stays open
	// to join any following suffix.
	splice := func(parts []string) {
		for i, part := range parts {
			if i > 0 {
				done = append(done, *cur)
				cur = &field{}
			}
			cur.addText(part, false)
		}
	}

	for si, seg := range w.Segs {
		switch s := seg.(type) {
		case shellparse.Lit:
			text := s.Text
			if si == 0 && !s.Quoted {
				text = expandTilde(text)
			}
			if s.Quoted {
				cur.quoted = true
			}
			cur.addText(text, !s.Quoted)
		case shellparse.EnvVar:
			vals, isSplice := lookupVar(st, s.Name)
			if isSplice {
				// $@ splices to one field per script arg even when quoted
				// ("$@" bash semantics).
				splice(vals)
			} else {
				cur.addText(strings.Join(vals, " "), false)
			}
		case shellparse.CmdSub:
			out, _, err := Capture(st, s.List, ev)
			if err != nil {
				return nil, err
			}
			if s.Quoted {
				cur.quoted = true
				cur.addText(out, false)
			} else {
				splice(strings.Fields(out))
			}
		case shellparse.GoExpr:
			if ev == nil {
				return nil, serr.New("Go interpolation {"+s.Src+"} is not available here",
					"hint", "the Go engine is wired in by the runner")
			}
			vals, err := ev.EvalGoExpr(s.Src)
			if err != nil {
				return nil, serr.Wrap(err, "in", "{"+s.Src+"}")
			}
			if s.Quoted || len(vals) == 1 {
				cur.addText(strings.Join(vals, " "), false)
			} else {
				splice(vals)
			}
		}
	}
	done = append(done, *cur)

	var out []string
	for i := range done {
		f := &done[i]
		plain := f.plain.String()
		// Drop empty fields that came only from expansions (bash drops
		// them too), but keep explicit empties like '' or "".
		if plain == "" && !f.quoted {
			continue
		}
		if f.hasGlob {
			if matches, err := filepath.Glob(f.pattern.String()); err == nil && len(matches) > 0 {
				out = append(out, matches...)
				continue
			}
			// No match: the pattern passes through literally (bash default).
		}
		out = append(out, plain)
	}
	return out, nil
}

// lookupVar resolves $NAME. The second result is true when the value should
// splice into multiple fields ($@).
func lookupVar(st *State, name string) ([]string, bool) {
	switch name {
	case "@":
		return st.ScriptArgs, true
	case "#":
		return []string{strconv.Itoa(len(st.ScriptArgs))}, false
	case "0":
		return []string{st.ScriptName}, false
	}
	if len(name) == 1 && name[0] >= '1' && name[0] <= '9' {
		n := int(name[0] - '1')
		if n < len(st.ScriptArgs) {
			return []string{st.ScriptArgs[n]}, false
		}
		return []string{""}, false
	}
	return []string{os.Getenv(name)}, false
}

func expandTilde(s string) string {
	if s != "~" && !strings.HasPrefix(s, "~/") {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	if s == "~" {
		return home
	}
	return home + s[1:]
}
