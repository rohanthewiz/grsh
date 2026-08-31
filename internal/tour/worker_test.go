package tour

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/grsh/internal/tutor"
)

// The worker architecture, tested through the surface a browser touches.
//
// These run against real subprocesses — TestMain makes this test binary
// able to be one — because the thing under test IS the process boundary.
// A test with the workers switched off would be testing the code that the
// boundary was introduced to replace.

// newBrowser is a second tab: its own cookie jar, its own session, its own
// worker. The name is the point — every test here is about two students
// who must not be able to feel each other.
func newBrowser(t *testing.T, base string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar, Timeout: 60 * time.Second}
	get(t, c, base+"/") // mints the session, which builds the worker
	return c
}

// TestOneStudentDoesNotWaitForAnother is the reason worker processes
// exist.
//
// A grsh session's working directory and environment are the PROCESS's,
// so two students in one process have to take turns — tutor's eval gate
// enforces exactly that, correctly, and the cost is that one student's
// `sleep 30` is thirty seconds of everyone else's time.
//
// Both halves are here on purpose. The second one runs the same scenario
// with Options.InProcess and shows it blocking, because a test that only
// proved the fast case would pass just as well if the slow case had never
// been slow.
func TestOneStudentDoesNotWaitForAnother(t *testing.T) {
	t.Run("a worker each", func(t *testing.T) {
		_, a, base := newTestServer(t)
		get(t, a, base+"/")
		b := newBrowser(t, base)

		events, stop := stream(t, a, base)
		defer stop()
		done := make(chan struct{})
		go func() {
			defer close(done)
			// The echo is the starting gun: it is how the test knows the
			// pipeline has actually been forked and the first student is
			// genuinely occupying their shell.
			submit(t, a, base, "echo running; sleep 30")
		}()
		waitFor(t, events, "running")

		start := time.Now()
		submit(t, b, base, "echo second-student")
		if waited := time.Since(start); waited > 5*time.Second {
			t.Errorf("the second student waited %s behind the first one's sleep", waited)
		}

		// Let the first student's shell go. Polled, because `sleep` is
		// forked a moment after `echo` reaches the page and an early
		// signal finds no foreground pipeline.
		deadline := time.After(15 * time.Second)
		for signalled := false; !signalled; {
			select {
			case <-deadline:
				t.Fatal("nothing was ever signalled")
			case <-time.After(50 * time.Millisecond):
			}
			signalled = post(t, a, base+"/interrupt")["signalled"]
		}
		<-done
	})

	t.Run("in one process they take turns", func(t *testing.T) {
		_, a, base := newTestServerWith(t, Options{InProcess: true})
		get(t, a, base+"/")
		b := newBrowser(t, base)

		events, stop := stream(t, a, base)
		defer stop()
		done := make(chan struct{})
		go func() {
			defer close(done)
			submit(t, a, base, "echo running; sleep 3")
		}()
		waitFor(t, events, "running")

		start := time.Now()
		submit(t, b, base, "echo second-student")
		if waited := time.Since(start); waited < time.Second {
			t.Errorf("the second student got through in %s — the eval gate was not held, "+
				"so this test can no longer tell the two architectures apart", waited)
		}
		<-done
	})
}

// TestStudentsKeepTheirOwnDirectoryAndEnvironment: the two pieces of
// process-global state a shell writes to constantly.
//
// In one process these are shared and tutor's driver has to save and
// restore them around every evaluation — which works, and is why the
// in-process path is still correct. In a worker each they are simply not
// shared, and this test is what says so from the outside.
func TestStudentsKeepTheirOwnDirectoryAndEnvironment(t *testing.T) {
	_, a, base := newTestServer(t)
	get(t, a, base+"/")
	b := newBrowser(t, base)

	dirA := submit(t, a, base, "cd notes").Dir
	dirB := submit(t, b, base, "pwd").Dir
	if dirA == "" || dirA == dirB {
		t.Fatalf("the two students share a playground (%q and %q)", dirA, dirB)
	}
	submit(t, a, base, "export TOUR_STUDENT=alpha")

	// The first student moved and exported; the second must see neither.
	submit(t, b, base, "pwd; printenv TOUR_STUDENT")
	seenB := transcriptOf(t, b, base)
	if !strings.Contains(seenB, "\n"+dirB+"\n") {
		t.Errorf("the second student was not standing in their own playground %s:\n%s", dirB, seenB)
	}
	if strings.Contains(seenB, "\nalpha\n") {
		t.Errorf("the first student's export reached the second:\n%s", seenB)
	}

	// And the first student's own state survived all of that, which is the
	// half a fix that merely stopped sharing would break.
	submit(t, a, base, "pwd; printenv TOUR_STUDENT")
	seenA := transcriptOf(t, a, base)
	if !strings.Contains(seenA, "\n"+dirA+"/notes\n") {
		t.Errorf("the first student's cd was lost:\n%s", seenA)
	}
	if !strings.Contains(seenA, "\nalpha\n") {
		t.Errorf("the first student's export was lost:\n%s", seenA)
	}
}

// TestShutdownDoesNotWaitForASleepingStudent.
//
// A visitor's lock is held for the whole of a running command, so closing
// one used to mean waiting for whatever they had typed — five minutes of
// `sleep 300` on every Ctrl+C, on every reaper pass, and on Reset. The
// signal path is the one thing that does not queue behind that lock, so
// close reaches for it first (visitor.stopWork).
func TestShutdownDoesNotWaitForASleepingStudent(t *testing.T) {
	s, c, base := newTestServer(t)
	get(t, c, base+"/")
	events, stop := stream(t, c, base)
	defer stop()

	dir := submit(t, c, base, "echo settling-in").Dir
	if dir == "" {
		t.Fatal("no playground")
	}
	running := make(chan struct{})
	go func() {
		defer close(running)
		submit(t, c, base, "echo running; sleep 300")
	}()
	waitFor(t, events, "running")

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		s.Close()
		done <- time.Since(start)
	}()
	select {
	case took := <-done:
		if took > 20*time.Second {
			t.Errorf("shutdown took %s — it waited for the student's command", took)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown never finished; it is waiting for a five-minute sleep")
	}
	if _, err := statDir(dir); err == nil {
		t.Errorf("the playground %s outlived the shutdown", dir)
	}
	// The student's own request comes back too — with a dead session
	// rather than with nothing — which is what stops the browser hanging
	// on a server that has already gone.
	select {
	case <-running:
	case <-time.After(20 * time.Second):
		t.Error("the in-flight command never returned to its browser")
	}
}

// TestADeadWorkerIsToldToTheStudent: a worker is a process, and processes
// die. The page must not simply stop answering — the student is told, in
// the transcript, and the View says the session ended so the page greys
// out its input and offers Reset.
func TestADeadWorkerIsToldToTheStudent(t *testing.T) {
	s, c, base := newTestServer(t)
	get(t, c, base+"/")
	events, stop := stream(t, c, base)
	defer stop()

	re := onlyRemote(t, s)
	if err := re.cmd.Process.Kill(); err != nil {
		t.Fatalf("could not kill the worker: %v", err)
	}
	select {
	case <-re.exited:
	case <-time.After(15 * time.Second):
		t.Fatal("the parent never noticed its worker die")
	}
	waitFor(t, events, "ended unexpectedly")

	if v := submit(t, c, base, "echo anyone-there"); !v.Ended {
		t.Error("a session with no worker did not report itself ended")
	}

	// And Reset really is the way back: a new worker, a new playground.
	res, err := c.Post(base+"/reset", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /reset: %v", err)
	}
	res.Body.Close()
	if v := submit(t, c, base, "echo back"); v.Ended || v.Dir == "" {
		t.Errorf("reset did not rebuild the session: ended=%v dir=%q", v.Ended, v.Dir)
	}
}

// TestProgressTravelsThroughTheParent.
//
// The database is a file one process holds open, and that process is the
// server. So a worker gets a snapshot of the student's records at startup
// and posts its saves back — see worker.go. Both directions are here:
// the save that reaches the real store, and the snapshot that lets the
// next worker resume from it.
func TestProgressTravelsThroughTheParent(t *testing.T) {
	store := &recordingStore{recs: map[string]tutor.Record{}}
	_, a, base := newTestServerWith(t, Options{Store: store})
	get(t, a, base+"/")

	// :skip advances a step, and advancing is what saves.
	if v := submit(t, a, base, ":skip"); v.Step != 2 {
		t.Fatalf(":skip left the student on step %d, want 2", v.Step)
	}
	// Saves are fire-and-forget by design — a prompt should not wait for a
	// disk write — so this is the one place the test has to poll.
	deadline := time.After(10 * time.Second)
	for store.saves() == 0 {
		select {
		case <-deadline:
			t.Fatal("no save ever reached the parent's store")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// A second tab starts a second worker, which is handed the records the
	// first one wrote and opens where they left off.
	b := newBrowser(t, base)
	var v tutor.View
	decodeInto(t, get(t, b, base+"/state"), &v)
	if v.Step != 2 {
		t.Errorf("a new worker opened on step %d, want 2 — the snapshot did not reach it", v.Step)
	}
}

// TestWorkerRejectsAnythingBeforeHello pins the one ordering rule the
// protocol has. Driven over plain pipes rather than a process, which is
// what RunWorker takes readers and writers for.
func TestWorkerRejectsAnythingBeforeHello(t *testing.T) {
	reqR, reqW, _ := os.Pipe()
	frR, frW, _ := os.Pipe()
	ctlR, ctlW, _ := os.Pipe()
	defer func() { reqW.Close(); ctlW.Close(); frR.Close() }()

	code := make(chan int, 1)
	go func() { code <- RunWorker(reqR, frW, ctlR); frW.Close() }()

	if _, err := reqW.Write([]byte(`{"id":1,"op":"submit","lines":["echo hi"]}` + "\n")); err != nil {
		t.Fatal(err)
	}
	var f frame
	decodeFrame(t, frR, &f)
	if f.Err == "" {
		t.Errorf("a worker served a request before hello: %+v", f)
	}
	select {
	case got := <-code:
		if got == 0 {
			t.Error("a worker that refused its first request exited 0")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not exit after refusing")
	}
}

// ── helpers ──────────────────────────────────────────────────────

// transcriptOf is the whole of a student's transcript, read the way a
// reloading browser reads it: attach to the stream and take the replay.
//
// Preferred here over matching frames as they arrive, for two reasons.
// The transcript echoes what the student typed, so waiting for a marker
// in the output finds it first in the input that asked for it. And the
// reply to /input is only sent once every byte the command produced has
// been written to the sink (see remoteEngine.read), so by the time submit
// has returned the replay is complete — no polling, and the ordering
// guarantee gets exercised on every call.
//
// Anything a value could be matched against is stripped of nothing: the
// escape codes stay, since a caller looking for a line on its own is
// looking for "\n" + text + "\n" and the codes never sit between them.
func transcriptOf(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	events, stop := stream(t, c, base)
	defer stop()
	select {
	case line, ok := <-events:
		if !ok {
			t.Fatal("the stream ended before the replay arrived")
		}
		var payload struct {
			Text string `json:"text"`
		}
		decodeInto(t, line, &payload)
		return payload.Text
	case <-time.After(15 * time.Second):
		t.Fatal("no replay arrived")
	}
	return ""
}

// decodeInto unmarshals a response body the test already has in hand.
func decodeInto(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

// decodeFrame reads one frame off a worker's output, with a deadline —
// a protocol test that hangs tells nobody anything until the whole run
// times out.
func decodeFrame(t *testing.T, r io.Reader, f *frame) {
	t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- json.NewDecoder(r).Decode(f) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("decode frame: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker said nothing")
	}
}

// onlyRemote returns the one visitor's worker, failing if the server is
// not running exactly one on the architecture under test.
func onlyRemote(t *testing.T, s *Server) *remoteEngine {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.visitors) != 1 {
		t.Fatalf("%d visitors, want 1", len(s.visitors))
	}
	for _, v := range s.visitors {
		re, ok := v.engine().(*remoteEngine)
		if !ok {
			t.Fatalf("visitor is running %T, not a worker", v.engine())
		}
		return re
	}
	return nil
}

// recordingStore is the progress database, in memory and countable.
// Locked because the saves arrive on the parent's frame-reader goroutine
// while the test reads from its own.
type recordingStore struct {
	mu    sync.Mutex
	recs  map[string]tutor.Record
	count int
}

func (r *recordingStore) Load(lesson string) (tutor.Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.recs[lesson]
	return rec, ok
}

func (r *recordingStore) Save(rec tutor.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs[rec.Lesson] = rec
	r.count++
	return nil
}

func (r *recordingStore) Close() error { return nil }

func (r *recordingStore) saves() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}
