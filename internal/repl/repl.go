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
	"time"

	"github.com/chzyer/readline"
	"github.com/rohanthewiz/grsh/internal/classify"
	"github.com/rohanthewiz/grsh/internal/runner"
	"github.com/rohanthewiz/grsh/internal/shellexec"
)

// lineReader abstracts the line editor so the loop is testable and the
// editor is swappable (reeflective by default, chzyer as legacy).
type lineReader interface {
	Readline() (string, error)
	SetPrompt(string)
}

// errInterrupt is the loop's editor-neutral Ctrl-C sentinel: each editor
// adapter translates its library's interrupt error onto it so the loop
// needn't know which readline is driving.
var errInterrupt = errors.New("interrupt")

// chzyerReader wraps the legacy chzyer editor, translating its interrupt
// sentinel. Everything else (SetPrompt, EOF as io.EOF) passes through.
type chzyerReader struct{ *readline.Instance }

func (c chzyerReader) Readline() (string, error) {
	line, err := c.Instance.Readline()
	if errors.Is(err, readline.ErrInterrupt) {
		return line, errInterrupt
	}
	return line, err
}

// Run drives the interactive session and returns the process exit code.
func Run(sess *runner.Session, version string, noRC bool) int {
	// Source the startup file first, before job control is enabled — like
	// bash, ~/.grshrc runs as plain script code. It goes through the same
	// classifier as everything else, so it can mix shell (aliases,
	// exports) and Go (helper functions) freely.
	if !noRC {
		if code, exited := loadRC(sess, os.Stderr); exited {
			return code
		}
	}

	hist := openHistory(unitHistoryPath())
	comp := newCompleter(sess.Idents)

	// Editor selection: reeflective is the default; GRSH_EDITOR=legacy
	// keeps chzyer available as the escape hatch while the new editor
	// proves itself as a daily driver.
	var rd lineReader
	if legacyEditor() {
		rl, err := readline.NewEx(&readline.Config{
			Prompt:          promptFor(sess, false, classify.PendingInfo{}),
			HistoryFile:     historyPath(),
			AutoComplete:    comp,
			InterruptPrompt: "^C",
			EOFPrompt:       "exit",
			// Drop ^Z at the prompt: readline's default handler SIGTSTPs the
			// PARENT process on macOS — suspending the user's outer shell.
			// ^Z during a foreground command suspends that job instead.
			FuncFilterInputRune: func(r rune) (rune, bool) {
				if r == readline.CharCtrlZ {
					return r, false
				}
				return r, true
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "grsh: %v\n", err)
			return 2
		}
		defer rl.Close()
		rd = chzyerReader{rl}
	} else {
		rd = newReefReader(sess, comp, hist)
	}

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

	// Job control. Note: no blanket signal.Ignore here — SIG_IGN survives
	// exec, so children would inherit an ignored SIGTSTP and ^Z would do
	// nothing. grsh itself doesn't need TSTP ignored (readline is raw at
	// the prompt and filters ^Z; a running job owns the terminal), and
	// SIGTTOU is ignored only around tcsetpgrp (see shellexec.tcSetpgrp).
	sess.SetInteractive(true)

	fmt.Printf("grsh %s — type exit or Ctrl+D to quit\n", version)
	return loop(sess, rd, os.Stdout, os.Stderr, hist)
}

// legacyEditor reports whether the user asked for the chzyer editor.
func legacyEditor() bool {
	switch os.Getenv("GRSH_EDITOR") {
	case "legacy", "chzyer":
		return true
	}
	return false
}

func loop(sess *runner.Session, rd lineReader, outW, errW io.Writer, hist *historyStore) int {
	var buf []string
	var pend classify.PendingInfo // continuation state for the prompt
	for {
		if len(buf) == 0 {
			for _, note := range sess.Notifications() {
				fmt.Fprintln(outW, note)
			}
		}
		rd.SetPrompt(promptFor(sess, len(buf) > 0, pend))
		line, err := rd.Readline()
		switch {
		case errors.Is(err, errInterrupt):
			buf, pend = buf[:0], classify.PendingInfo{} // ^C drops any pending continuation
			continue
		case errors.Is(err, io.EOF):
			if len(buf) > 0 {
				buf, pend = buf[:0], classify.PendingInfo{} // ^D mid-continuation abandons the unit
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
		// One speculative classification serves both the continue-reading
		// decision and the breadcrumb/indent in the continuation prompt.
		if pend = sess.Pending(src); pend.NeedsMore {
			continue
		}
		buf, pend = buf[:0], classify.PendingInfo{}
		if replCommand(src, sess, hist, outW, errW) {
			continue
		}
		hist.Append(src)
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
var evalLoc = regexp.MustCompile(`^<eval>:(\d+)(?::(\d+))?: `)

// userMsg renders an eval error for the prompt: single-line inputs drop
// the pointless location, multi-line inputs keep just the line number.
// When the error carries a column, the offending source line is echoed
// with a caret under it, compiler-style:
//
//	grsh: line 2: undefined: nope
//	    x := nope + 1
//	         ^
func userMsg(src string, err error) string {
	msg := runner.UserMessage(err)
	m := evalLoc.FindStringSubmatch(msg)
	if m == nil {
		return msg
	}
	head := msg[len(m[0]):]
	if strings.Contains(src, "\n") {
		head = "line " + m[1] + ": " + head
	}
	return head + caretBlock(src, m[1], m[2])
}

// caretBlock returns "\n  <source line>\n  <caret>" when the position
// resolves inside src, else "". Tabs are flattened to single spaces so
// the caret column stays aligned (columns are byte-based).
func caretBlock(src, lineStr, colStr string) string {
	if colStr == "" {
		return ""
	}
	lineNo, col := atoi(lineStr), atoi(colStr)
	lines := strings.Split(src, "\n")
	if lineNo < 1 || lineNo > len(lines) || col < 1 {
		return ""
	}
	srcLine := strings.ReplaceAll(lines[lineNo-1], "\t", " ")
	if col > len(srcLine)+1 {
		return ""
	}
	return "\n    " + srcLine + "\n    " + strings.Repeat(" ", col-1) + "^"
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// promptFor builds "grsh ~/dir> ", flagging a nonzero last status as
// "grsh ~/dir [1]> ". Continuation lines get an ellipsis gutter carrying
// the open-construct breadcrumb and depth-based indent:
//
//	grsh ~/p> func greet(name string) {
//	  ... func greet ▸   if name == "" {
//	  ... func greet ▸ if ▸     return
//
// The indent is part of the prompt string (purely visual), so history
// entries and the evaluated source stay clean.
func promptFor(sess *runner.Session, continuation bool, pend classify.PendingInfo) string {
	if continuation {
		p := "  ... "
		if len(pend.Constructs) > 0 {
			p += strings.Join(pend.Constructs, " ▸ ") + " ▸ "
		}
		if pend.Depth > 0 {
			p += strings.Repeat("  ", pend.Depth)
		}
		return p
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "?"
	}
	if tmpl := os.Getenv("GRSH_PROMPT"); tmpl != "" {
		return renderPrompt(tmpl, sess, cwd, time.Now(), colorEnabled())
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

// loadRC sources $GRSH_RC or ~/.grshrc. A missing file is silent; an
// error is reported and startup continues (a broken rc must not lock the
// user out of their shell). An explicit `exit` in the rc is honored.
func loadRC(sess *runner.Session, errW io.Writer) (code int, exited bool) {
	path := os.Getenv("GRSH_RC")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return 0, false
		}
		path = filepath.Join(home, ".grshrc")
	}
	if _, err := os.Stat(path); err != nil {
		return 0, false
	}
	if err := sess.RunFile(path); err != nil {
		if xe, ok := errors.AsType[shellexec.ExitErr](err); ok {
			return xe.Code, true
		}
		fmt.Fprintf(errW, "grsh: %s: %s\n", path, runner.UserMessage(err))
	}
	return 0, false
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
