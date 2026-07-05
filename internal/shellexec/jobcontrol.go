package shellexec

// Job control phase 2: interactive foreground pipelines run in their own
// process group with the terminal, and the shell waits with WUNTRACED —
// Ctrl+Z stops the group, the shell adopts it as a Stopped job, and
// fg/bg resume it with SIGCONT. Script mode never enters these paths.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// waitStatusCode converts a wait status to a shell status (128+signal
// for signaled processes, bash convention).
func waitStatusCode(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// runForegroundJobControl starts pre-wired commands as one foreground
// process group and waits with WUNTRACED. On suspension the pipeline is
// adopted into the job table and the shell takes the terminal back.
// closeAfterStart holds the parent's pipe fds.
func runForegroundJobControl(st *State, execs []*exec.Cmd, names []string, tty *os.File, stdio Stdio, closeAfterStart []io.Closer) (int, error) {
	shellPgid := syscall.Getpgrp()
	defer func() { _ = tcSetpgrp(tty, shellPgid) }()

	statuses := make([]int, len(execs))
	order := map[int]int{} // pid → pipeline position
	pgid := 0
	for i, c := range execs {
		attr := &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
		if pgid == 0 {
			// The leader takes the terminal before exec (tcsetpgrp in the
			// forked child), closing the window where a fast command could
			// read the tty from a background group.
			attr.Foreground = true
			attr.Ctty = int(tty.Fd())
		}
		c.SysProcAttr = attr
		if err := c.Start(); err != nil {
			statuses[i], _ = externalStatus(stdio, names[i], err)
			continue
		}
		if pgid == 0 {
			pgid = c.Process.Pid
		}
		order[c.Process.Pid] = i
	}
	closeAll(closeAfterStart)

	left := map[int]bool{}
	for pid := range order {
		left[pid] = true
	}
	for len(left) > 0 {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-pgid, &ws, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			break
		}
		if ws.Stopped() {
			// The stop signal went to the whole group: adopt every process
			// that has not exited yet, in pipeline order.
			pids := orderedLeft(order, left)
			job := st.Jobs.AdoptStopped(strings.Join(names, " | "), pgid, pids, order, statuses, st.PipeFail)
			fmt.Fprintf(stdio.Err, "\n[%d]  Stopped    %s\n", job.ID, job.Cmdline)
			return 128 + int(ws.StopSignal()), nil
		}
		delete(left, pid)
		statuses[order[pid]] = waitStatusCode(ws)
	}
	return pipelineStatus(statuses, st.PipeFail), nil
}

// orderedLeft returns the not-yet-exited pids in pipeline order.
func orderedLeft(order map[int]int, left map[int]bool) []int {
	pids := make([]int, 0, len(left))
	for pid := range left {
		pids = append(pids, pid)
	}
	for i := 0; i < len(pids); i++ {
		for k := i + 1; k < len(pids); k++ {
			if order[pids[k]] < order[pids[i]] {
				pids[i], pids[k] = pids[k], pids[i]
			}
		}
	}
	return pids
}

// stateOf snapshots a job's state under the table lock.
func (t *JobTable) stateOf(j *Job) (JobState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return j.state, j.adopted
}

func (t *JobTable) jobPgid(j *Job) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return j.pgid
}

// waitAdopted collects an adopted job's remaining processes (WUNTRACED).
// Returns stopped=true if the group was suspended again.
func (t *JobTable) waitAdopted(j *Job) (int, bool) {
	for {
		t.mu.Lock()
		left, pgid := len(j.pids), j.pgid
		t.mu.Unlock()
		if left == 0 {
			break
		}
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-pgid, &ws, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			break // ECHILD: nothing left to reap
		}
		if ws.Stopped() {
			return 0, true
		}
		t.mu.Lock()
		for i, p := range j.pids {
			if p == pid {
				j.pids = append(j.pids[:i], j.pids[i+1:]...)
				j.statuses[j.order[pid]] = waitStatusCode(ws)
				break
			}
		}
		t.mu.Unlock()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return pipelineStatus(j.statuses, j.pipefail), false
}

// ResumeForeground continues a stopped adopted job with the terminal
// (when one is available) and waits for it. stopped=true means it was
// suspended again; the job stays in the table, already re-marked.
func (t *JobTable) ResumeForeground(j *Job, tty *os.File) (int, bool) {
	pgid := t.jobPgid(j)
	if tty != nil {
		shellPgid := syscall.Getpgrp()
		defer func() { _ = tcSetpgrp(tty, shellPgid) }()
		_ = tcSetpgrp(tty, pgid)
	}
	t.markRunning(j)
	_ = syscall.Kill(-pgid, syscall.SIGCONT)
	status, stopped := t.waitAdopted(j)
	if stopped {
		t.markStopped(j, true) // caller prints the message
		return 0, true
	}
	return status, false
}

// ResumeBackground continues a stopped adopted job without the terminal;
// a watcher goroutine collects it, or re-marks it Stopped if it suspends
// again (e.g. SIGTTIN from reading the tty) for the REPL to announce.
func (t *JobTable) ResumeBackground(j *Job) {
	t.markRunning(j)
	_ = syscall.Kill(-t.jobPgid(j), syscall.SIGCONT)
	go func() {
		status, stopped := t.waitAdopted(j)
		if stopped {
			t.markStopped(j, false)
			return
		}
		t.finish(j, status)
	}()
}

// StopForFg forces a running adopted job into the Stopped state so fg can
// take over the wait from the background watcher. Best effort: returns
// false if the stop was not observed in time.
func (t *JobTable) StopForFg(j *Job) bool {
	_ = syscall.Kill(-t.jobPgid(j), syscall.SIGSTOP)
	for range 100 {
		if state, _ := t.stateOf(j); state == JobStopped {
			t.mu.Lock()
			j.stopNotified = true // fg resumes immediately; do not announce
			t.mu.Unlock()
			return true
		}
		if state, _ := t.stateOf(j); state == JobDone {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
