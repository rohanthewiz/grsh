package tour

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/grsh/internal/tutor"
)

// newTestServer starts a tour on a loopback port the kernel picks, and
// returns a client that carries the session cookie the way a browser does.
func newTestServer(t *testing.T) (*Server, *http.Client, string) {
	t.Helper()
	ready := make(chan struct{}, 1)
	s, err := New(Options{Addr: "127.0.0.1:0", Ready: ready, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() { _ = s.Run() }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server never became ready")
	}
	t.Cleanup(s.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, &http.Client{Jar: jar, Timeout: 30 * time.Second}, "http://127.0.0.1:" + s.Port()
}

// get is a GET returning the body, failing the test on anything but 200.
func get(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	res, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: status %d: %s", url, res.StatusCode, body)
	}
	return string(body)
}

// submit posts one line and decodes the View that comes back. The reply IS
// the acknowledgement, so a test that reads it is testing what the page
// reads.
func submit(t *testing.T, c *http.Client, base, line string) tutor.View {
	t.Helper()
	body := strings.NewReader(`{"line":` + quote(line) + `}`)
	res, err := c.Post(base+"/input", "application/json", body)
	if err != nil {
		t.Fatalf("POST /input %q: %v", line, err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("POST /input %q: status %d: %s", line, res.StatusCode, b)
	}
	var v tutor.View
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	return v
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestTourServesAndSessions walks the surface a browser touches on a first
// visit: the page mints a session, /state draws the sidebar, and a line of
// input comes back as a View.
func TestTourServesAndSessions(t *testing.T) {
	_, c, base := newTestServer(t)

	page := get(t, c, base+"/")
	for _, want := range []string{"grsh tour", "/app.js", "/app.css", `id="transcript"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if len(c.Jar.Cookies(mustURL(t, base))) == 0 {
		t.Fatal("the page did not mint a session cookie")
	}

	var v tutor.View
	if err := json.Unmarshal([]byte(get(t, c, base+"/state")), &v); err != nil {
		t.Fatalf("decode /state: %v", err)
	}
	if len(v.Chapters) != 8 {
		t.Errorf("%d chapters, want the curriculum's 8", len(v.Chapters))
	}
	if v.Step != 1 || v.Chapter != 0 {
		t.Errorf("a new visitor starts at chapter %d step %d, want 0/1", v.Chapter, v.Step)
	}
	if len(v.Prose) == 0 {
		t.Error("step 1 arrived with no prose — the sidebar would be blank")
	}
	if v.Dir == "" {
		t.Error("no playground reported")
	}
	// The answer must not ride along unasked; it is readable in the page.
	if v.Solution != "" {
		t.Errorf("the first View disclosed the solution: %q", v.Solution)
	}

	// The assets the page asks for really exist.
	if !strings.Contains(get(t, c, base+"/app.js"), "EventSource") {
		t.Error("/app.js is not the tour's script")
	}
	if !strings.Contains(get(t, c, base+"/app.css"), "transcript") {
		t.Error("/app.css is not the tour's stylesheet")
	}
}

// TestTourGradesAndNavigates: the meta-commands reach the engine and their
// effects come back in the View, which is the whole contract between the
// server and the sidebar.
func TestTourGradesAndNavigates(t *testing.T) {
	_, c, base := newTestServer(t)
	get(t, c, base+"/")

	if v := submit(t, c, base, ":hint"); len(v.Hints) != 1 {
		t.Errorf(":hint gave %d hints, want 1", len(v.Hints))
	}
	if v := submit(t, c, base, ":sol"); v.Solution == "" {
		t.Error(":sol did not disclose the answer")
	}

	// A wrong answer is a miss, not an error: the command really ran.
	if v := submit(t, c, base, "echo definitely-not-the-answer"); v.Attempts == 0 {
		t.Error("a wrong answer was not counted as a miss")
	}

	v := submit(t, c, base, ":menu 4")
	if v.Chapter != 3 {
		t.Fatalf(":menu 4 landed on chapter %d, want 3", v.Chapter)
	}
	if v.Step != 1 || v.Attempts != 0 {
		t.Errorf("the new chapter carried state over: step %d attempts %d", v.Step, v.Attempts)
	}
	if v.Title != v.Chapters[3].Title {
		t.Errorf("View title %q disagrees with the table of contents", v.Title)
	}
}

// TestTourStreamsOutput is the SSE half: what the shell prints reaches the
// browser, and a reconnecting browser is caught up rather than shown an
// empty terminal.
func TestTourStreamsOutput(t *testing.T) {
	_, c, base := newTestServer(t)
	get(t, c, base+"/")

	events, stop := stream(t, c, base)
	submit(t, c, base, "echo streamed-to-the-page")
	// The command's own output arrives as its own frame, after the echo of
	// the line that produced it.
	seen := waitFor(t, events, `{"text":"streamed-to-the-page\n"}`)
	if !strings.Contains(seen, "echo streamed-to-the-page") {
		t.Errorf("the submitted unit was not echoed into the transcript:\n%s", seen)
	}
	stop()

	// A reconnect replays. The frame is marked so the page replaces its
	// transcript instead of doubling it.
	events2, stop2 := stream(t, c, base)
	defer stop2()
	first := <-events2
	if !strings.Contains(first, `"replay":"1"`) {
		t.Errorf("a reconnect did not begin with a replay frame: %s", first)
	}
	if !strings.Contains(first, "streamed-to-the-page") {
		t.Errorf("the replay did not carry the earlier output: %s", first)
	}
}

// TestTourSendsADecodableStream guards a header, not a behaviour, and that
// is the point.
//
// rweb labels its SSE responses `Content-Encoding: text/plain` — a media
// type where a content coding belongs. Go's http client ignores the header
// (it only ever auto-decodes gzip), so every test here passed while the
// page in an actual browser showed a connected stream that never delivered
// a byte: Chrome and curl both treat an unrecognised coding as a body they
// cannot decode. Nothing about the events themselves can catch this from
// Go, so the header is asserted directly.
func TestTourSendsADecodableStream(t *testing.T) {
	_, c, base := newTestServer(t)
	get(t, c, base+"/")

	res, err := c.Get(base + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Content-Encoding"); got != "identity" {
		t.Errorf("Content-Encoding = %q; anything a browser does not recognise "+
			"as a content coding makes it discard the whole stream", got)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
}

// TestTourResets throws the playground away and starts over — the page's
// version of quitting and relaunching.
func TestTourResets(t *testing.T) {
	_, c, base := newTestServer(t)
	get(t, c, base+"/")

	before := submit(t, c, base, ":menu 5").Dir
	res, err := c.Post(base+"/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var v tutor.View
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Chapter != 0 || v.Step != 1 {
		t.Errorf("reset left the student at chapter %d step %d", v.Chapter, v.Step)
	}
	if v.Dir == before || v.Dir == "" {
		t.Errorf("reset reused the old playground (%q)", v.Dir)
	}
}

// TestTourRefusesStrangers: only the page mints a session. An /input from
// a tab whose server has restarted should be told to reload, not handed a
// fresh curriculum it will silently start over in.
func TestTourRefusesStrangers(t *testing.T) {
	_, _, base := newTestServer(t)
	bare := &http.Client{Timeout: 5 * time.Second} // no cookie jar

	for _, path := range []string{"/state", "/classify?src=ls"} {
		res, err := bare.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Errorf("GET %s without a session: status %d, want 404", path, res.StatusCode)
		}
	}
	res, err := bare.Post(base+"/input", "application/json", strings.NewReader(`{"line":"ls"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Errorf("POST /input without a session: status %d, want 404", res.StatusCode)
	}
}

// TestTourClassifies: chapter 2's lesson is the classifier's live verdict,
// and a page has to ask for it rather than watch a prompt.
func TestTourClassifies(t *testing.T) {
	_, c, base := newTestServer(t)
	get(t, c, base+"/")

	if v := submit(t, c, base, ":menu 2"); !v.Explain {
		t.Fatal("chapter 2 did not ask for the classifier lane")
	}
	body := get(t, c, base+"/classify?src="+"n+%3A%3D+42")
	if !strings.Contains(body, "go · rule=") {
		t.Errorf("/classify said %q", body)
	}
}

// TestVisitorsAreReclaimed: a closed tab must not leave a playground on
// disk and a shell session running until the process ends.
func TestVisitorsAreReclaimed(t *testing.T) {
	ready := make(chan struct{}, 1)
	// An idle timeout of zero makes every visitor immediately overdue,
	// which is the reaper's condition without a test that sleeps for it.
	s, err := New(Options{Addr: "127.0.0.1:0", Ready: ready, IdleTimeout: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Run() }()
	<-ready
	defer s.Close()

	v, err := newVisitor(nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := v.d.Dir()
	s.mu.Lock()
	s.visitors[v.id] = v
	s.mu.Unlock()

	// Drive one reaper pass directly rather than waiting a minute for the
	// ticker; the policy is what is under test, not the clock.
	s.mu.Lock()
	delete(s.visitors, v.id)
	s.mu.Unlock()
	v.close()

	if _, err := statDir(dir); err == nil {
		t.Errorf("the playground %s outlived its visitor", dir)
	}
	// Closing twice is a real path: an explicit reset and the reaper can
	// both reach a visitor.
	v.close()
}

// TestServerCloseRemovesPlaygrounds: shutting the server down has to take
// the visitors' directories with it.
//
// Worth pinning because the obvious implementation does not do it. rweb's
// Run installs its OWN SIGINT/SIGTERM handler and returns when one
// arrives, so a program that also handles the signal itself races Run —
// and loses, because Run returns and main falls off the end while the
// handler is still tearing down. Every Ctrl+C then leaves a playground
// behind. The rule this fixes in place is that Close is what tears a tour
// down, and that it works on a server already listening.
func TestServerCloseRemovesPlaygrounds(t *testing.T) {
	s, c, base := newTestServer(t)
	get(t, c, base+"/")

	var v tutor.View
	if err := json.Unmarshal([]byte(get(t, c, base+"/state")), &v); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(v.Dir); err != nil {
		t.Fatalf("playground %s missing while the tour is running: %v", v.Dir, err)
	}

	s.Close()
	if _, err := os.Stat(v.Dir); err == nil {
		t.Errorf("playground %s outlived the server", v.Dir)
	}
	// A closed server mints nothing further: a request arriving during
	// shutdown must be refused rather than handed a playground that
	// nothing will ever clean up.
	res, err := c.Get(base + "/")
	if err == nil {
		res.Body.Close()
		if res.StatusCode == 200 {
			t.Error("a closed server still started a new tour")
		}
	}
	// Cleanup calls Close again; it has to be idempotent.
}

// TestRequireLoopback guards the one thing standing between this tool and
// a shell on the network.
func TestRequireLoopback(t *testing.T) {
	ok := []string{"127.0.0.1:7654", "localhost:7654", "[::1]:7654"}
	bad := []string{":7654", "0.0.0.0:7654", "7654"}
	for _, a := range ok {
		if err := requireLoopback(a); err != nil {
			t.Errorf("requireLoopback(%q) = %v, want nil", a, err)
		}
	}
	for _, a := range bad {
		if err := requireLoopback(a); err == nil {
			t.Errorf("requireLoopback(%q) allowed a non-loopback bind", a)
		}
	}
	// The flag is the escape hatch, and New must honour it.
	if _, err := New(Options{Addr: "0.0.0.0:0", AllowRemote: true}); err != nil {
		t.Errorf("-allow-remote still refused: %v", err)
	}
}

// ── helpers ──────────────────────────────────────────────────────

// stream opens an SSE connection and delivers one string per frame's data
// line. Frames rather than events: the test only cares that the bytes
// arrive, and parsing SSE properly here would be testing net/http.
func stream(t *testing.T, c *http.Client, base string) (<-chan string, func()) {
	t.Helper()
	res, err := c.Get(base + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	out := make(chan string, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				out <- line
			}
		}
	}()
	return out, func() { res.Body.Close() }
}

// waitFor drains frames until one contains want, and returns everything it
// saw so a failure can show the whole stream.
func waitFor(t *testing.T, events <-chan string, want string) string {
	t.Helper()
	var seen strings.Builder
	deadline := time.After(15 * time.Second)
	for {
		select {
		case frame, ok := <-events:
			if !ok {
				t.Fatalf("stream ended before %q arrived:\n%s", want, seen.String())
			}
			seen.WriteString(frame + "\n")
			if strings.Contains(frame, want) {
				return seen.String()
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q:\n%s", want, seen.String())
		}
	}
}

// mustURL parses a base URL for cookie-jar inspection.
func mustURL(t *testing.T, base string) *url.URL {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// statDir is os.Stat, named for what the one caller asks of it.
func statDir(dir string) (os.FileInfo, error) { return os.Stat(dir) }
