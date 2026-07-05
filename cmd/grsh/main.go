// grsh is a Go-powered shell scripting language.
//
//	grsh                     interactive REPL (when stdin is a terminal)
//	grsh script.grsh [args...]
//	grsh -c "ls | wc -l"
//	echo "ls" | grsh         run a script from stdin
//
// Scripts may start with a shebang: #!/usr/bin/env grsh
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/rohanthewiz/grsh/internal/repl"
	"github.com/rohanthewiz/grsh/internal/runner"
	"github.com/rohanthewiz/grsh/internal/shellexec"
	"github.com/rohanthewiz/logger"
	"golang.org/x/term"
)

const version = "0.2.0-dev"

func main() {
	var (
		flagC       = flag.String("c", "", "run this command string and exit")
		flagVersion = flag.Bool("version", false, "print version and exit")
		flagDebug   = flag.Bool("debug", false, "verbose error output")
		flagExplain = flag.Bool("explain", false, "print each line's shell/Go classification and the rule that decided it")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: grsh [flags] script.grsh [args...]\n   or: grsh -c \"commands\"\n   or: grsh                (interactive; reads stdin when piped)\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *flagVersion {
		fmt.Println("grsh " + version)
		return
	}
	debug := *flagDebug || os.Getenv("GRSH_DEBUG") != ""
	var explain io.Writer
	if *flagExplain {
		explain = os.Stderr
	}

	var err error
	switch {
	case *flagC != "":
		sess := runner.NewSession(runner.Options{ScriptName: "grsh", ScriptArgs: flag.Args(), Explain: explain})
		err = sess.Eval(*flagC)
	case flag.NArg() > 0:
		script := flag.Arg(0)
		sess := runner.NewSession(runner.Options{ScriptName: script, ScriptArgs: flag.Args()[1:], Explain: explain})
		err = sess.RunFile(script)
	case term.IsTerminal(int(os.Stdin.Fd())):
		sess := runner.NewSession(runner.Options{ScriptName: "grsh", Explain: explain})
		os.Exit(repl.Run(sess, version))
	default:
		// Piped stdin: run it as a script (echo "ls" | grsh).
		src, rerr := io.ReadAll(os.Stdin)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "grsh: %v\n", rerr)
			os.Exit(2)
		}
		sess := runner.NewSession(runner.Options{ScriptName: "<stdin>", Explain: explain})
		err = sess.RunSource("<stdin>", string(src))
	}

	if err == nil {
		return
	}
	if xe, ok := errors.AsType[shellexec.ExitErr](err); ok {
		os.Exit(xe.Code)
	}
	if debug {
		logger.LogErr(err)
	} else {
		fmt.Fprintf(os.Stderr, "grsh: %s\n", runner.UserMessage(err))
	}
	if _, ok := errors.AsType[runner.ParseError](err); ok {
		os.Exit(1)
	}
	os.Exit(2)
}
