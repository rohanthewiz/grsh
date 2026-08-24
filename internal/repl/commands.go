package repl

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/rohanthewiz/grsh/internal/runner"
)

// replCommand intercepts prompt-only conveniences before Eval:
//
//	?name                    inspect a Go variable (type + value)
//	session save [N] file    write this session's input units (or the
//	                         last N units) as a runnable script
//
// These are REPL affordances, not language: scripts never see them, and
// they are not recorded into unit history. Returns true when handled.
func replCommand(src string, sess *runner.Session, hist *historyStore, outW, errW io.Writer) bool {
	t := strings.TrimSpace(src)

	if rest, ok := strings.CutPrefix(t, "?"); ok {
		name := strings.TrimSpace(rest)
		if !isIdentName(name) {
			return false // `? weird stuff` falls through to the shell
		}
		if s, ok := sess.Inspect(name); ok {
			fmt.Fprintln(outW, s)
		} else {
			fmt.Fprintf(errW, "grsh: %s is not defined\n", name)
		}
		return true
	}

	fields := strings.Fields(t)
	if len(fields) >= 3 && fields[0] == "session" && fields[1] == "save" {
		saveSession(fields[2:], hist, outW, errW)
		return true
	}
	return false
}

// saveSession writes history units as a runnable grsh script. Interactive
// work and scripts are the same language, so the round-trip is exact.
func saveSession(args []string, hist *historyStore, outW, errW io.Writer) {
	units := hist.SessionUnits()
	path := args[0]
	if n, err := strconv.Atoi(args[0]); err == nil {
		if len(args) < 2 {
			fmt.Fprintln(errW, "grsh: usage: session save [N] file.grsh")
			return
		}
		units = hist.Last(n)
		path = args[1]
	}
	if len(units) == 0 {
		fmt.Fprintln(errW, "grsh: session save: no history units to save")
		return
	}
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(errW, "grsh: session save: %s already exists (choose another name)\n", path)
		return
	}
	var b strings.Builder
	b.WriteString("#!/usr/bin/env grsh\n")
	for _, u := range units {
		b.WriteString("\n" + u + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0755); err != nil {
		fmt.Fprintf(errW, "grsh: session save: %v\n", err)
		return
	}
	fmt.Fprintf(outW, "saved %d unit(s) to %s\n", len(units), path)
}

func isIdentName(s string) bool {
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
