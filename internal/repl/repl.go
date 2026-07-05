// Package repl implements grsh's interactive mode on top of the
// runner.Session Eval seam. Lines are accumulated until the classifier
// reports a complete input unit (Session.NeedsMore), then evaluated as one
// chunk — so blocks, composite literals, and shell continuations span
// prompts exactly like they span script lines.
package repl

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/chzyer/readline"
	"github.com/rohanthewiz/grsh/internal/runner"
	"github.com/rohanthewiz/grsh/internal/shellexec"
)

// lineReader abstracts chzyer/readline so the loop is testable.
type lineReader interface {
	Readline() (string, error)
	SetPrompt(string)
}

// Run drives the interactive session and returns the process exit code.
func Run(sess *runner.Session, version string) int {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          promptFor(sess, false),
		HistoryFile:     historyPath(),
		AutoComplete:    newCompleter(sess.Idents),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "grsh: %v\n", err)
		return 2
	}
	defer rl.Close()

	// The terminal is in cooked mode while a command runs (readline is raw
	// only inside Readline), so Ctrl+C sends SIGINT to the whole foreground
	// group: the child must die, the shell must not.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	go func() {
		for range sigc {
		}
	}()
	defer signal.Stop(sigc)

	fmt.Printf("grsh %s — type exit or Ctrl+D to quit\n", version)
	return loop(sess, rl, os.Stderr)
}

func loop(sess *runner.Session, rd lineReader, errW io.Writer) int {
	var buf []string
	for {
		rd.SetPrompt(promptFor(sess, len(buf) > 0))
		line, err := rd.Readline()
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			buf = buf[:0] // ^C drops any pending continuation
			continue
		case errors.Is(err, io.EOF):
			if len(buf) > 0 {
				buf = buf[:0] // ^D mid-continuation abandons the unit
				continue
			}
			return sess.LastStatus()
		case err != nil:
			fmt.Fprintf(errW, "grsh: %v\n", err)
			return 2
		}

		buf = append(buf, line)
		src := strings.Join(buf, "\n")
		if strings.TrimSpace(src) == "" {
			buf = buf[:0]
			continue
		}
		if sess.NeedsMore(src) {
			continue
		}
		buf = buf[:0]
		if err := sess.Eval(src); err != nil {
			if xe, ok := errors.AsType[shellexec.ExitErr](err); ok {
				return xe.Code // exit builtin, or errexit tripping (set -e exits the shell)
			}
			fmt.Fprintf(errW, "grsh: %s\n", userMsg(src, err))
		}
	}
}

// evalLoc matches the "<eval>:line[:col]: " prefix RunSource stamps on
// eval errors.
var evalLoc = regexp.MustCompile(`^<eval>:(\d+)(?::\d+)?: `)

// userMsg renders an eval error for the prompt: single-line inputs drop the
// pointless location, multi-line inputs keep just the line number.
func userMsg(src string, err error) string {
	msg := runner.UserMessage(err)
	m := evalLoc.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	if strings.Contains(src, "\n") {
		return "line " + m[1] + ": " + msg[len(m[0]):]
	}
	return msg[len(m[0]):]
}

// promptFor builds "grsh ~/dir> ", flagging a nonzero last status as
// "grsh ~/dir [1]> ". Continuation lines get an ellipsis gutter.
func promptFor(sess *runner.Session, continuation bool) string {
	if continuation {
		return "  ... "
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "?"
	}
	if st := sess.LastStatus(); st != 0 {
		return fmt.Sprintf("grsh %s [%d]> ", abbrevHome(cwd), st)
	}
	return fmt.Sprintf("grsh %s> ", abbrevHome(cwd))
}

func abbrevHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// historyPath returns ~/.grsh_history, or "" (no persistence) when the
// home directory is unknown.
func historyPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".grsh_history")
}
