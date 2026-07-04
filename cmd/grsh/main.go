// grsh is a Go-powered shell scripting language.
//
//	grsh script.grsh [args...]
//	grsh -c "ls | wc -l"
//
// Scripts may start with a shebang: #!/usr/bin/env grsh
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/rohanthewiz/grsh/internal/runner"
	"github.com/rohanthewiz/grsh/internal/shellexec"
	"github.com/rohanthewiz/logger"
)

const version = "0.1.0-dev"

func main() {
	var (
		flagC       = flag.String("c", "", "run this command string and exit")
		flagVersion = flag.Bool("version", false, "print version and exit")
		flagDebug   = flag.Bool("debug", false, "verbose error output")
		flagExplain = flag.Bool("explain", false, "print each line's shell/Go classification and the rule that decided it")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: grsh [flags] script.grsh [args...]\n   or: grsh -c \"commands\"\n\nFlags:\n")
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
	default:
		flag.Usage()
		os.Exit(2)
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
