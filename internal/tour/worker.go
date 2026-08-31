package tour

// One student, one process.
//
// # Why
//
// A grsh session's working directory IS the process's working directory:
// `cd` calls os.Chdir, deliberately (see internal/shellexec). `export` is
// the same story with os.Setenv. So two students sharing one process
// cannot both be standing in their own playground at the same time, and
// tutor's evalGate makes that safe the only way it can from inside one
// process — by letting exactly one of them run at a time and restoring
// the other's cwd and environment around it.
//
// That is correct and it is slow in the one way that matters: a student
// who runs `sleep 30` blocks every other student's next command for
// thirty seconds. No amount of care inside one process fixes it, because
// the thing being shared is the process. So each visitor gets one.
//
//	tour.Server (parent)                       worker (child, one per visitor)
//	────────────────────────                   ──────────────────────────────
//	remoteEngine.Submit ── fd 3 (requests) ──▶ RunWorker loop ─▶ Driver.Submit
//	          sink ◀── fd 4 (frames) ──────── transcript writer ◀─ session out
//	remoteEngine.Interrupt ─ fd 5 (control) ─▶ signal goroutine ─▶ Driver.Interrupt
//
// The cwd and the environment are now genuinely private, evalGate is
// uncontended (one driver per process), and a wedged student wedges only
// their own tab. What it costs is a process per open browser tab and a
// protocol, which is the subject of the rest of this file.
//
// # Three pipes, not one
//
// Requests and frames are separate fds rather than stdin/stdout because
// the child is a whole shell: anything it or a dependency prints to fd 1
// or 2 — a stray log line, a panic — would otherwise land in the middle
// of a JSON stream that has no way to resynchronise. On dedicated fds a
// stray print goes where prints go, the tour's own terminal, and the
// protocol is unharmed.
//
// Control is a third fd because it must not queue. Interrupt's entire
// reason to exist is the moment when the child is inside a Submit that
// will not return, which is exactly when the request pipe is not being
// read. tutor.Driver.Interrupt is documented safe from another goroutine;
// the control fd is how that goroutine hears about it.
//
// # Progress
//
// The store (~/.grsh_tutor.db) is a file held open by one process, and
// that process is the parent. The child gets a snapshot of the student's
// records at startup and serves its own Loads from it, and its Saves ride
// back as fire-and-forget frames the parent applies to the real store.
//
// The trade is that two tabs open at once no longer see each other's
// progress land, only their own — where before they shared a live store.
// For a local single-user tool that is a fair price, and arguably the
// better behaviour: two tabs on two chapters no longer overwrite each
// other's place.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/rohanthewiz/grsh/internal/tutor"
)

// WorkerEnv marks a process as a visitor's worker. It is an environment
// variable rather than a flag because the same mechanism has to work for
// two very different executables: the real `grsh-tour` binary, and the
// test binary that `go test ./internal/tour` builds — which would reject
// an unknown flag but reads the environment happily. Both dispatch on
// IsWorker before doing anything else.
const WorkerEnv = "GRSH_TOUR_WORKER"

// The child's inherited descriptors. 0/1/2 are left alone on purpose —
// see the file comment.
const (
	fdRequests = 3 // parent → child: one JSON request per value
	fdFrames   = 4 // child → parent: one JSON frame per value
	fdControl  = 5 // parent → child: interrupts, out of band
)

// IsWorker reports whether this process was started to be one visitor's
// shell. A host that embeds this package MUST check it before its own
// startup and hand off to RunWorker; a host that does not will spawn
// copies of itself that try to serve HTTP.
func IsWorker() bool { return os.Getenv(WorkerEnv) == "1" }

// RunWorkerProcess is RunWorker on the descriptors a worker process is
// started with. It is what a host calls after IsWorker says yes.
//
// RunWorker itself takes plain readers and writers instead, so that a
// test can drive a worker over three ordinary pipes without forking
// anything and watch exactly what it says.
func RunWorkerProcess() int {
	// Close-on-exec, and this is not hygiene — it is a correctness fix
	// this cost an afternoon to find.
	//
	// A worker's three descriptors arrive through exec.Cmd.ExtraFiles,
	// which deliberately does NOT set close-on-exec: they have to survive
	// the exec that creates this process. But a worker's whole job is to
	// fork the student's commands, and every one of them would inherit
	// them too. So `sleep 300` ends up holding the write end of the frame
	// pipe, and the parent — which learns that its worker has died by
	// reading EOF from that pipe — learns nothing at all until the
	// student's own command finishes.
	//
	// Set here rather than in the parent because the parent's ends are a
	// different set of descriptors; these are ours, and the very next
	// exec is the student's.
	for _, fd := range []int{fdRequests, fdFrames, fdControl} {
		syscall.CloseOnExec(fd)
	}
	return RunWorker(
		os.NewFile(fdRequests, "requests"),
		os.NewFile(fdFrames, "frames"),
		os.NewFile(fdControl, "control"),
	)
}

// ── the wire ─────────────────────────────────────────────────────

// request is one thing the parent wants done, in the order the child
// reads them. Every request is answered by exactly one reply frame
// carrying the same ID, including "close".
type request struct {
	ID    uint64   `json:"id"`
	Op    string   `json:"op"`
	Lines []string `json:"lines,omitempty"` // submit
	Src   string   `json:"src,omitempty"`   // classify
	Hello *hello   `json:"hello,omitempty"` // hello
}

// Ops. Kept as strings so a protocol mismatch between two builds is a
// readable error rather than a silently different number.
const (
	opHello    = "hello"    // build the driver; must be first
	opSubmit   = "submit"   // feed physical lines, answer with the View
	opView     = "view"     // the View, unchanged
	opClassify = "classify" // the classifier's verdict for a draft line
	opClose    = "close"    // tear down the playground and exit
	opInt      = "int"      // control fd only: SIGINT the foreground pipeline
	opKill     = "kill"     // control fd only: SIGKILL it
)

// hello carries what the child cannot work out for itself: where the
// student had got to, and whether progress is being kept at all.
type hello struct {
	Records []tutor.Record `json:"records"`
	Store   bool           `json:"store"`
	Color   bool           `json:"color"`
	Width   int            `json:"width"`
}

// frame is everything the child says. Out frames are interleaved with
// replies on one fd, in the order they were produced, and that ordering
// is load-bearing: it is what guarantees a Submit's transcript reaches
// the browser before the View that describes the state it left behind.
type frame struct {
	T   string `json:"t"`
	ID  uint64 `json:"id,omitempty"`
	Err string `json:"err,omitempty"`

	// Out is transcript bytes. []byte rather than string because it is
	// base64 on the wire, and a shell's output is not guaranteed to be
	// valid UTF-8 — a JSON string would silently replace every bad byte
	// with U+FFFD, which is a lie about what the student's command wrote.
	Out []byte `json:"out,omitempty"`

	View      *tutor.View   `json:"view,omitempty"`
	Verdict   string        `json:"verdict,omitempty"`
	Signalled bool          `json:"signalled,omitempty"`
	Record    *tutor.Record `json:"record,omitempty"` // a save, going upstream
}

// Frame types.
const (
	frOut   = "out"   // transcript bytes
	frReply = "reply" // the answer to one request, by ID
	frSave  = "save"  // progress the parent should persist; no reply
)

// ── the child ────────────────────────────────────────────────────

// RunWorker is the whole of a worker process: build one driver, serve
// requests until told to close, and exit. It returns the process's exit
// code.
//
// The fds are parameters rather than being opened here so a test can
// drive a worker in-process over three pipes and watch what it says.
func RunWorker(requests io.Reader, frames io.Writer, control io.Reader) int {
	fw := &frameWriter{w: frames}
	dec := json.NewDecoder(requests)

	var first request
	if err := dec.Decode(&first); err != nil {
		fmt.Fprintf(os.Stderr, "grsh-tour worker: no hello: %v\n", err)
		return 1
	}
	if first.Op != opHello {
		fw.send(frame{T: frReply, ID: first.ID, Err: "first request must be " + opHello})
		return 1
	}
	h := first.Hello
	if h == nil {
		fw.send(frame{T: frReply, ID: first.ID, Err: opHello + " carried no options"})
		return 1
	}

	var store tutor.Store
	if h.Store {
		store = &proxyStore{recs: indexRecords(h.Records), fw: fw}
	}
	d, err := tutor.NewDriver(fw.transcript(), tutor.ResumeChapter(store), tutor.DriverOptions{
		Color:  h.Color,
		Width:  h.Width,
		Panels: io.Discard,
		Store:  store,
		// No terminal anywhere near this process: foreground pipelines
		// need their own process group for Interrupt to have something to
		// signal, and children must never reach for a tty.
		Embedded: true,
	})
	if err != nil {
		fw.send(frame{T: frReply, ID: first.ID, Err: err.Error()})
		return 1
	}
	view := d.View()
	fw.send(frame{T: frReply, ID: first.ID, View: &view})

	// The control reader is a goroutine because its whole purpose is to
	// be heard while the loop below is not listening. It touches only
	// Interrupt and Kill, the two calls tutor.Driver documents as safe
	// from another goroutine.
	go serveControl(control, d, fw)

	for {
		var r request
		if err := dec.Decode(&r); err != nil {
			// The parent went away — a crash, or a kill that beat the
			// close op. Tear the playground down anyway; nothing else
			// will, and it is this process's own $TMPDIR directory.
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "grsh-tour worker: %v\n", err)
			}
			d.Close()
			return 0
		}
		switch r.Op {
		case opSubmit:
			for _, line := range r.Lines {
				d.Submit(line)
			}
			view := d.View()
			fw.send(frame{T: frReply, ID: r.ID, View: &view})
		case opView:
			view := d.View()
			fw.send(frame{T: frReply, ID: r.ID, View: &view})
		case opClassify:
			fw.send(frame{T: frReply, ID: r.ID, Verdict: d.Classify(r.Src)})
		case opClose:
			d.Close()
			fw.send(frame{T: frReply, ID: r.ID})
			return 0
		default:
			fw.send(frame{T: frReply, ID: r.ID, Err: "unknown op " + r.Op})
		}
	}
}

// serveControl answers the out-of-band fd. It never touches the driver's
// state, only signals the pipeline the driver is currently running, so it
// is free to run while the request loop is blocked inside Submit — which
// is the only time either of these commands means anything.
func serveControl(control io.Reader, d *tutor.Driver, fw *frameWriter) {
	dec := json.NewDecoder(control)
	for {
		var r request
		if err := dec.Decode(&r); err != nil {
			return // the parent closed it; we are on our way out anyway
		}
		var ok bool
		switch r.Op {
		case opInt:
			ok = d.Interrupt()
		case opKill:
			ok = d.Kill()
		}
		fw.send(frame{T: frReply, ID: r.ID, Signalled: ok})
	}
}

// frameWriter serializes everything the child says onto one fd.
//
// The lock is not optional: os/exec's copier goroutines write the
// student's output while the request loop writes replies, so without it
// two frames would interleave into one unparseable line. One Encode is
// one write, so holding it is brief even for a large chunk of output.
type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (f *frameWriter) send(fr frame) {
	b, err := json.Marshal(fr)
	if err != nil {
		return // an unencodable frame is a bug, not a session-ending event
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.w.Write(append(b, '\n'))
}

// transcript is the io.Writer the driver renders into. Every write
// becomes one frame, so the browser sees output at the granularity the
// shell produced it rather than in whatever chunks a buffer chose.
func (f *frameWriter) transcript() io.Writer { return transcriptWriter{f} }

type transcriptWriter struct{ f *frameWriter }

func (t transcriptWriter) Write(p []byte) (int, error) {
	// The frame owns its bytes: Write's contract lets the caller reuse p
	// the moment this returns, and json.Marshal runs after that promise
	// would have been broken without a copy.
	t.f.send(frame{T: frOut, Out: append([]byte(nil), p...)})
	return len(p), nil
}

// proxyStore is the child's half of the progress database.
//
// Loads are served locally from the snapshot the parent sent, kept
// current by every Save this student makes — which is sound because this
// process is the only writer for this student. Saves are posted upstream
// and not waited on: a progress write is a convenience, and making the
// student's next prompt wait for a round trip and a disk write to earn it
// would be a bad trade.
type proxyStore struct {
	mu   sync.Mutex
	recs map[string]tutor.Record
	fw   *frameWriter
}

func indexRecords(rs []tutor.Record) map[string]tutor.Record {
	m := make(map[string]tutor.Record, len(rs))
	for _, r := range rs {
		m[r.Lesson] = r
	}
	return m
}

func (p *proxyStore) Load(lesson string) (tutor.Record, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.recs[lesson]
	return r, ok
}

func (p *proxyStore) Save(r tutor.Record) error {
	p.mu.Lock()
	p.recs[r.Lesson] = r
	p.mu.Unlock()
	p.fw.send(frame{T: frSave, Record: &r})
	return nil
}

// Close is a no-op: the file this stands in for belongs to the parent,
// and a worker closing it would take every other visitor's progress with
// it.
func (p *proxyStore) Close() error { return nil }
