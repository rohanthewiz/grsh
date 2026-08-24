package repl

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/rohanthewiz/grsh/internal/shellexec"
	"github.com/rohanthewiz/grsh/internal/stdlibreg"
)

// goKeywords a user plausibly starts an input line with.
var goKeywords = []string{
	"if", "for", "func", "var", "const", "type", "return",
	"switch", "defer", "import",
}

// completer implements readline.AutoCompleter.
//
//	command position → PATH executables, known idents, Go keywords
//	path-shaped word → directory listing
//	argument         → directory listing plus known idents
type completer struct {
	idents func() []string // live: builtins + user declarations + packages

	pathOnce sync.Once
	pathCmds []string
}

func newCompleter(idents func() []string) *completer {
	return &completer{idents: idents}
}

func (c *completer) Do(line []rune, pos int) ([][]rune, int) {
	before := string(line[:pos])
	word := currentWord(before)
	if word == "" {
		return nil, 0
	}

	var cands []string
	switch {
	case pathShaped(word):
		cands = fileCandidates(word)
	case selectorShaped(word):
		cands = memberCandidates(word)
	case commandPosition(before, word):
		cands = append(cands, c.commands()...)
		cands = append(cands, shellexec.BuiltinNames()...)
		cands = append(cands, c.idents()...)
		cands = append(cands, goKeywords...)
	default:
		cands = append(cands, fileCandidates(word)...)
		cands = append(cands, c.idents()...)
	}

	seen := map[string]bool{}
	var out [][]rune
	for _, cand := range cands {
		if !strings.HasPrefix(cand, word) || seen[cand] {
			continue
		}
		seen[cand] = true
		suffix := cand[len(word):]
		if !strings.HasSuffix(cand, "/") {
			suffix += " "
		}
		out = append(out, []rune(suffix))
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i]) < string(out[j]) })
	return out, len([]rune(word))
}

// currentWord is the run of non-space characters immediately before the
// cursor.
func currentWord(before string) string {
	i := strings.LastIndexAny(before, " \t")
	return before[i+1:]
}

func pathShaped(word string) bool {
	return strings.ContainsRune(word, '/') ||
		strings.HasPrefix(word, ".") || strings.HasPrefix(word, "~")
}

// selectorShaped reports whether word is pkg.Partial for a registered
// package — `fmt.Pr` completes to fmt.Print/Printf/... . Checked before
// commandPosition: a selector at the start of a line is Go, not a command.
func selectorShaped(word string) bool {
	pkg, _, ok := strings.Cut(word, ".")
	return ok && stdlibreg.Has(pkg)
}

// memberCandidates returns pkg.Member forms for every member of word's
// package (prefix filtering happens in Do, like every other source).
func memberCandidates(word string) []string {
	pkg, _, _ := strings.Cut(word, ".")
	members := stdlibreg.Members(pkg)
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, pkg+"."+m)
	}
	return out
}

// commandPosition reports whether word starts an input line or follows a
// pipe/logical operator — the places a command name goes.
func commandPosition(before, word string) bool {
	rest := strings.TrimSpace(before[:len(before)-len(word)])
	if rest == "" {
		return true
	}
	for _, op := range []string{"|", "&&", "||", ";", "$("} {
		if strings.HasSuffix(rest, op) {
			return true
		}
	}
	return false
}

// fileCandidates lists directory entries matching the word's base prefix.
// A leading ~ is expanded for the lookup but kept in the returned form.
// Directories complete with a trailing slash.
func fileCandidates(word string) []string {
	lookup := word
	if strings.HasPrefix(word, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			lookup = home + word[1:]
		}
	}
	dir, prefix := filepath.Split(lookup)
	scanDir := dir
	if scanDir == "" {
		scanDir = "."
	}
	entries, err := os.ReadDir(scanDir)
	if err != nil {
		return nil
	}
	typedDir := word[:len(word)-len(prefix)]
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if prefix == "" && strings.HasPrefix(name, ".") {
			continue // hidden files only when explicitly asked for
		}
		cand := typedDir + name
		if e.IsDir() {
			cand += "/"
		}
		out = append(out, cand)
	}
	return out
}

// commands scans $PATH once per session for executable names.
func (c *completer) commands() []string {
	c.pathOnce.Do(func() {
		seen := map[string]bool{}
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				name := e.Name()
				if seen[name] {
					continue
				}
				info, err := e.Info()
				if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
					continue
				}
				seen[name] = true
				c.pathCmds = append(c.pathCmds, name)
			}
		}
	})
	return c.pathCmds
}
