package tour

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rohanthewiz/grsh/internal/tutor"
)

// visitor is one student: a driver, the output stream feeding their page,
// and the lock that makes an HTTP server's goroutines look to the driver
// like the single caller it expects.
//
// The page's step panel is routed to io.Discard rather than into the
// transcript: the sidebar renders the same step from View, and printing it
// twice would be the web tour's most obvious tell that it was a terminal
// wearing a costume.
type visitor struct {
	id   string
	sink *sink

	// mu serializes everything that touches the driver. Interrupt is the
	// documented exception and takes nothing — a stop button that queues
	// behind the command it is stopping is not a stop button.
	mu sync.Mutex
	d  *tutor.Driver

	seenMu sync.Mutex
	seen   time.Time
}

// newVisitor mints a session and opens the student's first chapter, which
// creates their playground on disk.
func newVisitor(store tutor.Store) (*visitor, error) {
	v := &visitor{id: newID(), sink: newSink(transcriptLimit), seen: time.Now()}
	if err := v.start(store); err != nil {
		return nil, err
	}
	return v, nil
}

// start builds the driver. Split out from newVisitor so restart can reuse
// it without minting a new id — a reset should keep the browser's cookie
// working, since the page it came from is still open.
func (v *visitor) start(store tutor.Store) error {
	v.greet()
	d, err := tutor.NewDriver(v.sink, tutor.ResumeChapter(store), tutor.DriverOptions{
		// Colour on: the transcript is a terminal surface, and the page
		// renders the escape codes as spans. The engine's palette is part
		// of how a lesson reads.
		Color: true,
		// A fixed width, because there is no terminal to ask. 76 is the
		// engine's own upper bound, and the page's transcript is sized to
		// hold it without wrapping.
		Width:  76,
		Panels: io.Discard,
		Store:  store,
		// No controlling terminal here, so foreground pipelines need their
		// own process group for Interrupt to have anything to signal.
		Embedded: true,
	})
	if err != nil {
		return err
	}
	v.d = d
	return nil
}

// submit feeds physical lines and returns the resulting View. The lines go
// in under one lock so a paste cannot interleave with another request's
// line halfway through a block.
func (v *visitor) submit(lines []string) tutor.View {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, line := range lines {
		v.d.Submit(line)
	}
	return v.d.View()
}

func (v *visitor) view() tutor.View {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.d.View()
}

// classify reads the classifier's verdict for a half-typed line. It shares
// the driver's lock, so it blocks while a command is running — which is
// honest: the shell is busy, and the lane going quiet says so.
func (v *visitor) classify(src string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.d.Classify(src)
}

// restart throws the driver away and starts over on a clean playground.
func (v *visitor) restart(store tutor.Store) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.d.Close()
	v.sink.clear()
	return v.start(store)
}

// close removes the playground and detaches the browser. Taking the lock
// means an in-flight command finishes first: killing a session mid-Eval
// would leave the child process orphaned and the playground busy.
func (v *visitor) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.d.Close()
	v.sink.close()
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
