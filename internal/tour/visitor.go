package tour

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/rohanthewiz/grsh/internal/tutor"
)

// transcriptWidth pins the engine's panel rule. There is no terminal to
// ask, 76 is the engine's own upper bound, and the page's transcript is
// sized to hold it without wrapping.
const transcriptWidth = 76

// visitor is one student: an engine, the output stream feeding their page,
// and the lock that makes an HTTP server's goroutines look to the engine
// like the single caller it expects.
//
// The page's step panel is routed to io.Discard rather than into the
// transcript: the sidebar renders the same step from View, and printing it
// twice would be the web tour's most obvious tell that it was a terminal
// wearing a costume.
type visitor struct {
	id   string
	sink *sink

	// inProcess runs this student's driver here rather than in a worker of
	// their own. Off by default; see Options.InProcess.
	inProcess bool

	// mu serializes everything that touches the engine. Interrupt is the
	// documented exception and takes nothing — a stop button that queues
	// behind the command it is stopping is not a stop button.
	mu  sync.Mutex
	eng engine
	// engMu guards the eng FIELD, and nothing the field leads to. It
	// exists because interrupt deliberately does not take mu: a restart
	// swaps the engine while mu is held, and the signal path reads the
	// field with mu belonging to someone else. Held for a load and a
	// store, never across a call.
	engMu sync.Mutex

	seenMu sync.Mutex
	seen   time.Time
}

// newVisitor mints a session and opens the student's first chapter, which
// creates their playground on disk — in a worker process of the student's
// own unless the server was told to stay in-process.
func newVisitor(store tutor.Store, inProcess bool) (*visitor, error) {
	v := &visitor{id: newID(), sink: newSink(transcriptLimit), seen: time.Now(), inProcess: inProcess}
	if err := v.start(store); err != nil {
		return nil, err
	}
	return v, nil
}

// start builds the engine. Split out from newVisitor so restart can reuse
// it without minting a new id — a reset should keep the browser's cookie
// working, since the page it came from is still open.
//
// The greeting goes in first, and it goes in HERE rather than in the
// engine: it must land above the chapter banner, and with a worker that
// banner arrives asynchronously from another process the moment it starts.
func (v *visitor) start(store tutor.Store) error {
	v.greet()
	var (
		eng engine
		err error
	)
	if v.inProcess {
		eng, err = newLocalEngine(v.sink, store)
	} else {
		eng, err = newRemoteEngine(v.sink, store)
	}
	if err != nil {
		return err
	}
	v.engMu.Lock()
	v.eng = eng
	v.engMu.Unlock()
	return nil
}

// engine reads the current engine without waiting for whatever is running
// on it. Only the signal path needs this; every other caller already holds
// mu, which excludes the one writer.
func (v *visitor) engine() engine {
	v.engMu.Lock()
	defer v.engMu.Unlock()
	return v.eng
}

// interrupt is SIGINT to the student's foreground pipeline: the stop
// button, and Ctrl+C in the page's input.
func (v *visitor) interrupt() bool {
	e := v.engine()
	return e != nil && e.Interrupt()
}

// kill is the escalation, for a pipeline that ignored SIGINT — a second
// Ctrl+C, the way a terminal user reaches for `kill -9` after the first
// one does nothing. Separated from interrupt rather than retried
// automatically, because SIGKILL leaves no chance to clean up and the
// student is better placed than a timer to decide they have waited enough.
func (v *visitor) kill() bool {
	e := v.engine()
	return e != nil && e.Kill()
}

// submit feeds physical lines and returns the resulting View. The lines go
// in under one lock so a paste cannot interleave with another request's
// line halfway through a block.
func (v *visitor) submit(lines []string) tutor.View {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.eng.Submit(lines)
}

func (v *visitor) view() tutor.View {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.eng.View()
}

// dir is the student's playground on disk. Only the reaper's test and the
// server's own bookkeeping need it; the page reads it out of the View.
func (v *visitor) dir() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.eng.Dir()
}

// classify reads the classifier's verdict for a half-typed line. It shares
// the driver's lock, so it blocks while a command is running — which is
// honest: the shell is busy, and the lane going quiet says so.
func (v *visitor) classify(src string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.eng.Classify(src)
}

// restart throws the engine away and starts over on a clean playground.
//
// The teardown happens outside mu for the same reason close's does, and
// then mu is taken to swap the engine in — which excludes a concurrent
// submit from reading a field that is halfway through being replaced.
func (v *visitor) restart(store tutor.Store) error {
	v.stop()
	v.mu.Lock()
	defer v.mu.Unlock()
	v.sink.clear()
	return v.start(store)
}

// close removes the playground and detaches the browser.
func (v *visitor) close() {
	v.stop()
	v.sink.close()
}

// stop ends this student's session without waiting for their lock.
//
// mu is held for the whole of a running command, so a teardown that took
// it would wait for whatever the student happened to have typed — five
// minutes of `sleep 300` on every Ctrl+C, on every reaper pass, and on
// Reset. Neither of the calls here queues behind that lock: Kill is the
// signal path, which exists precisely to be heard while a command is
// running, and engine.Close is required to be safe from any goroutine.
//
// The Kill comes first anyway. It is the polite version — the worker
// gets to remove its own playground and exit — where Close's fallback is
// to kill the process and clean up after it.
//
// A Submit still in flight returns a View of a session that has just
// ended underneath it. Nobody is left to read it: a reset replaces the
// page's state a moment later, and a shutdown has no page to tell.
func (v *visitor) stop() {
	e := v.engine()
	if e == nil {
		return
	}
	e.Kill()
	e.Close()
}

// greet is the transcript's first paragraph, written before the driver so
// it lands above the first chapter's banner.
//
// It says the two things a browser cannot show by being a browser: that
// this is a real shell running real commands on this machine, and that the
// tutor's colon-commands are typed here like anything else — the sidebar's
// buttons are a convenience for the common ones, not the whole vocabulary.
func (v *visitor) greet() {
	fmt.Fprintf(v.sink, "\x1b[1mgrsh tour\x1b[0m\x1b[2m — a real grsh session in a throwaway directory.\x1b[0m\n")
	fmt.Fprintf(v.sink, "\x1b[2mThe lesson is on the right; type here to answer it.\x1b[0m\n")
	fmt.Fprintf(v.sink, "\x1b[2mTutor commands work too — \x1b[0m\x1b[1;36m:help\x1b[0m\x1b[2m lists them.\x1b[0m\n")
}

func (v *visitor) touch() {
	v.seenMu.Lock()
	v.seen = time.Now()
	v.seenMu.Unlock()
}

func (v *visitor) lastSeen() time.Time {
	v.seenMu.Lock()
	defer v.seenMu.Unlock()
	return v.seen
}

// newID is the session cookie's value. Sixteen random bytes: it is a
// capability to a live shell on this machine, so it is generated the way a
// session token should be even though nothing but this process ever sees
// it.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform grsh builds for, and a
		// tour that refused to start over a random number would be worse
		// than one with a predictable id on a loopback socket.
		return hex.EncodeToString([]byte(time.Now().String()))
	}
	return hex.EncodeToString(b[:])
}
