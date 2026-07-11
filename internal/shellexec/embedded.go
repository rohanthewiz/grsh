package shellexec

// Embedded mode: the session lives inside a host program (an editor
// panel, a bot, a test harness) that owns the terminal — or has no
// terminal at all. Foreground pipelines still need to be cancelable
// without signaling the host itself, so they run in their own process
// group like interactive jobs do, but with none of the tty machinery:
//
//	                 script    interactive    embedded
//	own pgroup         no          yes           yes
//	tcsetpgrp/^Z       no          yes           no
//	wait call        c.Wait()   Wait4(-pgid)   c.Wait()
//
// c.Wait (not Wait4) is load-bearing here: hosts pass in-process
// writers (buffers, event pumps), so os/exec wires the children to
// pipes and pumps them with copier goroutines that only Wait flushes.
// Reaping the pgroup directly would race those copiers and drop tail
// output.

import (
	"io"
	"os/exec"
	"syscall"
)

// eofReader is the embedded stand-in for stdin: children see immediate
// EOF instead of a chance to read the host's terminal.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// embeddedPgroup reports whether this pipeline should run in its own
// process group without terminal handoff. Interactive wins if both are
// somehow set — a real tty means real job control is available.
func embeddedPgroup(st *State) bool {
	return st.Embedded && !st.Interactive
}

// runForegroundEmbedded starts pre-wired commands as one new process
// group, registers it as the session's foreground group (so the host's
// Interrupt/Kill can signal it from another goroutine), and waits for
// every member with exec's Wait. closeAfterStart holds the parent's
// pipe fds, mirroring runForegroundJobControl.
func runForegroundEmbedded(st *State, execs []*exec.Cmd, names []string, stdio Stdio, closeAfterStart []io.Closer) (int, error) {
	statuses := make([]int, len(execs))
	started := make([]bool, len(execs))
	pgid := 0
	for i, c := range execs {
		// The leader (Pgid 0) becomes its own group; the rest join it.
		// If the leader exits before a follower starts, that Start fails
		// with EPERM — the same accepted race as background jobs.
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
		if err := c.Start(); err != nil {
			statuses[i], _ = externalStatus(stdio, names[i], err)
			continue
		}
		started[i] = true
		if pgid == 0 {
			pgid = c.Process.Pid
			st.setForegroundPgid(pgid)
		}
	}
	// Evals are strictly sequential per session, so a plain clear (not a
	// compare-and-clear) cannot stomp another pipeline's registration.
	defer st.setForegroundPgid(0)
	closeAll(closeAfterStart)

	for i, c := range execs {
		if !started[i] {
			continue
		}
		if err := c.Wait(); err != nil {
			statuses[i], _ = externalStatus(stdio, names[i], err)
		}
	}
	return pipelineStatus(statuses, st.PipeFail), nil
}
