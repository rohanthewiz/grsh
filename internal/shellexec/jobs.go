package shellexec

// Background jobs. A job is one and-or chain launched with a trailing &.
// Every word, alias, and redirect target is expanded synchronously at
// launch time (a deliberate deviation from bash's lazy subshell — grsh's
// interpreter is single-threaded), then the pipelines start, wait, and
// short-circuit in a goroutine that touches no shell or interpreter
// state: only the job table, under its lock. Each pipeline runs in its
// own process group, so terminal Ctrl+C never reaches background jobs,
// and stdin comes from /dev/null so they cannot steal interactive input.

import (
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

type JobState int

const (
	JobRunning JobState = iota
	JobStopped
	JobDone
)

// Job is one background and-or chain. All mutable fields are guarded by
// the owning JobTable's lock; Done is closed exactly once on completion.
// pgidSet closes when the process group exists (or the job finishes
// without ever starting a process) so Signal never races the launch.
type Job struct {
	ID      int
	Cmdline string
	Done    chan struct{}

	pgid      int
	pgidSet   chan struct{}
	readyOnce sync.Once
	state     JobState
	status    int
	notified  bool

	// Adopted jobs (a foreground pipeline suspended with ^Z). Background
	// (&) jobs are owned by their launch goroutine; adopted jobs are
	// waited on by whoever resumes them (fg inline, bg via a watcher).
	adopted      bool
	pids         []int       // not-yet-exited processes
	order        map[int]int // pid → pipeline position
	statuses     []int       // per-position statuses (filled as pids exit)
	pipefail     bool        // captured at suspension
	stopNotified bool        // "[N] Stopped" already shown
}

// JobInfo is a lock-free snapshot for builtins and notifications.
type JobInfo struct {
	ID      int
	Cmdline string
	State   JobState
	Status  int
	Pgid    int
}

type JobTable struct {
	mu   sync.Mutex
	jobs []*Job
}

func NewJobTable() *JobTable { return &JobTable{} }

// Add registers a new job. IDs grow while any job is tracked and reset to
// 1 when the table drains, like bash.
func (t *JobTable) Add(cmdline string) *Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := 1
	for _, j := range t.jobs {
		if j.ID >= id {
			id = j.ID + 1
		}
	}
	j := &Job{ID: id, Cmdline: cmdline, Done: make(chan struct{}), pgidSet: make(chan struct{})}
	t.jobs = append(t.jobs, j)
	return j
}

func (t *JobTable) setPgid(j *Job, pgid int) {
	t.mu.Lock()
	j.pgid = pgid
	t.mu.Unlock()
	j.readyOnce.Do(func() { close(j.pgidSet) })
}

func (t *JobTable) finish(j *Job, status int) {
	t.mu.Lock()
	j.state = JobDone
	j.status = status
	t.mu.Unlock()
	j.readyOnce.Do(func() { close(j.pgidSet) })
	close(j.Done)
}

// Wait blocks until the job completes and returns its status.
func (t *JobTable) Wait(j *Job) int {
	<-j.Done
	t.mu.Lock()
	defer t.mu.Unlock()
	return j.status
}

// Snapshot returns the current jobs oldest-first.
func (t *JobTable) Snapshot() []JobInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]JobInfo, 0, len(t.jobs))
	for _, j := range t.jobs {
		out = append(out, JobInfo{ID: j.ID, Cmdline: j.Cmdline, State: j.state, Status: j.status, Pgid: j.pgid})
	}
	return out
}

// Running returns the jobs not yet finished, oldest-first.
func (t *JobTable) Running() []*Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*Job
	for _, j := range t.jobs {
		if j.state != JobDone {
			out = append(out, j)
		}
	}
	return out
}

// All returns every tracked job, oldest-first.
func (t *JobTable) All() []*Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*Job(nil), t.jobs...)
}

// Reap removes one collected job from the table without emitting a
// notification (wait/fg already reported its status to the caller).
func (t *JobTable) Reap(j *Job) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, cur := range t.jobs {
		if cur == j {
			t.jobs = append(t.jobs[:i], t.jobs[i+1:]...)
			return
		}
	}
}

// Find resolves a job spec: "%N" by id; "%%" or "%+" the newest job.
func (t *JobTable) Find(spec string) *Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.jobs) == 0 {
		return nil
	}
	switch spec {
	case "", "%%", "%+":
		return t.jobs[len(t.jobs)-1]
	}
	rest, ok := strings.CutPrefix(spec, "%")
	if !ok {
		return nil
	}
	for _, j := range t.jobs {
		if fmt.Sprint(j.ID) == rest {
			return j
		}
	}
	return nil
}

// Signal delivers sig to the job's whole process group, waiting out the
// brief window between launch and the first process start.
func (t *JobTable) Signal(j *Job, sig syscall.Signal) error {
	<-j.pgidSet
	t.mu.Lock()
	pgid, state := j.pgid, j.state
	t.mu.Unlock()
	if state == JobDone {
		return serr.New("job has terminated")
	}
	if pgid == 0 {
		return serr.New("job has no process group")
	}
	return syscall.Kill(-pgid, sig)
}

// AdoptStopped registers a suspended foreground pipeline as a Stopped
// job. The suspension message is printed by the suspender, so it is born
// already stop-notified.
func (t *JobTable) AdoptStopped(cmdline string, pgid int, pids []int, order map[int]int, statuses []int, pipefail bool) *Job {
	j := t.Add(cmdline)
	t.mu.Lock()
	j.pgid = pgid
	j.state = JobStopped
	j.adopted = true
	j.pids = pids
	j.order = order
	j.statuses = statuses
	j.pipefail = pipefail
	j.stopNotified = true
	t.mu.Unlock()
	j.readyOnce.Do(func() { close(j.pgidSet) })
	return j
}

func (t *JobTable) markRunning(j *Job) {
	t.mu.Lock()
	j.state = JobRunning
	t.mu.Unlock()
}

// markStopped flags a suspended job. notified=false queues a REPL
// announcement (a bg-resumed job hitting SIGTTIN); notified=true means
// the suspender already printed it (fg's own ^Z).
func (t *JobTable) markStopped(j *Job, notified bool) {
	t.mu.Lock()
	j.state = JobStopped
	j.stopNotified = notified
	t.mu.Unlock()
}

// Notifications drains completion messages for finished jobs (removing
// them) and stop announcements for newly stopped ones (which stay).
func (t *JobTable) Notifications() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var notes []string
	for _, j := range t.jobs {
		switch {
		case j.state == JobDone && !j.notified:
			j.notified = true
			notes = append(notes, doneLine(j.ID, j.status, j.Cmdline))
		case j.state == JobStopped && !j.stopNotified:
			j.stopNotified = true
			notes = append(notes, fmt.Sprintf("[%d]  Stopped    %s", j.ID, j.Cmdline))
		}
	}
	t.removeNotified()
	return notes
}

// reap removes finished jobs that have already been reported (after the
// jobs builtin lists them). Callers hold the lock via public methods.
func (t *JobTable) removeNotified() {
	kept := t.jobs[:0]
	for _, j := range t.jobs {
		if j.state == JobDone && j.notified {
			continue
		}
		kept = append(kept, j)
	}
	t.jobs = kept
}

// jobSafeWriter passes *os.File writers through (the kernel serializes
// those) and replaces anything else with /dev/null so a background
// goroutine never writes into a same-process buffer concurrently.
func jobSafeWriter(w io.Writer, closers *[]io.Closer) io.Writer {
	if w == nil {
		return nil
	}
	if f, ok := w.(*os.File); ok {
		return f
	}
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil
	}
	*closers = append(*closers, f)
	return f
}

func doneLine(id, status int, cmdline string) string {
	if status == 0 {
		return fmt.Sprintf("[%d]  Done       %s", id, cmdline)
	}
	return fmt.Sprintf("[%d]  Exit %-5d %s", id, status, cmdline)
}

// preparedCmd is a fully expanded command, ready to start asynchronously.
type preparedCmd struct {
	argv   []string
	redirs []resolvedRedir
}

// launchJob expands the whole chain, registers the job, and starts the
// async runner. Expansion errors report bash-style (message + status 1)
// without creating a job.
func launchJob(st *State, ao *shellparse.AndOr, ev WordEvaluator, stdio Stdio) (int, error) {
	pipes := make([][]preparedCmd, 0, len(ao.Pipes))
	var names []string
	for _, pl := range ao.Pipes {
		prepared := make([]preparedCmd, 0, len(pl.Cmds))
		for _, cmd := range pl.Cmds {
			argv, err := ExpandWords(st, cmd.Words, ev)
			if err != nil {
				return userErr(stdio, err)
			}
			argv = expandAlias(st, argv)
			if len(argv) > 1 && argv[0] == "command" {
				argv = argv[1:]
			}
			if len(argv) == 0 {
				return userErr(stdio, serr.New("empty command in background job"))
			}
			if isBuiltin(argv[0]) {
				return userErr(stdio, serr.New("builtin '"+argv[0]+"' cannot run in the background"))
			}
			redirs, err := resolveRedirs(st, cmd.Redirs, ev)
			if err != nil {
				return userErr(stdio, err)
			}
			prepared = append(prepared, preparedCmd{argv: argv, redirs: redirs})
			names = append(names, strings.Join(argv, " "))
		}
		pipes = append(pipes, prepared)
	}

	job := st.Jobs.Add(strings.Join(names, " | ") + " &")
	// PipeFail is captured at launch, like the rest of the expansion.
	go runJob(st.Jobs, job, pipes, ao.Ops, stdio, st.PipeFail)
	return 0, nil
}

// runJob executes the prepared chain in the background: && / || logic on
// local statuses only, final status recorded in the job table.
func runJob(jt *JobTable, job *Job, pipes [][]preparedCmd, ops []shellparse.LogicOp, stdio Stdio, pipefail bool) {
	// Only real files (terminal, redirected file) are safe for concurrent
	// writes; in capture/buffer contexts the job's output is discarded —
	// use explicit redirection to keep it.
	var sanitized []io.Closer
	stdio.Out = jobSafeWriter(stdio.Out, &sanitized)
	stdio.Err = jobSafeWriter(stdio.Err, &sanitized)
	defer closeAll(sanitized)

	status := 0
	for i, pl := range pipes {
		if i > 0 {
			if ops[i-1] == shellparse.AndOp && status != 0 {
				continue
			}
			if ops[i-1] == shellparse.OrOp && status == 0 {
				continue
			}
		}
		status = runPreparedPipeline(jt, job, pl, stdio, pipefail)
	}
	jt.finish(job, status)
}

// runPreparedPipeline mirrors runPipes for pre-expanded commands, adding
// job-control process semantics: the pipeline runs in its own process
// group and reads stdin from /dev/null.
func runPreparedPipeline(jt *JobTable, job *Job, cmds []preparedCmd, stdio Stdio, pipefail bool) int {
	n := len(cmds)
	execs := make([]*exec.Cmd, n)
	statuses := make([]int, n)
	var parentFiles []io.Closer
	var allClosers []io.Closer
	defer func() { closeAll(allClosers) }()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		fmt.Fprintf(stdio.Err, "grsh: %v\n", err)
		return 126
	}
	allClosers = append(allClosers, devnull)

	for i, pc := range cmds {
		execs[i] = exec.Command(pc.argv[0], pc.argv[1:]...)
		execs[i].Stderr = stdio.Err
	}
	execs[0].Stdin = devnull
	execs[n-1].Stdout = stdio.Out
	for i := 0; i < n-1; i++ {
		pr, pw, err := os.Pipe()
		if err != nil {
			closeAll(parentFiles)
			fmt.Fprintf(stdio.Err, "grsh: %v\n", err)
			return 126
		}
		execs[i].Stdout = pw
		execs[i+1].Stdin = pr
		parentFiles = append(parentFiles, pr, pw)
	}

	for i, pc := range cmds {
		if len(pc.redirs) == 0 {
			continue
		}
		base := Stdio{In: execs[i].Stdin, Out: execs[i].Stdout, Err: execs[i].Stderr}
		sio, closers, err := applyResolved(pc.redirs, base)
		allClosers = append(allClosers, closers...)
		if err != nil {
			closeAll(parentFiles)
			fmt.Fprintf(stdio.Err, "grsh: %s\n", serr.StringFromErr(err))
			return 1
		}
		execs[i].Stdin, execs[i].Stdout, execs[i].Stderr = sio.In, sio.Out, sio.Err
	}

	started := make([]bool, n)
	pgid := 0
	for i, c := range execs {
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
		if err := c.Start(); err != nil {
			statuses[i], _ = externalStatus(stdio, cmds[i].argv[0], err)
			continue
		}
		started[i] = true
		if pgid == 0 {
			pgid = c.Process.Pid // leader; the rest join its group
			jt.setPgid(job, pgid)
		}
	}
	closeAll(parentFiles)

	for i, c := range execs {
		if !started[i] {
			continue
		}
		if err := c.Wait(); err != nil {
			statuses[i], _ = externalStatus(stdio, cmds[i].argv[0], err)
		}
	}
	return pipelineStatus(statuses, pipefail)
}
