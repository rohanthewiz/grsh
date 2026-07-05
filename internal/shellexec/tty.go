package shellexec

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// interactiveTTY returns the terminal when job control applies to this
// pipeline: the session is interactive and the statement's stdin is the
// real terminal (captures and redirected stdins never job-control).
func interactiveTTY(st *State, stdio Stdio) (*os.File, bool) {
	if !st.Interactive {
		return nil, false
	}
	f, ok := stdio.In.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil, false
	}
	return f, true
}

// tcSetpgrp makes pgid the terminal's foreground process group. SIGTTOU
// is ignored only for the duration of the call (the shell is in a
// background group when it takes the terminal back) — a process-wide
// SIG_IGN would leak into children via exec and break ^Z entirely.
func tcSetpgrp(tty *os.File, pgid int) error {
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTOU)
	return unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSPGRP, pgid)
}
