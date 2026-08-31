package tour

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/rohanthewiz/rweb"
)

// transcriptLimit bounds the replay buffer.
//
// 256KB is roughly thirty times a chapter's transcript, so the whole
// eight-chapter curriculum fits with room to spare and only a student who
// cats something large loses anything. That is the trade: the buffer is
// what a reload redraws from, and an unbounded one is a server a single
// `cat /dev/urandom` can grow until it dies. When it does bite, it says so
// — see trim.
const transcriptLimit = 256 << 10

// lineScan bounds how far trim will walk forward looking for a line
// boundary. Comfortably longer than any line a lesson or a shell command
// writes on purpose, and short enough that giving up costs nothing.
//
// trim also caps the walk at a quarter of the buffer, so the bound scales
// with a small transcript instead of swallowing it — the guarantee being
// that landing on a line boundary never costs more than a quarter of what
// was being kept.
const lineScan = 8 << 10

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

	// trimmed records that the head of the transcript has been dropped, so
	// a replay can admit it. Without this a student who scrolled up would
	// find the session apparently beginning mid-sentence.
	trimmed bool

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
	s.trim()
	s.send(rweb.SSEvent{Type: "out", Data: jsonLine(map[string]string{"text": string(p)})})
	return len(p), nil
}

// trim enforces transcriptLimit. Callers hold the lock.
//
// It slides the window rather than reallocating — the same trade the
// grading capture makes — but it does not cut where the arithmetic lands.
// The transcript is a byte stream carrying UTF-8 and ANSI escapes, and an
// arbitrary byte offset can land inside either: half a rune reaches the
// browser as a replacement character, and half an escape sequence reaches
// it as `[1;36m` printed literally in the middle of a word.
//
// So the cut advances to the start of the next whole line, which is past
// the end of any escape sequence (none contain a newline) and any rune.
//
// It advances at most lineScan bytes, and that bound is the whole lesson
// of this function. An unbounded search is the obvious version and it is
// badly wrong: a program whose output is one long line — macOS `base64`
// emits the entire encoding unwrapped — puts the next newline at the very
// END of the buffer, so "advance to it" discards everything and leaves a
// transcript of nothing. Found by watching a real page after a real flood;
// no test written against newline-terminated output can see it, which is
// why one below is written against output that has none.
//
// Past the bound, the cut is aligned on a rune instead and a broken escape
// sequence is accepted. That is the right trade: a quarter-megabyte line
// with colour changes inside it is not a real case, and losing the whole
// transcript to protect against it plainly is.
//
// One thing the cut cannot preserve is the SGR state that was open across
// it. The replay's leading text would inherit whatever colour the page was
// last left in; the notice attach prepends ends in a reset, which puts it
// back to plain. That is not a happy accident to rely on silently, hence
// this sentence.
func (s *sink) trim() {
	if len(s.log) <= s.limit {
		return
	}
	cut := len(s.log) - s.limit
	window := min(cut+min(lineScan, s.limit/4), len(s.log))
	if i := bytes.IndexByte(s.log[cut:window], '\n'); i >= 0 {
		cut += i + 1
	} else {
		// No line boundary within reach: step off any UTF-8 continuation
		// bytes so at least the first rune is whole.
		for cut < len(s.log) && s.log[cut]&0xC0 == 0x80 {
			cut++
		}
	}
	s.log = s.log[:copy(s.log, s.log[cut:])]
	s.trimmed = true
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
		text := string(s.log)
		if s.trimmed {
			text = fmt.Sprintf("\x1b[2m[… earlier output dropped — the transcript keeps the last %dKB …]\x1b[0m\n",
				s.limit>>10) + text
		}
		s.ch <- rweb.SSEvent{Type: "out", Data: jsonLine(map[string]string{"text": text, "replay": "1"})}
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
	s.trimmed = false
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
