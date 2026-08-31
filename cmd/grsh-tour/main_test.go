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
	var dir string
	code, stdout, stderr := runTour(t, []string{"-addr", "127.0.0.1:0", "-open=false"}, func(base string) {
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
	if !strings.Contains(stdout, "http://127.0.0.1:0") {
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

	if _, _, _ = runTour(t, []string{"-addr", "127.0.0.1:0"}, nil); len(opened) != 1 {
		t.Fatalf("the default opened %d browser windows, want 1", len(opened))
	}
	if opened[0] != "http://127.0.0.1:0" {
		t.Errorf("browser sent to %q, not the address the tour printed", opened[0])
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
