package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rohanthewiz/grsh/internal/tour"
	"github.com/rohanthewiz/grsh/internal/tutor"
)

// This file exists because `main` was the one place in the tour with no
// test at all, and it is not the trivial wiring that description suggests:
// it owns the shutdown order, and getting that wrong leaks a playground
// directory and a live shell session on every Ctrl+C. The seams in main.go
// (serve, openStore, launch) are there for these tests and nothing else.

// runTour calls run with the serve hook replaced by one that starts the
// real listener, hands fn its base URL, and then returns — which is what a
// signal does to a real tour. Everything after serve in run() therefore
// happens for real, which is the half under test.
func runTour(t *testing.T, args []string, fn func(base string)) (code int, stdout, stderr string) {
	t.Helper()
	defer swap(t, &serve, func(s *tour.Server, ready <-chan struct{}) error {
		errc := make(chan error, 1)
		go func() { errc <- s.Run() }()
		select {
		case <-ready:
		case err := <-errc:
			return err
		case <-time.After(10 * time.Second):
			return errors.New("server never became ready")
		}
		if fn != nil {
			fn("http://127.0.0.1:" + s.Port())
		}
		return nil
	})()

	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// swap sets *p to v and returns the restore. A generic because the three
// hooks have three different types and a test that forgot to restore one
// would corrupt every test after it.
func swap[T any](t *testing.T, p *T, v T) func() {
	t.Helper()
	prev := *p
	*p = v
	return func() { *p = prev }
}

// noBrowser is the default for every test here: nothing may open a window
// on the machine running them.
func noBrowser(t *testing.T) { t.Cleanup(swap(t, &launch, func(string) {})) }

// TestRunClosesPlaygroundsWhenTheServerStops is the reason this file
// exists.
//
// A visitor holds a directory in $TMPDIR and a running shell session, and
// nothing reclaims either on its own — not the signal that ends the tour,
// and not the process exiting. tour's own TestServerCloseRemovesPlaygrounds
// proves Close does the work; this proves main actually calls it, on the
// path a real Ctrl+C takes, and that it does so BEFORE returning an exit
// code rather than after falling off the end of a function.
func TestRunClosesPlaygroundsWhenTheServerStops(t *testing.T) {
	noBrowser(t)
	var dir, base string
	code, stdout, stderr := runTour(t, []string{"-addr", "127.0.0.1:0", "-open=false"}, func(u string) {
		base = u
		dir = visitAndReportPlayground(t, base)
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("playground %s missing while the tour is running: %v", dir, err)
		}
	})

	if code != exitOK {
		t.Errorf("exit code %d, want %d\nstderr: %s", code, exitOK, stderr)
	}
	if dir == "" {
		t.Fatal("no playground was ever created — the test proved nothing")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("playground %s outlived the process", dir)
	}
	if !strings.Contains(stdout, base) {
		t.Errorf("the address was not printed for the user:\n%s", stdout)
	}
	if !strings.Contains(stderr, "closing playgrounds") {
		t.Errorf("shutdown was silent:\n%s", stderr)
	}
}

// TestRunReportsAFailedListener: a listener that dies is an exit code, and
// the playgrounds still go. The teardown must not be conditional on
// success — the case where the server failed is exactly the case where
// nothing else will clean up.
func TestRunReportsAFailedListener(t *testing.T) {
	noBrowser(t)
	defer swap(t, &serve, func(s *tour.Server, ready <-chan struct{}) error {
		return errors.New("address already in use")
	})()

	var out, errOut bytes.Buffer
	if code := run([]string{"-addr", "127.0.0.1:0", "-open=false"}, &out, &errOut); code != exitServe {
		t.Errorf("exit code %d, want %d", code, exitServe)
	}
	if !strings.Contains(errOut.String(), "address already in use") {
		t.Errorf("the listener's error was swallowed:\n%s", errOut.String())
	}
}

// TestRunRefusesANonLoopbackBind: the guard that stands between this tool
// and a shell on the network is tour's, but the exit code and the message
// are main's, and a wrapper script has only those to go on.
func TestRunRefusesANonLoopbackBind(t *testing.T) {
	noBrowser(t)
	served := false
	defer swap(t, &serve, func(s *tour.Server, ready <-chan struct{}) error {
		served = true
		return nil
	})()

	var out, errOut bytes.Buffer
	code := run([]string{"-addr", ":7654"}, &out, &errOut)
	if code != exitUsage {
		t.Errorf("exit code %d, want %d", code, exitUsage)
	}
	if served {
		t.Error("a refused address still reached the listener")
	}
	if !strings.Contains(errOut.String(), "allow-remote") {
		t.Errorf("the refusal does not name the way past it:\n%s", errOut.String())
	}

	// And the escape hatch really is one. Port 0 so the test binds nothing
	// anyone else wants; the point is that New accepted it.
	code, _, _ = runTour(t, []string{"-addr", "0.0.0.0:0", "-allow-remote", "-open=false"}, nil)
	if code != exitOK {
		t.Errorf("-allow-remote still refused: exit %d", code)
	}
}

// TestRunFlagHandling covers the three ways run ends before it serves
// anything. Table-driven because they differ only in argument and code.
func TestRunFlagHandling(t *testing.T) {
	noBrowser(t)
	for _, tc := range []struct {
		name  string
		args  []string
		want  int
		inOut string // expected on stdout
		inErr string // expected on stderr
	}{
		{"version", []string{"-version"}, exitOK, "grsh-tour " + version, ""},
		{"help", []string{"-h"}, exitOK, "", "-allow-remote"},
		{"unknown flag", []string{"-teleport"}, exitUsage, "", "teleport"},
		{"bad duration", []string{"-idle", "soon"}, exitUsage, "", "idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := false
			defer swap(t, &serve, func(s *tour.Server, ready <-chan struct{}) error {
				served = true
				return nil
			})()

			var out, errOut bytes.Buffer
			if code := run(tc.args, &out, &errOut); code != tc.want {
				t.Errorf("exit code %d, want %d (stderr: %s)", code, tc.want, errOut.String())
			}
			if served {
				t.Errorf("%v started a server", tc.args)
			}
			if tc.inOut != "" && !strings.Contains(out.String(), tc.inOut) {
				t.Errorf("stdout %q does not contain %q", out.String(), tc.inOut)
			}
			if tc.inErr != "" && !strings.Contains(errOut.String(), tc.inErr) {
				t.Errorf("stderr %q does not contain %q", errOut.String(), tc.inErr)
			}
		})
	}
}

// TestRunOpensTheBrowserOnce: -open is on by default, and off means off.
// Worth a test because the default is the one flag whose value most users
// never see and every user notices.
func TestRunOpensTheBrowserOnce(t *testing.T) {
	var opened []string
	defer swap(t, &launch, func(u string) { opened = append(opened, u) })()

	var base string
	_, _, _ = runTour(t, []string{"-addr", "127.0.0.1:0"}, func(u string) { base = u })
	if len(opened) != 1 {
		t.Fatalf("the default opened %d browser windows, want 1", len(opened))
	}
	// The listening address, not the requested one. A browser sent to
	// port 0 lands nowhere.
	if opened[0] != base {
		t.Errorf("browser sent to %q, want %q", opened[0], base)
	}

	opened = nil
	runTour(t, []string{"-addr", "127.0.0.1:0", "-open=false"}, nil)
	if len(opened) != 0 {
		t.Errorf("-open=false opened %v", opened)
	}
}

// TestBrowseCmd checks the launcher for the platform the tests are running
// on. Shallow on purpose: the only failure this can catch is a typo in a
// program name or a dropped URL, and those are exactly the failures nobody
// notices until a user on that platform reports a tour that never opens.
func TestBrowseCmd(t *testing.T) {
	cmd := browseCmd("http://127.0.0.1:7654")
	want := map[string]string{"darwin": "open", "windows": "rundll32"}[runtime.GOOS]
	if want == "" {
		want = "xdg-open"
	}
	if got := cmd.Args[0]; got != want {
		t.Errorf("browseCmd uses %q on %s, want %q", got, runtime.GOOS, want)
	}
	if got := cmd.Args[len(cmd.Args)-1]; got != "http://127.0.0.1:7654" {
		t.Errorf("browseCmd dropped the URL: %v", cmd.Args)
	}
}

// TestRunStoreLifecycle: -progress is the only flag that takes a lock on a
// file outside the playground, so it is the only one whose cleanup can
// strand something. It must be opened only when asked, and closed on both
// the way out and the early exit.
func TestRunStoreLifecycle(t *testing.T) {
	noBrowser(t)
	var st *fakeStore
	defer swap(t, &openStore, func() tutor.Store { st = &fakeStore{}; return st })()

	runTour(t, []string{"-addr", "127.0.0.1:0", "-open=false"}, nil)
	if st != nil {
		t.Error("the progress database was opened without -progress")
	}

	runTour(t, []string{"-addr", "127.0.0.1:0", "-open=false", "-progress"}, nil)
	if st == nil {
		t.Fatal("-progress did not open a store")
	}
	if !st.closed() {
		t.Error("-progress left ~/.grsh_tutor.db open; `grsh tutor` could not use it afterwards")
	}

	// The early exit too: New refusing an address must not strand the file
	// it opened a moment earlier.
	st = nil
	var out, errOut bytes.Buffer
	run([]string{"-addr", ":7654", "-progress"}, &out, &errOut)
	if st == nil {
		t.Fatal("-progress did not open a store")
	}
	if !st.closed() {
		t.Error("a refused address left the progress database open")
	}
}

// TestRunPrintsThePortTheKernelPicked: `-addr host:0` means "any free
// port", and the port is not decided until the listener binds. Printing
// *addr — which is what run used to do, before it served — handed the
// user "http://127.0.0.1:0" and the real port went nowhere.
//
// This also pins the ordering that makes the fix possible: the URL is
// printed after rweb signals readiness, and run does not return until
// that has happened.
func TestRunPrintsThePortTheKernelPicked(t *testing.T) {
	noBrowser(t)
	var base string
	_, stdout, _ := runTour(t, []string{"-addr", "127.0.0.1:0", "-open=false"}, func(u string) { base = u })

	if base == "" || strings.HasSuffix(base, ":0") {
		t.Fatalf("the listener never took a real port (%q) — the test proved nothing", base)
	}
	if !strings.Contains(stdout, base) {
		t.Errorf("stdout does not name the listening address %s:\n%s", base, stdout)
	}
	if strings.Contains(stdout, "http://127.0.0.1:0") {
		t.Errorf("the requested port 0 was printed as if it were a place:\n%s", stdout)
	}
}

// TestRunSaysNothingWhenTheListenerNeverStarts: the announcement is now
// on its own goroutine waiting for readiness, and a server that never
// becomes ready must not leave it hanging or print a URL that never
// worked. run has to join it either way.
func TestRunSaysNothingWhenTheListenerNeverStarts(t *testing.T) {
	noBrowser(t)
	defer swap(t, &serve, func(s *tour.Server, ready <-chan struct{}) error {
		return errors.New("address already in use")
	})()

	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run([]string{"-addr", "127.0.0.1:0", "-open=false"}, &out, &errOut) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run never returned — the announcement goroutine was not joined")
	}
	if strings.Contains(out.String(), "http://") {
		t.Errorf("a URL was printed for a server that never listened:\n%s", out.String())
	}
}

// TestDisplayURL covers the shapes of -addr separately from the server,
// because binding each of them in a test would mean binding a real port
// on every interface of the machine running it.
func TestDisplayURL(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		port      string // what the listener took
		want      string
	}{
		{"the usual case, unchanged", "127.0.0.1:7654", "7654", "http://127.0.0.1:7654"},
		{"port 0 takes the real port", "127.0.0.1:0", "51234", "http://127.0.0.1:51234"},
		{"a wildcard host is a place you can go", ":7654", "7654", "http://127.0.0.1:7654"},
		{"an IPv6 literal keeps its brackets", "[::1]:0", "51234", "http://[::1]:51234"},
		{"no port from the listener falls back", "127.0.0.1:7654", "", "http://127.0.0.1:7654"},
		{"not a host:port at all", "localhost:", "", "http://localhost:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayURL(tc.requested, tc.port); got != tc.want {
				t.Errorf("displayURL(%q, %q) = %q, want %q", tc.requested, tc.port, got, tc.want)
			}
		})
	}
}

// ── helpers ──────────────────────────────────────────────────────

// visitAndReportPlayground behaves as a browser's first visit does — the
// page mints the session — and reads back the directory it was given.
func visitAndReportPlayground(t *testing.T, base string) string {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	res, err := c.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	res.Body.Close()

	res, err = c.Get(base + "/state")
	if err != nil {
		t.Fatalf("GET /state: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	var v tutor.View
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode /state: %v (%s)", err, body)
	}
	if v.Dir == "" {
		t.Fatal("/state reported no playground")
	}
	return v.Dir
}

// fakeStore stands in for the progress database. Locked because run closes
// it on the goroutine the test is watching from.
type fakeStore struct {
	mu   sync.Mutex
	shut bool
}

func (f *fakeStore) Load(string) (tutor.Record, bool) { return tutor.Record{}, false }
func (f *fakeStore) Save(tutor.Record) error          { return nil }

func (f *fakeStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shut = true
	return nil
}

func (f *fakeStore) closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shut
}
