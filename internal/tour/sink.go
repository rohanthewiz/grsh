package tour

import (
	"sync"

	"github.com/rohanthewiz/rweb"
)

// transcriptLimit bounds the replay buffer. Big enough for a chapter's
// worth of panels and output, small enough that a `cat`-happy student
// cannot grow the server without bound.
const transcriptLimit = 256 << 10

// eventBuffer is how far the output stream may run ahead of a browser.
// Generous, because the writes come from a live shell and blocking one to
// wait for a socket would be a shell that stutters when a page is slow.
const eventBuffer = 1024

// sink is one visitor's output: an io.Writer on the driver's side, an SSE
// stream on the browser's, and a bounded transcript in between.
//
// The transcript is what makes a reload work. An SSE stream carries only
// what happens after it connects, so without a replay a refreshed page
// would show an empty terminal and a sidebar insisting the student was on
// step four.
//
// Writes arrive from os/exec's copier goroutines while a command runs, so
// everything here locks. Sends to the browser never block: a page that has
// gone away stops draining, and a shell that blocked on that would hang on
// its own output.
type sink struct {
	mu    sync.Mutex
	log   []byte
	limit int

	ch chan any // the attached browser, or nil
	// dropped counts events lost to a full channel, so the next successful
	// send can admit it. Silently truncating a student's output would be
	// the kind of bug that gets blamed on the shell.
	dropped int
	closed  bool
}

func newSink(limit int) *sink { return &sink{limit: limit} }

// Write implements io.Writer for the driver's transcript. It never reports
// an error: a failure here would surface to the student as a command that
// could not write to stdout, which would be a lie about their shell.
func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, p...)
	if over := len(s.log) - s.limit; over > 0 {
		// Slide the window rather than reallocate, the same trade the
		// grading capture makes: the head of a long transcript is the part
		// nobody scrolls back to.
		s.log = s.log[:copy(s.log, s.log[over:])]
	}
	s.send(rweb.SSEvent{Type: "out", Data: jsonLine(map[string]string{"text": string(p)})})
	return len(p), nil
}

// state pushes a View to the browser. The page also gets the View as the
// reply to whatever it just posted; this is for the other cases — a reset,
// and anything a future background event might change.
func (s *sink) state(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.send(rweb.SSEvent{Type: "state", Data: jsonLine(v)})
}

// send posts to the attached browser if there is one and it is keeping up.
// Callers hold the lock.
func (s *sink) send(ev rweb.SSEvent) {
	if s.ch == nil {
		return
	}
	if s.dropped > 0 {
		// Report the gap before the event that follows it, so the notice
		// appears where the missing output would have been.
		select {
		case s.ch <- rweb.SSEvent{Type: "out",
			Data: jsonLine(map[string]string{"text": "\x1b[33m[output dropped — the page fell behind]\x1b[0m\n"})}:
			s.dropped = 0
		default:
			return // still full; the notice can wait with everything else
		}
	}
	select {
	case s.ch <- ev:
	default:
		s.dropped++
	}
}

// attach starts a new stream and ends any previous one.
//
// Closing the old channel is what tells rweb's SSE loop to let go: an SSE
// connection whose browser has navigated away is not detectably dead until
// something writes to it, so a reload would otherwise leave a reader
// holding a channel nobody will ever read from.
func (s *sink) attach() <-chan any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ch != nil {
		close(s.ch)
	}
	s.ch = make(chan any, eventBuffer)
	s.dropped = 0
	if s.closed {
		close(s.ch)
		return s.ch
	}
	// The replay goes in before the channel is visible to anyone else, so
	// the browser's first frames are the transcript it missed and nothing
	// can interleave ahead of them.
	if len(s.log) > 0 {
		s.ch <- rweb.SSEvent{Type: "out", Data: jsonLine(map[string]string{"text": string(s.log), "replay": "1"})}
	}
	return s.ch
}

// clear empties the transcript, for a reset. The browser is told to wipe
// its own copy, since a replay after this would no longer contain the
// lines still on its screen.
func (s *sink) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = s.log[:0]
	s.send(rweb.SSEvent{Type: "clear", Data: "{}"})
}

// close ends the stream for good.
func (s *sink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.ch != nil {
		close(s.ch)
		s.ch = nil
	}
}
