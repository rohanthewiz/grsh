package shellexec

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/rohanthewiz/serr"
)

var builtinNames = map[string]bool{
	"cd": true, "export": true, "unset": true, "exit": true,
	"alias": true, "unalias": true, "source": true, ".": true,
	"jobs": true, "wait": true, "fg": true, "bg": true,
}

func isBuiltin(name string) bool { return builtinNames[name] }

func runBuiltin(st *State, name string, args []string, stdio Stdio) (int, error) {
	switch name {
	case "cd":
		return builtinCd(args, stdio)
	case "export":
		return builtinExport(args, stdio)
	case "unset":
		for _, a := range args {
			_ = os.Unsetenv(a)
		}
		return 0, nil
	case "exit":
		code := 0
		if len(args) > 0 {
			n, err := strconv.Atoi(args[0])
			if err != nil {
				return userErr(stdio, serr.New("exit: numeric argument required", "got", args[0]))
			}
			code = n
		}
		return code, ExitErr{Code: code}
	case "alias":
		return builtinAlias(st, args, stdio)
	case "unalias":
		for _, a := range args {
			delete(st.Aliases, a)
		}
		return 0, nil
	case "source", ".":
		return builtinSource(st, args, stdio)
	case "jobs":
		return builtinJobs(st, stdio)
	case "wait":
		return builtinWait(st, args, stdio)
	case "fg":
		return builtinFg(st, args, stdio)
	case "bg":
		return builtinBg(st, args, stdio)
	}
	return userErr(stdio, serr.New("unknown builtin", "name", name))
}

func builtinJobs(st *State, stdio Stdio) (int, error) {
	for _, ji := range st.Jobs.Snapshot() {
		switch {
		case ji.State == JobDone && ji.Status == 0:
			fmt.Fprintf(stdio.Out, "[%d]  Done       %s\n", ji.ID, ji.Cmdline)
		case ji.State == JobDone:
			fmt.Fprintf(stdio.Out, "[%d]  Exit %-5d %s\n", ji.ID, ji.Status, ji.Cmdline)
		case ji.State == JobStopped:
			fmt.Fprintf(stdio.Out, "[%d]  Stopped    %s\n", ji.ID, ji.Cmdline)
		default:
			fmt.Fprintf(stdio.Out, "[%d]  Running    %s\n", ji.ID, ji.Cmdline)
		}
	}
	// Listing counts as notification: finished jobs are reaped, as in bash.
	_ = st.Jobs.Notifications()
	return 0, nil
}

// builtinWait collects jobs and, like bash, removes them from the table —
// a later `wait %1` must never match an already-collected job. Stopped
// jobs are skipped with a warning: blocking on one would deadlock the
// shell (nothing left to resume it, and the REPL absorbs Ctrl+C).
func builtinWait(st *State, args []string, stdio Stdio) (int, error) {
	if len(args) == 0 {
		for _, j := range st.Jobs.All() {
			if state, _ := st.Jobs.stateOf(j); state == JobStopped {
				fmt.Fprintf(stdio.Err, "grsh: wait: job %d is stopped (fg/bg it first)\n", j.ID)
				continue
			}
			st.Jobs.Wait(j)
			st.Jobs.Reap(j)
		}
		return 0, nil
	}
	status := 0
	for _, spec := range args {
		j := st.Jobs.Find(spec)
		if j == nil {
			fmt.Fprintf(stdio.Err, "grsh: wait: %s: no such job\n", spec)
			return 127, nil
		}
		if state, _ := st.Jobs.stateOf(j); state == JobStopped {
			fmt.Fprintf(stdio.Err, "grsh: wait: job %d is stopped (fg/bg it first)\n", j.ID)
			return 1, nil
		}
		status = st.Jobs.Wait(j)
		st.Jobs.Reap(j)
	}
	return status, nil
}

// builtinFg brings a job to the foreground. A stopped (adopted) job gets
// the terminal back, a SIGCONT, and a suspendable wait; a running
// background (&) job is simply waited for — its stdin is /dev/null, so
// there is no terminal to hand it.
func builtinFg(st *State, args []string, stdio Stdio) (int, error) {
	spec := ""
	if len(args) > 0 {
		spec = args[0]
	}
	j := st.Jobs.Find(spec)
	if j == nil {
		if spec == "" {
			fmt.Fprintf(stdio.Err, "grsh: fg: no current job\n")
		} else {
			fmt.Fprintf(stdio.Err, "grsh: fg: %s: no such job\n", spec)
		}
		return 1, nil
	}
	fmt.Fprintln(stdio.Out, j.Cmdline)

	state, adopted := st.Jobs.stateOf(j)
	// A bg-resumed adopted job is watcher-owned; stop it briefly so fg can
	// take over the wait.
	if adopted && state == JobRunning {
		if st.Jobs.StopForFg(j) {
			state = JobStopped
		}
	}
	if adopted && state == JobStopped {
		tty, _ := interactiveTTY(st, stdio) // nil is fine: resume without handoff
		status, stopped := st.Jobs.ResumeForeground(j, tty)
		if stopped {
			fmt.Fprintf(stdio.Err, "\n[%d]  Stopped    %s\n", j.ID, j.Cmdline)
			return 128 + int(syscall.SIGTSTP), nil
		}
		st.Jobs.Reap(j)
		return status, nil
	}

	status := st.Jobs.Wait(j)
	st.Jobs.Reap(j)
	return status, nil
}

// builtinBg resumes a stopped job in the background.
func builtinBg(st *State, args []string, stdio Stdio) (int, error) {
	spec := ""
	if len(args) > 0 {
		spec = args[0]
	}
	j := st.Jobs.Find(spec)
	if j == nil {
		if spec == "" {
			fmt.Fprintf(stdio.Err, "grsh: bg: no current job\n")
		} else {
			fmt.Fprintf(stdio.Err, "grsh: bg: %s: no such job\n", spec)
		}
		return 1, nil
	}
	state, adopted := st.Jobs.stateOf(j)
	switch {
	case state == JobDone:
		fmt.Fprintf(stdio.Err, "grsh: bg: job %d has terminated\n", j.ID)
		return 1, nil
	case state == JobRunning || !adopted:
		fmt.Fprintf(stdio.Err, "grsh: bg: job %d already in background\n", j.ID)
		return 1, nil
	}
	fmt.Fprintf(stdio.Out, "[%d]  %s\n", j.ID, j.Cmdline)
	st.Jobs.ResumeBackground(j)
	return 0, nil
}

// hasJobSpec reports whether any non-flag argument is a %job spec.
func hasJobSpec(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "%") {
			return true
		}
	}
	return false
}

// jobSignals is the subset of named signals kill accepts for job specs.
var jobSignals = map[string]syscall.Signal{
	"TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "INT": syscall.SIGINT,
	"HUP": syscall.SIGHUP, "QUIT": syscall.SIGQUIT, "USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2, "STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT,
}

// builtinKillJob handles `kill [-SIG] %spec...`. Plain-pid kill never gets
// here — it stays an external command.
func builtinKillJob(st *State, args []string, stdio Stdio) (int, error) {
	sig := syscall.SIGTERM
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		name := strings.TrimPrefix(strings.TrimPrefix(args[0], "-"), "SIG")
		if n, err := strconv.Atoi(name); err == nil {
			sig = syscall.Signal(n)
		} else if s, ok := jobSignals[strings.ToUpper(name)]; ok {
			sig = s
		} else {
			return userErr(stdio, serr.New("kill: invalid signal", "sig", args[0]))
		}
		args = args[1:]
	}
	status := 0
	for _, spec := range args {
		if !strings.HasPrefix(spec, "%") {
			return userErr(stdio, serr.New("kill: mixing pids and job specs is not supported", "arg", spec))
		}
		j := st.Jobs.Find(spec)
		if j == nil {
			fmt.Fprintf(stdio.Err, "grsh: kill: %s: no such job\n", spec)
			status = 1
			continue
		}
		if err := st.Jobs.Signal(j, sig); err != nil {
			fmt.Fprintf(stdio.Err, "grsh: kill: %s: %s\n", spec, serr.StringFromErr(err))
			status = 1
			continue
		}
		// Signaling a stopped adopted job: a terminating signal would sit
		// pending forever, and nobody is waiting to reap the outcome. The
		// background watcher sends SIGCONT and collects either way.
		if state, adopted := st.Jobs.stateOf(j); state == JobStopped && adopted && sig != syscall.SIGSTOP {
			st.Jobs.ResumeBackground(j)
		}
	}
	return status, nil
}

func builtinCd(args []string, stdio Stdio) (int, error) {
	var dir string
	switch {
	case len(args) == 0:
		home, err := os.UserHomeDir()
		if err != nil {
			return userErr(stdio, serr.Wrap(err, "op", "cd"))
		}
		dir = home
	case args[0] == "-":
		dir = os.Getenv("OLDPWD")
		if dir == "" {
			return userErr(stdio, serr.New("cd: OLDPWD not set"))
		}
		fmt.Fprintln(stdio.Out, dir)
	default:
		dir = args[0]
	}
	prev, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintf(stdio.Err, "grsh: cd: %s: %s\n", dir, cdReason(err))
		return 1, nil
	}
	now, _ := os.Getwd()
	_ = os.Setenv("OLDPWD", prev)
	_ = os.Setenv("PWD", now)
	return 0, nil
}

func cdReason(err error) string {
	if os.IsNotExist(err) {
		return "no such file or directory"
	}
	if os.IsPermission(err) {
		return "permission denied"
	}
	return err.Error()
}

func builtinExport(args []string, stdio Stdio) (int, error) {
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if k == "" {
			return userErr(stdio, serr.New("export: invalid name", "arg", a))
		}
		if ok {
			_ = os.Setenv(k, v)
		}
		// `export NAME` without a value: env vars are always exported
		// here (we use the real process environment), so it's a no-op.
	}
	return 0, nil
}

func builtinAlias(st *State, args []string, stdio Stdio) (int, error) {
	if len(args) == 0 {
		names := make([]string, 0, len(st.Aliases))
		for k := range st.Aliases {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Fprintf(stdio.Out, "alias %s='%s'\n", k, st.Aliases[k])
		}
		return 0, nil
	}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			if v, exists := st.Aliases[k]; exists {
				fmt.Fprintf(stdio.Out, "alias %s='%s'\n", k, v)
				continue
			}
			return userErr(stdio, serr.New("alias: not found", "name", k))
		}
		st.Aliases[k] = v
	}
	return 0, nil
}

func builtinSource(st *State, args []string, stdio Stdio) (int, error) {
	if len(args) == 0 {
		return userErr(stdio, serr.New("source: filename argument required"))
	}
	if st.SourceFn == nil {
		return userErr(stdio, serr.New("source: not available in this context"))
	}
	if err := st.SourceFn(args[0]); err != nil {
		if _, isExit := err.(ExitErr); isExit {
			return 0, err // exit inside a sourced file exits the script
		}
		return 1, err
	}
	return st.LastStatus, nil
}
