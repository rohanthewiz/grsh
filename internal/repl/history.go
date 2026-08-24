package repl

import (
	"os"
	"path/filepath"
	"strings"
)

// historyStore persists complete input units — a multi-line func or
// heredoc is one entry, not N fragments the way readline's per-physical-
// line history records it. Format is fish-style: one unit per file line,
// backslash and newline escaped. chzyer's own history keeps handling
// up-arrow recall; this store backs `session save` (and, later, unit
// recall and autosuggestions when the editor is swapped).
type historyStore struct {
	path         string
	units        []string // newest last
	sessionStart int      // index of the first unit from this session
}

// openHistory loads path (missing file → empty store). An empty path
// disables persistence but keeps the in-memory store working.
func openHistory(path string) *historyStore {
	h := &historyStore{path: path}
	if path == "" {
		return h
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return h
	}
	for _, ln := range strings.Split(string(b), "\n") {
		if ln == "" {
			continue
		}
		h.units = append(h.units, unescapeUnit(ln))
	}
	h.sessionStart = len(h.units)
	return h
}

// Append records one complete input unit and persists it.
func (h *historyStore) Append(unit string) {
	if strings.TrimSpace(unit) == "" {
		return
	}
	h.units = append(h.units, unit)
	if h.path == "" {
		return
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return // history is a convenience; never break the prompt over it
	}
	defer f.Close()
	_, _ = f.WriteString(escapeUnit(unit) + "\n")
}

// Units returns all stored units, oldest first.
func (h *historyStore) Units() []string { return h.units }

// SessionUnits returns only the units entered in this session.
func (h *historyStore) SessionUnits() []string { return h.units[h.sessionStart:] }

// Last returns the most recent n units (oldest first); n <= 0 or n larger
// than the store returns everything.
func (h *historyStore) Last(n int) []string {
	if n <= 0 || n >= len(h.units) {
		return h.units
	}
	return h.units[len(h.units)-n:]
}

func escapeUnit(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

func unescapeUnit(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == 'n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unitHistoryPath returns ~/.grsh_units, or "" when home is unknown.
func unitHistoryPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".grsh_units")
}
