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
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/rohanthewiz/grsh/internal/tour"
	"github.com/rohanthewiz/grsh/internal/tutor"
)

const version = "0.2.0-dev"

func main() {
	var (
		addr        = flag.String("addr", "127.0.0.1:7654", "listen address")
		allowRemote = flag.Bool("allow-remote", false,
			"permit a non-loopback bind — the tour runs shell commands as you, so mean it")
		openBrowser = flag.Bool("open", true, "open a browser when the server starts")
		progress    = flag.Bool("progress", false,
			"remember chapter progress in ~/.grsh_tutor.db (holds the file open, so `grsh tutor` cannot)")
		idle    = flag.Duration("idle", 30*time.Minute, "reclaim a playground after this long without a request")
		verbose = flag.Bool("v", false, "log requests")
		showVer = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("grsh-tour " + version)
		return
	}

	opts := tour.Options{
		Addr:        *addr,
		AllowRemote: *allowRemote,
		IdleTimeout: *idle,
		Verbose:     *verbose,
	}
	var store tutor.Store
	if *progress {
		store = tutor.OpenStore()
		opts.Store = store
	}

	srv, err := tour.New(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grsh-tour: %v\n", err)
		os.Exit(2)
	}

	url := "http://" + *addr
	fmt.Printf("grsh tour · %s\n%s\n", version, url)
	fmt.Println("Each browser tab gets its own playground. Ctrl+C ends the tour and removes them.")
	if *openBrowser {
		browse(url)
	}

	// rweb's Run installs its own SIGINT/SIGTERM handler and returns when
	// one arrives, so shutdown here is "Run returned" rather than a signal
	// this program watches for itself. Registering a second handler seems
	// like the obvious thing and is wrong: Go delivers the signal to every
	// registered channel, so the two race — and ours loses, because Run
	// returns and main falls off the end while the handler is still
	// tearing down. The playgrounds survive the process that made them.
	runErr := srv.Run()

	// Teardown runs whatever ended the server. Every visitor holds a
	// directory in $TMPDIR and a live shell session, and neither a signal
	// nor a failed listener removes them on its own.
	fmt.Fprintln(os.Stderr, "\ngrsh-tour: closing playgrounds…")
	srv.Close()
	if store != nil {
		_ = store.Close()
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "grsh-tour: %v\n", runErr)
		os.Exit(1)
	}
}

// browse opens the page in the user's browser, and shrugs if it cannot —
// the address has already been printed, and a tour that refused to start
// because it could not find a browser would be absurd.
func browse(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Started, not run: the launcher may outlive us on some desktops, and
	// waiting on it would hold the tour's startup hostage to a browser
	// that takes its time.
	_ = cmd.Start()
}
