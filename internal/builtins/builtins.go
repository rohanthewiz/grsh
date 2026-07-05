// Package builtins provides the helper functions pre-declared in every
// grsh script's global scope: glob, lines, fields, status(), etc.
package builtins

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rohanthewiz/grsh/internal/shellexec"
	"github.com/rohanthewiz/grsh/internal/shellparse"
	"github.com/rohanthewiz/serr"
)

// Make builds the builtin map bound to a shell state and stdio.
func Make(st *shellexec.State, stdio shellexec.Stdio) map[string]any {
	return map[string]any{
		// sh and capture run dynamically-built command lines at runtime
		// (no {expr} interpolation — build the string with Go instead).
		"sh": func(cmdline string) error {
			list, err := shellparse.Parse(cmdline)
			if err != nil {
				return serr.Wrap(err, "in", "sh()")
			}
			status, err := shellexec.Run(st, list, nil, stdio)
			if err != nil {
				return err
			}
			if status != 0 {
				return serr.New("command failed", "status", strconv.Itoa(status), "cmd", cmdline)
			}
			return nil
		},
		"capture": func(cmdline string) (string, error) {
			list, err := shellparse.Parse(cmdline)
			if err != nil {
				return "", serr.Wrap(err, "in", "capture()")
			}
			out, status, err := shellexec.Capture(st, list, nil)
			if err != nil {
				return "", err
			}
			if status != 0 {
				return out, serr.New("command failed", "status", strconv.Itoa(status), "cmd", cmdline)
			}
			return out, nil
		},
		"glob": func(pat string) []string {
			m, _ := filepath.Glob(pat)
			return m
		},
		"lines": func(s string) []string {
			s = strings.TrimRight(s, "\n")
			if s == "" {
				return nil
			}
			return strings.Split(s, "\n")
		},
		"fields": strings.Fields,
		"trim":   strings.TrimSpace,
		"readFile": func(p string) (string, error) {
			b, err := os.ReadFile(p)
			if err != nil {
				return "", serr.Wrap(err, "op", "readFile")
			}
			return string(b), nil
		},
		"writeFile": func(p, s string) error {
			return os.WriteFile(p, []byte(s), 0644)
		},
		"appendFile": func(p, s string) error {
			f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
			if err != nil {
				return serr.Wrap(err, "op", "appendFile")
			}
			defer f.Close()
			_, err = f.WriteString(s)
			return err
		},
		"exists": func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		"env":    os.Getenv,
		"setenv": func(k, v string) { _ = os.Setenv(k, v) },
		"cd": func(dir string) error {
			prev, _ := os.Getwd()
			if err := os.Chdir(dir); err != nil {
				return serr.Wrap(err, "op", "cd")
			}
			now, _ := os.Getwd()
			_ = os.Setenv("OLDPWD", prev)
			_ = os.Setenv("PWD", now)
			return nil
		},
		"pwd": func() string {
			d, _ := os.Getwd()
			return d
		},
		"args":     func() []string { return st.ScriptArgs },
		"status":   func() int { return st.LastStatus },
		"ok":       func() bool { return st.LastStatus == 0 },
		"errexit":  func(on bool) { st.ErrExit = on },
		"pipefail": func(on bool) { st.PipeFail = on },
	}
}
