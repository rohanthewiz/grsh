package shellexec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/rohanthewiz/grsh/internal/shellparse"
	"github.com/rohanthewiz/serr"
)

// Run executes a command list and returns the status of the last pipeline.
// User-level failures (command not found, bad redirect) print to stderr and
// surface as a nonzero status, like bash; the error return is reserved for
// exit requests (ExitErr) and internal failures.
func Run(st *State, list *shellparse.CmdList, ev WordEvaluator, stdio Stdio) (int, error) {
	status := 0
	for _, ao := range list.Items {
		var err error
		if ao.Background {
			status, err = launchJob(st, ao, ev, stdio)
		} else {
			status, err = runAndOr(st, ao, ev, stdio)
		}
		if err != nil {
			return status, err
		}
		st.LastStatus = status
	}
	return status, nil
}

// Capture runs a command list buffering stdout, with trailing newlines
// trimmed (command substitution semantics). Stderr passes through.
func Capture(st *State, list *shellparse.CmdList, ev WordEvaluator) (string, int, error) {
	var buf bytes.Buffer
	status, err := Run(st, list, ev, Stdio{In: os.Stdin, Out: &buf, Err: os.Stderr})
	return strings.TrimRight(buf.String(), "\n"), status, err
}

func runAndOr(st *State, ao *shellparse.AndOr, ev WordEvaluator, stdio Stdio) (int, error) {
	status, err := runPipeline(st, ao.Pipes[0], ev, stdio)
	if err != nil {
		return status, err
	}
	for i, op := range ao.Ops {
		st.LastStatus = status
		if op == shellparse.AndOp && status != 0 {
			continue
		}
		if op == shellparse.OrOp && status == 0 {
			continue
		}
		status, err = runPipeline(st, ao.Pipes[i+1], ev, stdio)
		if err != nil {
			return status, err
		}
	}
	return status, nil
}

func runPipeline(st *State, pl *shellparse.Pipeline, ev WordEvaluator, stdio Stdio) (int, error) {
	if len(pl.Cmds) == 1 {
		return runSimple(st, pl.Cmds[0], ev, stdio)
	}
	return runPipes(st, pl.Cmds, ev, stdio)
}

// runSimple runs a single (non-piped) command: builtin or external.
func runSimple(st *State, cmd *shellparse.Command, ev WordEvaluator, stdio Stdio) (int, error) {
	argv, err := ExpandWords(st, cmd.Words, ev)
	if err != nil {
		return userErr(stdio, err)
	}
	sio, closers, err := applyRedirs(st, cmd.Redirs, ev, stdio)
	defer closeAll(closers)
	if err != nil {
		return userErr(stdio, err)
	}
	if len(argv) == 0 {
		return 0, nil // redirs only, e.g. `> file` truncates
	}

	argv = expandAlias(st, argv)

	force := false
	if argv[0] == "command" && len(argv) > 1 {
		argv, force = argv[1:], true
	}
	if !force && isBuiltin(argv[0]) {
		return runBuiltin(st, argv[0], argv[1:], sio)
	}
	// kill with a %job spec is ours; plain-pid kill stays external.
	if !force && argv[0] == "kill" && hasJobSpec(argv[1:]) {
		return builtinKillJob(st, argv[1:], sio)
	}

	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = sio.In, sio.Out, sio.Err
	if err := c.Run(); err != nil {
		return externalStatus(sio, argv[0], err)
	}
	return 0, nil
}

// syncWriter serializes writes to a shared non-file writer. Pipeline
// commands each get an exec copy goroutine for buffer-backed stderr;
// without this they race on the same bytes.Buffer.
type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// runPipes runs cmd1 | cmd2 | ... with real OS pipes.
// The pipeline status is the last command's status (bash default).
func runPipes(st *State, cmds []*shellparse.Command, ev WordEvaluator, stdio Stdio) (int, error) {
	n := len(cmds)

	// Wrap shared non-file writers once per distinct writer (identity is
	// preserved so exec's Stdout==Stderr fd dedup still applies).
	mu := &sync.Mutex{}
	wrapped := map[io.Writer]io.Writer{}
	wrap := func(w io.Writer) io.Writer {
		if w == nil {
			return nil
		}
		if _, ok := w.(*os.File); ok {
			return w
		}
		if sw, ok := wrapped[w]; ok {
			return sw
		}
		sw := &syncWriter{mu: mu, w: w}
		wrapped[w] = sw
		return sw
	}
	stdio.Out = wrap(stdio.Out)
	stdio.Err = wrap(stdio.Err)
	execs := make([]*exec.Cmd, n)
	statuses := make([]int, n)
	var parentFiles []io.Closer // parent-side pipe fds to close after start

	// Build all commands first so expansion errors abort before anything runs.
	for i, cmd := range cmds {
		argv, err := ExpandWords(st, cmd.Words, ev)
		if err != nil {
			return userErr(stdio, err)
		}
		argv = expandAlias(st, argv)
		if len(argv) == 0 {
			return userErr(stdio, serr.New("empty command in pipeline"))
		}
		if argv[0] == "command" && len(argv) > 1 {
			argv = argv[1:]
		} else if isBuiltin(argv[0]) {
			return userErr(stdio, serr.New("builtin '"+argv[0]+"' cannot be used in a pipeline"))
		}
		execs[i] = exec.Command(argv[0], argv[1:]...)
		execs[i].Stderr = stdio.Err
	}

	execs[0].Stdin = stdio.In
	execs[n-1].Stdout = stdio.Out
	for i := 0; i < n-1; i++ {
		pr, pw, err := os.Pipe()
		if err != nil {
			closeAll(parentFiles)
			return 0, serr.Wrap(err, "op", "create pipe")
		}
		execs[i].Stdout = pw
		execs[i+1].Stdin = pr
		parentFiles = append(parentFiles, pr, pw)
	}

	// Per-command redirections override pipe wiring, as in bash.
	var allClosers []io.Closer
	for i, cmd := range cmds {
		if len(cmd.Redirs) == 0 {
			continue
		}
		base := Stdio{In: execs[i].Stdin, Out: execs[i].Stdout, Err: execs[i].Stderr}
		sio, closers, err := applyRedirs(st, cmd.Redirs, ev, base)
		allClosers = append(allClosers, closers...)
		if err != nil {
			closeAll(parentFiles)
			closeAll(allClosers)
			return userErr(stdio, err)
		}
		execs[i].Stdin, execs[i].Stdout, execs[i].Stderr = sio.In, sio.Out, sio.Err
	}
	defer closeAll(allClosers)

	started := make([]bool, n)
	for i, c := range execs {
		if err := c.Start(); err != nil {
			statuses[i], _ = externalStatus(stdio, c.Path, err)
			continue
		}
		started[i] = true
	}
	// Close the parent's copies so readers see EOF when writers exit.
	closeAll(parentFiles)

	for i, c := range execs {
		if !started[i] {
			continue
		}
		if err := c.Wait(); err != nil {
			statuses[i], _ = externalStatus(stdio, c.Path, err)
		}
	}
	return pipelineStatus(statuses, st.PipeFail), nil
}

// pipelineStatus is the last command's status, or with pipefail the
// rightmost nonzero one (bash `set -o pipefail`).
func pipelineStatus(statuses []int, pipefail bool) int {
	if pipefail {
		for i := len(statuses) - 1; i >= 0; i-- {
			if statuses[i] != 0 {
				return statuses[i]
			}
		}
	}
	return statuses[len(statuses)-1]
}

// resolvedRedir is a redirection with its target already expanded to a
// path — safe to apply from a background goroutine (no State/ev access).
type resolvedRedir struct {
	shellparse.Redir
	path string // "" for RedirDup
}

// resolveRedirs expands redirection targets (the State/ev-touching half).
func resolveRedirs(st *State, redirs []shellparse.Redir, ev WordEvaluator) ([]resolvedRedir, error) {
	var out []resolvedRedir
	for _, r := range redirs {
		if r.FD > 2 || r.DupTo > 2 {
			return nil, serr.New("only file descriptors 0, 1, 2 are supported")
		}
		rr := resolvedRedir{Redir: r}
		if r.Target != nil {
			fields, err := ExpandWords(st, []*shellparse.Word{r.Target}, ev)
			if err != nil {
				return nil, err
			}
			if len(fields) != 1 {
				return nil, serr.New("ambiguous redirect", "fields", fmt.Sprint(fields))
			}
			rr.path = fields[0]
		}
		out = append(out, rr)
	}
	return out, nil
}

// applyResolved opens resolved targets in order and layers them over base
// (the fd-only half; goroutine-safe).
func applyResolved(redirs []resolvedRedir, base Stdio) (Stdio, []io.Closer, error) {
	var closers []io.Closer
	fds := map[int]any{0: base.In, 1: base.Out, 2: base.Err}

	open := func(path string, flags int) (*os.File, error) {
		f, err := os.OpenFile(path, flags, 0644)
		if err != nil {
			return nil, serr.Wrap(err, "op", "redirect")
		}
		closers = append(closers, f)
		return f, nil
	}

	for _, r := range redirs {
		switch r.Op {
		case shellparse.RedirOut:
			f, err := open(r.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
			if err != nil {
				return base, closers, err
			}
			fds[r.FD] = f
		case shellparse.RedirAppend:
			f, err := open(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
			if err != nil {
				return base, closers, err
			}
			fds[r.FD] = f
		case shellparse.RedirIn:
			f, err := open(r.path, os.O_RDONLY)
			if err != nil {
				return base, closers, err
			}
			fds[0] = f
		case shellparse.RedirDup:
			fds[r.FD] = fds[r.DupTo]
		case shellparse.RedirOutErr, shellparse.RedirOutErrApp:
			flags := os.O_CREATE | os.O_TRUNC | os.O_WRONLY
			if r.Op == shellparse.RedirOutErrApp {
				flags = os.O_CREATE | os.O_APPEND | os.O_WRONLY
			}
			f, err := open(r.path, flags)
			if err != nil {
				return base, closers, err
			}
			fds[1], fds[2] = f, f
		}
	}

	out := Stdio{}
	if v, ok := fds[0].(io.Reader); ok {
		out.In = v
	}
	if v, ok := fds[1].(io.Writer); ok {
		out.Out = v
	}
	if v, ok := fds[2].(io.Writer); ok {
		out.Err = v
	}
	return out, closers, nil
}

// applyRedirs resolves and applies in one step (the foreground path).
func applyRedirs(st *State, redirs []shellparse.Redir, ev WordEvaluator, base Stdio) (Stdio, []io.Closer, error) {
	resolved, err := resolveRedirs(st, redirs, ev)
	if err != nil {
		return base, nil, err
	}
	return applyResolved(resolved, base)
}

func expandAlias(st *State, argv []string) []string {
	seen := map[string]bool{}
	for len(argv) > 0 {
		val, ok := st.Aliases[argv[0]]
		if !ok || seen[argv[0]] {
			break
		}
		seen[argv[0]] = true
		argv = append(splitQuoted(val), argv[1:]...)
	}
	return argv
}

// splitQuoted splits an alias value into words, honoring single/double
// quotes and backslash escapes (no expansion — aliases are literal).
func splitQuoted(s string) []string {
	var out []string
	var w strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t':
			if inWord {
				out = append(out, w.String())
				w.Reset()
				inWord = false
			}
		case '\'', '"':
			inWord = true
			q := c
			for i++; i < len(s) && s[i] != q; i++ {
				// Inside double quotes, backslash escapes only \ and ";
				// otherwise it is literal (so "%s!\n" keeps its \n).
				if q == '"' && s[i] == '\\' && i+1 < len(s) && (s[i+1] == '\\' || s[i+1] == '"') {
					i++
				}
				w.WriteByte(s[i])
			}
		case '\\':
			inWord = true
			if i+1 < len(s) {
				i++
				w.WriteByte(s[i])
			}
		default:
			inWord = true
			w.WriteByte(c)
		}
	}
	if inWord {
		out = append(out, w.String())
	}
	return out
}

// userErr reports a user-level failure bash-style: message on stderr,
// status 1, script continues (unless errexit).
func userErr(stdio Stdio, err error) (int, error) {
	fmt.Fprintf(stdio.Err, "grsh: %s\n", serr.StringFromErr(err))
	return 1, nil
}

// externalStatus converts an exec error into a shell status.
func externalStatus(stdio Stdio, name string, err error) (int, error) {
	if xe, ok := errors.AsType[*exec.ExitError](err); ok {
		// A signaled process reports 128+signal (bash convention), not
		// Go's -1 (kill %1 → wait %1 must yield 143, not [-1] prompts).
		if ws, ok := xe.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return xe.ExitCode(), nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		fmt.Fprintf(stdio.Err, "grsh: %s: command not found\n", name)
		return 127, nil
	}
	if errors.Is(err, os.ErrPermission) {
		fmt.Fprintf(stdio.Err, "grsh: %s: permission denied\n", name)
		return 126, nil
	}
	fmt.Fprintf(stdio.Err, "grsh: %s: %v\n", name, err)
	return 126, nil
}

func closeAll(cs []io.Closer) {
	for _, c := range cs {
		if c != nil {
			_ = c.Close()
		}
	}
}
