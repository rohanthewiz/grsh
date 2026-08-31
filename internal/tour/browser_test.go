package tour

// The tour, in an actual browser.
//
// Two of the four bugs this feature shipped with were invisible to Go and
// found by a person looking at a page:
//
//   - Ctrl+C was bound to the input, and the input is disabled while a
//     command runs — so the one moment the key means anything was the one
//     moment nothing was listening.
//   - The transcript cap searched forward for a line boundary without a
//     bound, and on output with no newlines in it (macOS `base64` writes
//     the whole encoding unwrapped) that search runs to the end and throws
//     the transcript away.
//
// Neither is reachable from a test that speaks HTTP. Both fail this file
// when reintroduced, which was checked rather than assumed.
//
// The third browser-shaped bug — rweb sending `Content-Encoding:
// text/plain` on the SSE stream, which made browsers drop a body every Go
// client read happily — is NOT caught here, and that is worth writing
// down: today's Chrome renders the stream perfectly well with the bad
// header on it. What guards that one is TestTourSendsADecodableStream,
// asserting on the header itself. A browser is evidence about one
// browser, on one day; it is not a substitute for pinning the wire.
//
// # Why there is a CDP client in here
//
// Chrome speaks the DevTools Protocol over a WebSocket, or — with
// --remote-debugging-pipe — over two inherited file descriptors carrying
// NUL-terminated JSON. The second needs no WebSocket implementation, so
// the whole client is the sixty lines below and the module keeps its
// dependency list. That list is a stated property of this project: the
// shell links neither a web framework nor a browser driver, and a test
// dependency would still be in go.mod.
//
// The test skips, loudly, when no browser is installed.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestTourInABrowser is one browser, one server, and the sequence a
// student actually performs. It is written as phases rather than as
// separate tests because launching a browser is the expensive part and
// every phase wants the state the previous one left.
func TestTourInABrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("browser tests launch a real browser")
	}
	b := newChrome(t)
	_, _, base := newTestServer(t)
	b.navigate(t, base+"/")

	t.Run("the page draws itself from a cold start", func(t *testing.T) {
		// The transcript arrives over SSE and over nothing else, and this
		// is the only test in the tree that reads it with an EventSource
		// rather than by parsing the frames itself.
		b.waitFor(t, `document.getElementById("transcript").textContent.includes("chapter 1")`,
			"the chapter banner never reached the transcript")
		// The sidebar is drawn from the View, over a separate request.
		b.waitFor(t, `document.getElementById("stepTitle").textContent.trim().length > 0`,
			"the sidebar never drew a step")
		if dir := b.evalString(t, `document.getElementById("dir").textContent`); !strings.Contains(dir, "grsh-tutor-") {
			t.Errorf("the header does not name the playground: %q", dir)
		}
	})

	t.Run("typing a command runs it", func(t *testing.T) {
		b.typeLine(t, "echo browser-was-here")
		b.waitFor(t, `document.getElementById("transcript").textContent.includes("browser-was-here")`,
			"the command's output never appeared")
		// The input comes back: send()'s finally clause is what re-enables
		// it, and a page that forgot would look exactly like a shell that
		// hung.
		b.waitFor(t, `!document.getElementById("line").disabled`,
			"the input stayed disabled after the command finished")
	})

	t.Run("ctrl+C reaches a running command", func(t *testing.T) {
		// The bug this pins: the input is disabled for the whole of a
		// running command, and a disabled input receives no key events. So
		// the listener is on the document, and dispatching to the document
		// is how this test can tell the difference.
		b.typeLine(t, "echo running; sleep 30")
		b.waitFor(t, `document.getElementById("transcript").textContent.includes("running")`,
			"the command never started")
		b.waitFor(t, `document.getElementById("line").disabled`,
			"the page did not mark itself busy while a command ran")

		// Retried, because `sleep` is forked a moment after `echo` reaches
		// the page and a signal that arrives in between finds nothing.
		deadline := time.Now().Add(30 * time.Second)
		for {
			b.eval(t, `document.dispatchEvent(new KeyboardEvent("keydown",`+
				`{key:"c", ctrlKey:true, bubbles:true, cancelable:true})), true`)
			if b.evalBool(t, `!document.getElementById("line").disabled`) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Ctrl+C on the document never stopped a 30-second sleep")
			}
			time.Sleep(200 * time.Millisecond)
		}
		if txt := b.evalString(t, `document.getElementById("transcript").textContent`); !strings.Contains(txt, "^C") {
			t.Error("the transcript does not show the interrupt the student typed")
		}
	})

	t.Run("a reload replays the session", func(t *testing.T) {
		// An SSE stream carries only what happens after it connects, so
		// everything above depends on the replay in sink.attach — and a
		// reload is the only way to exercise it as a browser does.
		b.call(t, "Page.reload", map[string]any{"ignoreCache": true})
		b.waitFor(t, `document.getElementById("transcript").textContent.includes("browser-was-here")`,
			"the reloaded page did not replay the session")
	})

	t.Run("a flood is trimmed and says so", func(t *testing.T) {
		// The other bug a browser found. The transcript is capped, and the
		// first version of the cap searched forward for a line boundary
		// without a bound — which on output that has no newlines at all
		// (macOS `base64` writes the whole encoding unwrapped) found the
		// one at the very end and threw the transcript away.
		//
		// What is pinned here is the end-to-end shape: after a flood and a
		// reload the student still has a transcript, and it admits at the
		// top that it lost the beginning.
		b.typeLine(t, "head -c 300000 /dev/urandom | base64")
		b.waitForWith(t, 60*time.Second,
			`!document.getElementById("line").disabled &&`+
				`document.getElementById("transcript").textContent.length > 100000`,
			"the flood never arrived")

		b.call(t, "Page.reload", map[string]any{"ignoreCache": true})
		b.waitFor(t, `document.getElementById("transcript").textContent.length > 1000`,
			"the reload came back to an empty transcript — the trim ate it")
		head := b.evalString(t, `document.getElementById("transcript").textContent.slice(0, 300)`)
		if !strings.Contains(head, "earlier output dropped") {
			t.Errorf("the transcript was trimmed without saying so; it begins:\n%q", head)
		}
	})
}

// ── a DevTools client, over a pipe ───────────────────────────────

// browser is one headless Chrome and the session attached to its one tab.
type browser struct {
	cmd  *exec.Cmd
	in   *os.File      // we write; the browser's fd 3
	out  *bufio.Reader // the browser's fd 4
	sess string        // the attached target's session id
	id   int           // command sequence
}

// chromeCandidates is where a browser might be. GRSH_TOUR_CHROME comes
// first so a machine with an unusual install — or a CI image — can say so
// without this list having to know about it.
func chromeCandidates() []string {
	if p := os.Getenv("GRSH_TOUR_CHROME"); p != "" {
		return []string{p}
	}
	if runtime.GOOS == "darwin" {
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		}
	}
	return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"}
}

func findChrome() string {
	for _, c := range chromeCandidates() {
		if strings.ContainsRune(c, os.PathSeparator) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// newChrome launches a headless browser with a throwaway profile and
// attaches to one blank tab.
func newChrome(t *testing.T) *browser {
	t.Helper()
	exe := findChrome()
	if exe == "" {
		t.Skip("no browser found; set GRSH_TOUR_CHROME to run the browser tests")
	}

	// Chrome reads commands from fd 3 and writes them to fd 4. The pair of
	// pipes is the whole transport.
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	profile := t.TempDir()

	cmd := exec.Command(exe,
		"--headless=new",
		"--remote-debugging-pipe",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		// A profile of its own, so the test never touches — or is
		// influenced by — the browser the developer is using.
		"--user-data-dir="+profile,
		"about:blank",
	)
	cmd.ExtraFiles = []*os.File{inR, outW}
	if testing.Verbose() {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("could not start %s: %v", exe, err)
	}
	inR.Close()
	outW.Close()

	b := &browser{cmd: cmd, in: inW, out: bufio.NewReaderSize(outR, 1<<20)}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		inW.Close()
		outR.Close()
	})

	target := b.call(t, "Target.createTarget", map[string]any{"url": "about:blank"})["targetId"]
	// flatten:true puts the page session on this same pipe, tagged with a
	// session id, rather than tunnelled inside Target.sendMessageToTarget.
	b.sess, _ = b.call(t, "Target.attachToTarget",
		map[string]any{"targetId": target, "flatten": true})["sessionId"].(string)
	if b.sess == "" {
		t.Fatal("could not attach to a browser tab")
	}
	return b
}

// call sends one command and returns its result, discarding the events
// that arrive in between — this client subscribes to nothing.
func (b *browser) call(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	b.id++
	id := b.id
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if b.sess != "" && !strings.HasPrefix(method, "Target.") {
		msg["sessionId"] = b.sess
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.in.Write(append(raw, 0)); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("the browser never answered %s", method)
		}
		line, err := b.out.ReadBytes(0)
		if err != nil {
			t.Fatalf("read after %s: %v", method, err)
		}
		var reply struct {
			ID     int            `json:"id"`
			Result map[string]any `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(line[:len(line)-1], &reply); err != nil {
			continue // an event we do not care about, or a frame we cannot read
		}
		if reply.ID != id {
			continue
		}
		if reply.Error != nil {
			t.Fatalf("%s: %s", method, reply.Error.Message)
		}
		return reply.Result
	}
}

func (b *browser) navigate(t *testing.T, url string) {
	t.Helper()
	b.call(t, "Page.navigate", map[string]any{"url": url})
}

// eval runs an expression in the page and returns its value. awaitPromise
// so an expression may be async; returnByValue so the result is data
// rather than a remote object handle this client has no way to inspect.
func (b *browser) eval(t *testing.T, js string) any {
	t.Helper()
	res := b.call(t, "Runtime.evaluate", map[string]any{
		"expression":    js,
		"awaitPromise":  true,
		"returnByValue": true,
	})
	if ex, ok := res["exceptionDetails"]; ok {
		t.Fatalf("evaluating %s threw: %v", js, ex)
	}
	if r, ok := res["result"].(map[string]any); ok {
		return r["value"]
	}
	return nil
}

func (b *browser) evalString(t *testing.T, js string) string {
	t.Helper()
	s, _ := b.eval(t, js).(string)
	return s
}

func (b *browser) evalBool(t *testing.T, js string) bool {
	t.Helper()
	v, _ := b.eval(t, js).(bool)
	return v
}

// waitFor polls an expression until it is true. Polling rather than
// waiting on a CDP event because every condition here is about the page
// having finished reacting to something, and there is no event for that.
func (b *browser) waitFor(t *testing.T, js, msg string) {
	t.Helper()
	b.waitForWith(t, 30*time.Second, js, msg)
}

func (b *browser) waitForWith(t *testing.T, limit time.Duration, js, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if b.evalBool(t, js) {
			return
		}
		if time.Now().After(deadline) {
			// The transcript is the most useful thing to see on a failure,
			// and it is the thing every condition here is about.
			txt := b.evalString(t, `document.getElementById("transcript").textContent.slice(-2000)`)
			t.Fatalf("%s\ncondition: %s\ntranscript tail:\n%s", msg, js, txt)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// typeLine puts a line in the input and presses Enter, through the page's
// own listener — the point being that this is the path a student's
// keystrokes take, not a fetch the test invented.
func (b *browser) typeLine(t *testing.T, line string) {
	t.Helper()
	b.waitFor(t, `!document.getElementById("line").disabled`, "the input never became typeable")
	quoted, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	b.eval(t, fmt.Sprintf(`(() => {
		const i = document.getElementById("line");
		i.value = %s;
		i.dispatchEvent(new KeyboardEvent("keydown", {key: "Enter", bubbles: true}));
		return true;
	})()`, quoted))
}
