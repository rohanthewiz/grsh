package tour

// The parent's half of the worker protocol. See worker.go for the shape
// of the wire and the reasons there is one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rohanthewiz/grsh/internal/tutor"
)

// How long the parent waits for the child at each stage. These are
// generous rather than tuned: the only thing a timeout here protects is
// an HTTP handler that would otherwise never answer, and a tour that gave
// up on a busy machine would be a worse bug than a slow page.
const (
	startTimeout   = 30 * time.Second // fork, exec, build a playground
	signalTimeout  = 5 * time.Second  // deliver an interrupt and hear back
	closeTimeout   = 5 * time.Second  // tear the chapter down politely
	killRetry      = 100 * time.Millisecond
	requestTimeout = 30 * time.Second // everything that is not a Submit
)

// engine is what a visitor drives: one student's curriculum, wherever it
// happens to be running.
//
// Two implementations, and the difference between them is the whole point
// of this file. localEngine is a tutor.Driver in this process — simple,
// and shares its cwd and environment with every other visitor through
// tutor's eval gate. remoteEngine is a driver in a process of its own,
// which is what actual isolation costs.
//
// Submit takes all of a paste's lines at once rather than one at a time
// because each call is a round trip; the driver still sees them one by
// one, which is what it expects.
//
// Kill and Close carry an extra requirement the others do not: both must
// be safe to call from another goroutine while a Submit is in flight,
// because that is the only moment either is worth anything. See
// visitor.stop.
type engine interface {
	Submit(lines []string) tutor.View
	View() tutor.View
	Classify(src string) string
	Interrupt() bool
	Kill() bool
	Dir() string
	Close()
}

// localEngine is the in-process driver, kept for hosts that cannot fork
// themselves — a test that wants no subprocess, and any embedder that has
// not wired up IsWorker. Everything it does, it does under tutor's eval
// gate, so one student's long command stops every other student.
//
// The lock is what lets Close be called from another goroutine, which the
// interface requires. It cannot make Close prompt the way the worker's
// is: tutor.Driver is not safe for concurrent use, so a teardown here
// genuinely has to wait for the command in progress. Kill is the way out
// of that, and it is the same way out the terminal tutor has.
type localEngine struct {
	mu sync.Mutex
	d  *tutor.Driver
}

func newLocalEngine(out io.Writer, store tutor.Store) (*localEngine, error) {
	d, err := tutor.NewDriver(out, tutor.ResumeChapter(store), tutor.DriverOptions{
		Color:    true,
		Width:    transcriptWidth,
		Panels:   io.Discard,
		Store:    store,
		Embedded: true,
	})
	if err != nil {
		return nil, err
	}
	return &localEngine{d: d}, nil
}

func (l *localEngine) Submit(lines []string) tutor.View {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range lines {
		l.d.Submit(line)
	}
	return l.d.View()
}

func (l *localEngine) View() tutor.View {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.View()
}

func (l *localEngine) Classify(src string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Classify(src)
}

func (l *localEngine) Dir() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.Dir()
}

func (l *localEngine) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.d.Close()
}

// Interrupt and Kill take no lock, exactly as tutor.Driver's own do and
// for the same reason: a stop button that queued behind the command it is
// stopping would not be one.
func (l *localEngine) Interrupt() bool { return l.d.Interrupt() }
func (l *localEngine) Kill() bool      { return l.d.Kill() }

// remoteEngine is one visitor's worker process, seen from the server.
type remoteEngine struct {
	cmd   *exec.Cmd
	out   io.Writer   // the visitor's sink; every out frame lands here
	store tutor.Store // the real database, which only this process may touch

	reqs io.WriteCloser // fd 3
	ctl  io.WriteCloser // fd 5

	// mu guards the request pipe and the waiter table. The pipe needs it
	// because Interrupt writes to a different fd concurrently with a
	// Submit sitting in this one, and the table needs it because the
	// reader goroutine delivers into it.
	mu    sync.Mutex
	next  uint64
	waits map[uint64]chan frame
	dead  bool

	// dir is the last playground the child reported. The parent keeps it
	// for one case only: a child that died without cleaning up after
	// itself, where this is the only remaining way to find the directory.
	dir string

	exited chan struct{} // closed when the frame stream ends
}

// newRemoteEngine starts a worker and waits for it to have a playground.
//
// It re-executes this very binary with WorkerEnv set, which is why a host
// must dispatch on IsWorker before anything else. os.Executable rather
// than os.Args[0] so that a tour started through a relative path or a
// PATH lookup still finds itself after a `cd`, which the shell it is
// about to run does constantly.
func newRemoteEngine(out io.Writer, store tutor.Store) (_ *remoteEngine, err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot find this program to run a worker: %w", err)
	}

	// Three pipes, named for the direction they carry. Every one of them
	// is closed on the way out of a failed start; a leaked pipe pair here
	// would be a file descriptor per abandoned visitor.
	reqR, reqW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	frR, frW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		return nil, err
	}
	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		reqR.Close()
		reqW.Close()
		frR.Close()
		frW.Close()
		return nil, err
	}
	defer func() {
		// The child's ends belong to the child once it starts; ours stay
		// open. On any failure, everything goes.
		reqR.Close()
		frW.Close()
		ctlR.Close()
		if err != nil {
			reqW.Close()
			frR.Close()
			ctlW.Close()
		}
	}()

	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), WorkerEnv+"=1")
	// Standard streams are left as diagnostics, not protocol: a panic in
	// the child should reach the terminal that started the tour. stdin is
	// /dev/null because a worker has no business reading the operator's
	// keyboard — and the chapters it runs are careful about that too (see
	// the embedded stdin in tutor.newChapter).
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{reqR, frW, ctlR} // → fds 3, 4, 5
	// Its own process group, so that a Ctrl+C in the terminal running the
	// tour reaches the tour and not, simultaneously, every student's shell
	// — which would kill the workers before they could remove the
	// playgrounds they own.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start a worker: %w", err)
	}

	r := &remoteEngine{
		cmd: cmd, out: out, store: store,
		reqs: reqW, ctl: ctlW,
		waits:  map[uint64]chan frame{},
		exited: make(chan struct{}),
	}
	go r.read(frR)

	view, err := r.hello(store)
	if err != nil {
		r.forceStop()
		return nil, err
	}
	r.dir = view.Dir
	return r, nil
}

// hello is the first request and the handshake: the child answers it only
// once it has a driver, which means a playground on disk. A failure here
// is reported to the browser as a tour that would not start, which is
// exactly what it is.
func (r *remoteEngine) hello(store tutor.Store) (tutor.View, error) {
	f, err := r.roundTrip(request{
		Op: opHello,
		Hello: &hello{
			Records: tutor.SnapshotProgress(store),
			Store:   store != nil,
			// Colour on: the page renders the escape codes as spans, and
			// the engine's palette is part of how a lesson reads.
			Color: true,
			Width: transcriptWidth,
		},
	}, startTimeout)
	if err != nil {
		return tutor.View{}, err
	}
	if f.View == nil {
		return tutor.View{}, errors.New("worker started without a lesson")
	}
	return *f.View, nil
}

// read is the only goroutine that touches the frame stream, and its being
// the only one is what orders the session.
//
// Transcript bytes are written to the sink here, synchronously, before
// the reply that follows them is handed to whoever is waiting. So a
// Submit's caller cannot see the View describing a finished command until
// every byte that command printed is already in the transcript — which is
// the ordering the page depends on to re-enable its input at the right
// moment.
func (r *remoteEngine) read(frames *os.File) {
	defer frames.Close()
	dec := json.NewDecoder(frames)
	for {
		var f frame
		if err := dec.Decode(&f); err != nil {
			break
		}
		switch f.T {
		case frOut:
			_, _ = r.out.Write(f.Out)
		case frSave:
			// Progress travels up because the database is a file this
			// process holds open; see worker.go. A failed save is not
			// worth a word to the student, who did not ask for one.
			if r.store != nil && f.Record != nil {
				_ = r.store.Save(*f.Record)
			}
		default:
			r.deliver(f)
		}
	}
	r.die()
}

// deliver hands a reply to its waiter, or drops it if nobody is left.
func (r *remoteEngine) deliver(f frame) {
	r.mu.Lock()
	ch, ok := r.waits[f.ID]
	delete(r.waits, f.ID)
	r.mu.Unlock()
	if ok {
		ch <- f // buffered: the waiter may already have timed out
	}
}

// die marks the worker gone and wakes everyone waiting on it.
//
// The student is told, in the transcript, because the alternative is a
// page that simply stops answering. A dead worker is recoverable — Reset
// builds a new one — and saying so is the difference between a bug report
// and a click.
func (r *remoteEngine) die() {
	r.mu.Lock()
	if r.dead {
		r.mu.Unlock()
		return
	}
	r.dead = true
	waits := r.waits
	r.waits = map[uint64]chan frame{}
	r.mu.Unlock()

	for id, ch := range waits {
		ch <- frame{ID: id, Err: "the worker process ended"}
	}
	// Said in the transcript, in the tutor's own warning colour, because
	// the page has no other way to distinguish "the shell died" from "the
	// shell is thinking". Reset builds a new worker, so this is a click
	// away from recovered rather than the end of the session.
	fmt.Fprint(r.out, "\r\n\x1b[33m[the session ended unexpectedly — press Reset to start again]\x1b[0m\r\n")
	close(r.exited)
	_ = r.cmd.Wait() // reap it; the pipes are already closed
	r.cleanupDir()
}

// roundTrip sends one request and waits for its reply.
//
// The timeout is not a limit on how long a student's command may run —
// Submit passes none, because `sleep 30` is a perfectly good thing to
// type in a shell tutorial and the browser is entitled to wait for it.
// It is there for the requests that should never be slow, so that a
// wedged worker costs one handler rather than all of them.
func (r *remoteEngine) roundTrip(req request, timeout time.Duration) (frame, error) {
	ch := make(chan frame, 1)
	r.mu.Lock()
	if r.dead {
		r.mu.Unlock()
		return frame{}, errors.New("the worker process ended")
	}
	r.next++
	req.ID = r.next
	r.waits[req.ID] = ch
	b, err := json.Marshal(req)
	if err == nil {
		_, err = r.reqs.Write(append(b, '\n'))
	}
	r.mu.Unlock()
	if err != nil {
		r.mu.Lock()
		delete(r.waits, req.ID)
		r.mu.Unlock()
		return frame{}, err
	}

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case f := <-ch:
		if f.Err != "" {
			return f, errors.New(f.Err)
		}
		return f, nil
	case <-timer:
		r.mu.Lock()
		delete(r.waits, req.ID)
		r.mu.Unlock()
		return frame{}, fmt.Errorf("worker did not answer %s within %s", req.Op, timeout)
	}
}

// signal sends an interrupt down the control fd, which is not the fd a
// running command is blocking. The reply comes back on the ordinary frame
// stream, so this still learns whether there was anything to signal.
func (r *remoteEngine) signal(op string) bool {
	ch := make(chan frame, 1)
	r.mu.Lock()
	if r.dead {
		r.mu.Unlock()
		return false
	}
	r.next++
	id := r.next
	r.waits[id] = ch
	r.mu.Unlock()

	b, err := json.Marshal(request{ID: id, Op: op})
	if err == nil {
		_, err = r.ctl.Write(append(b, '\n'))
	}
	if err != nil {
		r.mu.Lock()
		delete(r.waits, id)
		r.mu.Unlock()
		return false
	}
	t := time.NewTimer(signalTimeout)
	defer t.Stop()
	select {
	case f := <-ch:
		return f.Signalled
	case <-t.C:
		r.mu.Lock()
		delete(r.waits, id)
		r.mu.Unlock()
		return false
	}
}

func (r *remoteEngine) Interrupt() bool { return r.signal(opInt) }
func (r *remoteEngine) Kill() bool      { return r.signal(opKill) }

// Submit has no timeout on purpose: it is a student running a command,
// and how long that takes is up to them.
func (r *remoteEngine) Submit(lines []string) tutor.View {
	return r.viewOf(request{Op: opSubmit, Lines: lines}, 0)
}

func (r *remoteEngine) View() tutor.View {
	return r.viewOf(request{Op: opView}, requestTimeout)
}

// viewOf answers with the child's View, or with an ended one if the child
// can no longer speak. Ended is the honest answer: the page greys out its
// input and offers Reset, which is precisely what a student whose worker
// has gone should be offered.
func (r *remoteEngine) viewOf(req request, timeout time.Duration) tutor.View {
	f, err := r.roundTrip(req, timeout)
	if err != nil || f.View == nil {
		return tutor.View{Ended: true, Code: 1, Dir: r.lastDir()}
	}
	r.mu.Lock()
	r.dir = f.View.Dir
	r.mu.Unlock()
	return *f.View
}

// Classify is called per keystroke, so it fails quietly: an empty verdict
// blanks the hint lane, which is the right thing to show when the answer
// is not available.
func (r *remoteEngine) Classify(src string) string {
	f, err := r.roundTrip(request{Op: opClassify, Src: src}, requestTimeout)
	if err != nil {
		return ""
	}
	return f.Verdict
}

func (r *remoteEngine) Dir() string { return r.lastDir() }

func (r *remoteEngine) lastDir() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dir
}

// Close ends the worker, in three steps that each exist for a reason.
//
// First a kill down the control fd: the child cannot read the close
// request while it is inside a Submit, and a student who left `sleep 300`
// running must not hold up the server's shutdown — which is exactly what
// the in-process version did, since closing took the lock the command was
// holding.
//
// Then the close request itself, which is what makes the child remove its
// own playground and exit. Only if that does not happen does the parent
// reach for the process, and then it has to remove the directory itself.
func (r *remoteEngine) Close() {
	select {
	case <-r.exited:
		return // already gone; die() did the cleanup
	default:
	}

	// The close request goes on the request fd, which a worker inside a
	// command is not reading — so it is sent from a goroutine and the
	// killing happens alongside it rather than once, before.
	//
	// Once is not enough, and that is the subtle part. `echo hi; sleep 30`
	// is two pipelines, and a signal that lands in the gap between them
	// finds no foreground pipeline, reports honestly that it signalled
	// nothing, and leaves the sleep to start a millisecond later. Retrying
	// until the worker answers costs a few signals nobody feels and closes
	// that window.
	done := make(chan error, 1)
	go func() {
		_, err := r.roundTrip(request{Op: opClose}, closeTimeout)
		done <- err
	}()
	tick := time.NewTicker(killRetry)
	defer tick.Stop()
	for {
		r.signal(opKill)
		select {
		case err := <-done:
			if err == nil {
				// It answered, so it is on its way out. Wait for the frame
				// stream to end, which is the child actually exiting.
				select {
				case <-r.exited:
					return
				case <-time.After(closeTimeout):
				}
			}
			r.forceStop()
			return
		case <-tick.C:
		}
	}
}

// forceStop is the end of the line: kill the worker and wait for the
// reader goroutine to notice, which is what runs the cleanup.
func (r *remoteEngine) forceStop() {
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	r.reqs.Close()
	r.ctl.Close()
	select {
	case <-r.exited:
	case <-time.After(closeTimeout):
		// The reader is wedged on a pipe that will not close. Nothing left
		// to wait for; die's cleanup is best-effort from here.
		r.die()
	}
}

// cleanupDir removes a playground the worker did not.
//
// It only ever runs after the child is gone, and only for a directory
// that still looks like one of ours: this is an os.RemoveAll built from a
// path that arrived over a pipe, so it checks the shape of the name
// rather than trusting it. A worker that shut down properly has already
// removed the directory and this finds nothing.
func (r *remoteEngine) cleanupDir() {
	dir := r.lastDir()
	if dir == "" || !strings.HasPrefix(filepath.Base(dir), "grsh-tutor-") {
		return
	}
	_ = os.RemoveAll(dir)
}
