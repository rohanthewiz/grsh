// Package zshimport translates the commonly used parts of a ~/.zshrc
// into a grsh startup file (~/.grshrc).
//
// The translation is deliberately conservative: a line is emitted ACTIVE
// only when its grsh behavior is certain (comments, safe aliases,
// exports, PATH edits). Everything zsh-specific — setopt, bindkey,
// completion setup, functions, plugin-manager sources — is preserved as
// commented lines, with a note, so nothing silently changes meaning and
// the user can port the leftovers by hand.
package zshimport

import (
	"fmt"
	"strings"
)

// Result is a translation report: the generated file plus counts for the
// summary line `grsh init` prints.
type Result struct {
	Output  string
	Active  int // lines emitted as working grsh
	Skipped int // zsh-specific lines preserved as comments
	Todos   int // things worth porting by hand (functions, evals)
}

// zshOnlyWords are leading words with no grsh equivalent; their lines
// are kept as comments rather than run as (failing) commands.
var zshOnlyWords = map[string]bool{
	"setopt": true, "unsetopt": true, "bindkey": true, "zstyle": true,
	"autoload": true, "compinit": true, "zmodload": true, "typeset": true,
	"local": true, "zle": true, "add-zsh-hook": true, "promptinit": true,
	"compdef": true, "emulate": true, "unfunction": true,
}

func isZshOnly(t string) bool { return zshOnlyWords[firstWord(t)] }

func Translate(src string) Result {
	var b strings.Builder
	res := Result{}
	lines := strings.Split(src, "\n")

	emit := func(s string) { b.WriteString(s + "\n") }
	skip := func(line, why string) {
		res.Skipped++
		emit("# [zsh " + why + "] " + line)
	}
	todo := func(why string) {
		res.Todos++
		emit("# TODO(grsh init): " + why)
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		t := strings.TrimSpace(line)

		switch {
		case t == "" || strings.HasPrefix(t, "#"):
			emit(line) // blank lines and comments pass through
			continue

		case isFuncStart(t):
			// zsh functions don't translate mechanically (different
			// parameter/expansion semantics). Preserve the whole body as
			// comments with a porting note.
			name := funcName(t)
			todo(fmt.Sprintf("port function %q to a grsh Go func (func %s() { ... })", name, name))
			end := funcEnd(lines, i)
			for j := i; j <= end; j++ {
				skip(lines[j], "function")
				res.Skipped-- // counted once as a todo, not per line
			}
			res.Skipped++
			i = end
			continue

		case isBlockStart(t):
			// if/case/for scripting blocks are zsh syntax; comment through
			// the closer.
			end := blockEnd(lines, i, blockCloser(t))
			todo("this zsh " + firstWord(t) + "-block needs manual porting (grsh uses Go control flow)")
			for j := i; j <= end; j++ {
				skip(lines[j], "block")
				res.Skipped--
			}
			res.Skipped++
			i = end
			continue

		case strings.HasPrefix(t, "alias "):
			out, ok := translateAlias(t)
			if ok {
				res.Active++
				emit(out)
			} else {
				skip(line, "alias with redirection/pipe — grsh aliases are argv-only")
			}
			continue

		case strings.HasPrefix(t, "export "):
			if strings.ContainsAny(t, "`") || strings.Contains(t, "$(") {
				// Command substitutions in exports usually invoke zsh-isms;
				// grsh $() is close enough that we keep it, but flag it.
				todo("verify this export's command substitution under grsh")
			}
			res.Active++
			emit(t)
			continue

		case isPathEdit(t):
			out := translatePathEdit(t)
			res.Active++
			emit(out)
			continue

		case strings.HasPrefix(t, "source ") || strings.HasPrefix(t, ". "):
			skip(line, "sources a zsh script")
			continue

		case isZshOnly(t):
			skip(line, "no grsh equivalent")
			continue

		case strings.HasPrefix(t, "eval "):
			// `eval "$(starship init zsh)"` and friends: tool inits emit
			// zsh code; the grsh equivalent (if any) is manual.
			todo("tool init line — check whether the tool has plain-env setup")
			skip(line, "eval")
			continue

		case isBareAssign(t):
			name, _, _ := strings.Cut(t, "=")
			skip(line, "shell variable — use export "+t+" or "+name+" := ... in grsh")
			continue
		}

		// Anything else is likely a plain command; commands are the one
		// thing grsh runs the same way. Emit active.
		res.Active++
		emit(line)
	}

	res.Output = b.String()
	return res
}

// translateAlias keeps `alias name='value'` when the value is plain argv
// (grsh aliases split into words; pipes/redirs inside would not re-parse).
func translateAlias(t string) (string, bool) {
	body := strings.TrimSpace(strings.TrimPrefix(t, "alias"))
	// alias -g / -s are zsh global/suffix aliases — no grsh equivalent.
	if strings.HasPrefix(body, "-") {
		return "", false
	}
	_, val, ok := strings.Cut(body, "=")
	if !ok {
		return "", false
	}
	unq := strings.Trim(val, `'"`)
	if strings.ContainsAny(unq, "|<>;&") || strings.Contains(unq, "$(") {
		return "", false
	}
	return t, true
}

func isPathEdit(t string) bool {
	return strings.HasPrefix(t, "PATH=") ||
		strings.HasPrefix(t, "path+=(") || strings.HasPrefix(t, "path=(")
}

// translatePathEdit converts zsh's array-style path edits to PATH exports.
func translatePathEdit(t string) string {
	if rest, ok := strings.CutPrefix(t, "path+=("); ok {
		dirs := strings.Fields(strings.TrimSuffix(rest, ")"))
		return `export PATH="$PATH:` + strings.Join(dirs, ":") + `"`
	}
	if rest, ok := strings.CutPrefix(t, "path=("); ok {
		dirs := strings.Fields(strings.TrimSuffix(rest, ")"))
		for i, d := range dirs {
			if d == "$path" || d == "${path}" {
				dirs[i] = "$PATH"
			}
		}
		return `export PATH="` + strings.Join(dirs, ":") + `"`
	}
	return "export " + t
}

func isFuncStart(t string) bool {
	if strings.HasPrefix(t, "function ") {
		return true
	}
	// name() { — identifier immediately followed by ().
	name := firstWord(t)
	base, ok := strings.CutSuffix(name, "()")
	if !ok || base == "" {
		return false
	}
	return isIdent(base)
}

func funcName(t string) string {
	t = strings.TrimPrefix(t, "function ")
	name := firstWord(t)
	return strings.TrimSuffix(name, "()")
}

// funcEnd finds the closing } of a brace-counted body starting at line i.
func funcEnd(lines []string, i int) int {
	depth := 0
	for j := i; j < len(lines); j++ {
		depth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
		if depth <= 0 && strings.Contains(lines[j], "}") {
			return j
		}
	}
	return len(lines) - 1
}

func isBlockStart(t string) bool {
	switch firstWord(t) {
	case "if", "case", "for", "while", "until":
		return true
	}
	return false
}

func blockCloser(t string) string {
	switch firstWord(t) {
	case "if":
		return "fi"
	case "case":
		return "esac"
	default:
		return "done"
	}
}

func blockEnd(lines []string, i int, closer string) int {
	opener := firstWord(strings.TrimSpace(lines[i]))
	depth := 0
	for j := i; j < len(lines); j++ {
		w := firstWord(strings.TrimSpace(lines[j]))
		if w == opener {
			depth++
		}
		if w == closer || strings.TrimSpace(lines[j]) == closer {
			depth--
			if depth <= 0 {
				return j
			}
		}
	}
	return len(lines) - 1
}

func firstWord(t string) string {
	if i := strings.IndexAny(t, " \t"); i >= 0 {
		return t[:i]
	}
	return t
}

func isBareAssign(t string) bool {
	name, _, ok := strings.Cut(t, "=")
	return ok && isIdent(name)
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}
