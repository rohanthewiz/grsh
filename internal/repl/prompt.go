package repl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rohanthewiz/grsh/internal/runner"
	"golang.org/x/term"
)

// The primary prompt is templatable via $GRSH_PROMPT (settable from
// ~/.grshrc with `export`). Escapes:
//
//	%d  cwd, ~-abbreviated        %s  " [N]" when last status nonzero
//	%g  git branch (or empty)     %t  HH:MM
//	%j  " [Nj]" when jobs exist   %%  literal %
//
// Color tags {red} {green} {yellow} {blue} {magenta} {cyan} {bold} {dim}
// {reset} emit ANSI when color is enabled and vanish otherwise (NO_COLOR,
// TERM=dumb, or a non-terminal stdout). Unset GRSH_PROMPT keeps the
// classic colorless "grsh ~/dir [N]> ".
var colorTags = map[string]string{
	"{red}": "\x1b[31m", "{green}": "\x1b[32m", "{yellow}": "\x1b[33m",
	"{blue}": "\x1b[34m", "{magenta}": "\x1b[35m", "{cyan}": "\x1b[36m",
	"{bold}": "\x1b[1m", "{dim}": "\x1b[2m", "{reset}": "\x1b[0m",
}

func renderPrompt(tmpl string, sess *runner.Session, cwd string, now time.Time, color bool) string {
	var b strings.Builder
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] == '{' {
			if j := strings.IndexByte(tmpl[i:], '}'); j >= 0 {
				if esc, ok := colorTags[tmpl[i:i+j+1]]; ok {
					if color {
						b.WriteString(esc)
					}
					i += j
					continue
				}
			}
		}
		if tmpl[i] != '%' || i+1 >= len(tmpl) {
			b.WriteByte(tmpl[i])
			continue
		}
		i++
		switch tmpl[i] {
		case 'd':
			b.WriteString(abbrevHome(cwd))
		case 's':
			if st := sess.LastStatus(); st != 0 {
				fmt.Fprintf(&b, " [%d]", st)
			}
		case 'g':
			b.WriteString(gitBranch(cwd))
		case 't':
			b.WriteString(now.Format("15:04"))
		case 'j':
			if n := sess.JobCount(); n > 0 {
				fmt.Fprintf(&b, " [%dj]", n)
			}
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(tmpl[i])
		}
	}
	return b.String()
}

// colorEnabled follows the informal standard: NO_COLOR wins, dumb
// terminals and non-terminals stay plain.
func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// gitBranch reads .git/HEAD directly (no `git` fork per prompt), walking
// up from dir. Detached HEAD shows the short hash.
func gitBranch(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		head := filepath.Join(d, ".git", "HEAD")
		if b, err := os.ReadFile(head); err == nil {
			s := strings.TrimSpace(string(b))
			if ref, ok := strings.CutPrefix(s, "ref: refs/heads/"); ok {
				return ref
			}
			if len(s) >= 8 {
				return s[:8]
			}
			return s
		}
		if d == filepath.Dir(d) {
			return "" // reached the filesystem root
		}
	}
}
