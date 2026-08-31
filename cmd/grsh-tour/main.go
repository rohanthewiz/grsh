// Command grsh-tour serves the grsh tutorial in a browser.
//
// It is a second binary rather than a `grsh tour` subcommand on purpose.
// The tour needs an HTTP server and an HTML page; the shell needs neither,
// and reaching them from grsh's main would link a web framework into every
// copy of the shell for the benefit of the one session in a thousand that
// takes the tour. The lesson engine is shared — internal/tutor — and only
// the transport differs.
//
//	grsh-tour                      # http://127.0.0.1:7654, opens a browser
//	grsh-tour -addr 127.0.0.1:9000
//	grsh-tour -progress            # remember where you got to
//
// The server runs shell commands as the user who started it. It binds to
// loopback and refuses anything else unless told twice; see -allow-remote.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/rohanthewiz/grsh/internal/tour"
	"github.com/rohanthewiz/grsh/internal/tutor"
)

const version = "0.2.0-dev"

// Exit codes, named because the tests assert them: a tour that reported a
// refused bind as success would be a bad thing to script against.
const (
	exitOK    = 0
	exitServe = 1 // the listener failed while running
	exitUsage = 2 // bad flags, or an address we will not bind
)

// The three things main does that a test cannot let it do, in variables so
// a test can stand in for them.
//
// This is the only reason main is not one function: serve blocks until a
// signal arrives, openStore takes the developer's own ~/.grsh_tutor.db,
// and launch opens a browser window on the machine running the tests. What
// is left — the flags, the wiring, and above all the shutdown order — is
// the part worth pinning, and it is now reachable.
var (
	serve     = func(s *tour.Server, ready <-chan struct{}) error { return s.Run() }
	openStore = func() tutor.Store { return tutor.OpenStore() }
	launch    = browse
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is main with its edges injected. It returns the process's exit code
// rather than calling os.Exit, so that the teardown below always happens.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("grsh-tour", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		addr        = fs.String("addr", "127.0.0.1:7654", "listen address")
		allowRemote = fs.Bool("allow-remote", false,
			"permit a non-loopback bind — the tour runs shell commands as you, so mean it")
		openBrowser = fs.Bool("open", true, "open a browser when the server starts")
		progress    = fs.Bool("progress", false,
			"remember chapter progress in ~/.grsh_tutor.db (holds the file open, so `grsh tutor` cannot)")
		idle    = fs.Duration("idle", 30*time.Minute, "reclaim a playground after this long without a request")
		verbose = fs.Bool("v", false, "log requests")
		showVer = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(args); err != nil {
		// -h is a request that was answered, not a mistake.
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *showVer {
		fmt.Fprintln(stdout, "grsh-tour "+version)
		return exitOK
	}

	// Ready is threaded through even in production, where nothing reads
	// it: a listener on port 0 is not knowable until it exists, and the
	// alternative for the test that needs the port is polling, which is a
	// race dressed up as a retry.
	ready := make(chan struct{}, 1)
	opts := tour.Options{
		Addr:        *addr,
		AllowRemote: *allowRemote,
		IdleTimeout: *idle,
		Verbose:     *verbose,
		Ready:       ready,
	}
	var store tutor.Store
	if *progress {
		store = openStore()
		opts.Store = store
	}

	srv, err := tour.New(opts)
	if err != nil {
		// The loopback refusal lands here. Closing the store first: New
		// failing means nothing will ever call the teardown below.
		if store != nil {
			_ = store.Close()
		}
		fmt.Fprintf(stderr, "grsh-tour: %v\n", err)
		return exitUsage
	}

	url := "http://" + *addr
	fmt.Fprintf(stdout, "grsh tour · %s\n%s\n", version, url)
	fmt.Fprintln(stdout, "Each browser tab gets its own playground. Ctrl+C ends the tour and removes them.")
	if *openBrowser {
		launch(url)
	}

	// rweb's Run installs its own SIGINT/SIGTERM handler and returns when
	// one arrives, so shutdown here is "Run returned" rather than a signal
	// this program watches for itself. Registering a second handler seems
	// like the obvious thing and is wrong: Go delivers the signal to every
	// registered channel, so the two race — and ours loses, because Run
	// returns and main falls off the end while the handler is still
	// tearing down. The playgrounds survive the process that made them.
	runErr := serve(srv, ready)

	// Teardown runs whatever ended the server. Every visitor holds a
	// directory in $TMPDIR and a live shell session, and neither a signal
	// nor a failed listener removes them on its own.
	fmt.Fprintln(stderr, "\ngrsh-tour: closing playgrounds…")
	srv.Close()
	if store != nil {
		_ = store.Close()
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "grsh-tour: %v\n", runErr)
		return exitServe
	}
	return exitOK
}

// browse opens the page in the user's browser, and shrugs if it cannot —
// the address has already been printed, and a tour that refused to start
// because it could not find a browser would be absurd.
func browse(url string) {
	cmd := browseCmd(url)
	// Started, not run: the launcher may outlive us on some desktops, and
	// waiting on it would hold the tour's startup hostage to a browser
	// that takes its time.
	_ = cmd.Start()
}

// browseCmd is the per-platform half, split out so it can be checked
// without opening a window on the machine running the tests.
func browseCmd(url string) *exec.Cmd {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url)
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return exec.Command("xdg-open", url)
	}
}
