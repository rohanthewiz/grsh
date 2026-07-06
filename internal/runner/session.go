// Package runner ties the pipeline stages together behind a Session:
//
//	source → classify → shellparse/transform → go/parser → interp → shellexec
//
// A script run is one Eval of the whole file; a future REPL calls Eval
// once per input chunk against the same Session (classifier scope, interp
// globals, and the shell side table all persist).
package runner

import (
	"errors"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"

	"github.com/rohanthewiz/grsh/internal/builtins"
	"github.com/rohanthewiz/grsh/internal/classify"
	"github.com/rohanthewiz/grsh/internal/interp"
	"github.com/rohanthewiz/grsh/internal/shellexec"
	"github.com/rohanthewiz/grsh/internal/stdlibreg"
	"github.com/rohanthewiz/grsh/internal/transform"
	"github.com/rohanthewiz/serr"
)

// Options configures a Session. Zero values inherit the process streams.
type Options struct {
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	ScriptName string
	ScriptArgs []string
	Explain    io.Writer // when set, classification decisions are printed
}

type Session struct {
	st      *shellexec.State
	stdio   shellexec.Stdio
	cls     *classify.Classifier
	in      *interp.Interp
	tabLen  int
	explain io.Writer
}

// ParseError marks a script syntax error (exit code 1 vs 2 for runtime).
type ParseError struct{ Err error }

func (e ParseError) Error() string { return e.Err.Error() }
func (e ParseError) Unwrap() error { return e.Err }

// UserMessage renders an error as a concise one-liner for the terminal:
// "script.grsh:12: <root cause>". The full serr field chain stays behind
// the --debug flag.
func UserMessage(err error) string {
	if pe, ok := errors.AsType[ParseError](err); ok {
		err = pe.Err
	}
	se := serr.WrapAsSErr(err)
	msg := se.GetError().Error()
	if loc, ok := se.GetAttribute("loc"); ok {
		return fmt.Sprintf("%v: %s", loc, msg)
	}
	return msg
}

func NewSession(o Options) *Session {
	stdio := shellexec.OSStdio()
	if o.Stdin != nil {
		stdio.In = o.Stdin
	}
	if o.Stdout != nil {
		stdio.Out = o.Stdout
	}
	if o.Stderr != nil {
		stdio.Err = o.Stderr
	}
	st := shellexec.NewState()
	st.ScriptName = o.ScriptName
	st.ScriptArgs = o.ScriptArgs

	fns := builtins.Make(st, stdio)
	cls := classify.New(stdlibreg.Names())
	for name := range fns {
		cls.Predeclare(name)
	}
	// Go builtin functions classify as Go in call position (`delete(m, k)`)
	// — but only with Go punctuation after, so `make build` stays shell.
	// iff is grsh's lazy ternary intrinsic.
	cls.Predeclare("len", "cap", "append", "delete", "copy", "make", "min", "max", "iff")
	s := &Session{
		st:      st,
		stdio:   stdio,
		cls:     cls,
		in:      interp.New(st, stdio, fns),
		explain: o.Explain,
	}
	st.SourceFn = s.RunFile
	return s
}

// LastStatus exposes the status of the last executed pipeline.
func (s *Session) LastStatus() int { return s.st.LastStatus }

// NeedsMore reports whether src is an incomplete input unit (unclosed
// block, mid-expression Go line, or trailing shell continuation). The REPL
// keeps reading lines while this is true. Classifier state is not mutated.
func (s *Session) NeedsMore(src string) bool { return s.cls.NeedsMore(src) }

// Idents lists every identifier the classifier currently knows — builtins,
// registry packages were seeded at construction, plus anything the user has
// declared since. The REPL completes on these.
func (s *Session) Idents() []string { return s.cls.Names() }

// Notifications drains "[1]  Done  cmd &" messages for finished background
// jobs; the REPL prints them before each prompt.
func (s *Session) Notifications() []string { return s.st.Jobs.Notifications() }

// SetInteractive enables job control: foreground pipelines run in their
// own process group so Ctrl+Z suspends them into the job table. Only the
// REPL turns this on.
func (s *Session) SetInteractive(on bool) { s.st.Interactive = on }

func (s *Session) RunFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return serr.Wrap(err, "op", "read script")
	}
	return s.RunSource(path, string(b))
}

// Eval runs a chunk of source (the -c flag, and the REPL seam for v2).
func (s *Session) Eval(src string) error {
	return s.RunSource("<eval>", src)
}

func (s *Session) RunSource(name, src string) error {
	chunks, err := s.cls.File(src)
	if err != nil {
		return ParseError{Err: serr.Wrap(err, "stage", "classify")}
	}
	if s.explain != nil {
		for _, ch := range chunks {
			if ch.Kind == classify.Blank {
				continue
			}
			fmt.Fprintf(s.explain, "%s:%d-%d\t%-5s\trule=%s\t%s\n",
				name, ch.StartLine, ch.EndLine, ch.Kind, ch.Rule, firstLine(ch.Text))
		}
	}

	res, err := transform.File(name, chunks, s.tabLen)
	if err != nil {
		return ParseError{Err: err}
	}

	fset := token.NewFileSet()
	astf, err := parser.ParseFile(fset, name, res.GoSrc, parser.SkipObjectResolution)
	if err != nil {
		return ParseError{Err: goParseErr(err)}
	}

	s.tabLen += len(res.Tab)
	s.in.AddTab(res.Tab)
	if err := s.in.Run(fset, astf); err != nil {
		if _, isExit := errors.AsType[shellexec.ExitErr](err); isExit {
			return err
		}
		return serr.Wrap(err, "script", name)
	}
	return nil
}

// goParseErr formats a go/parser error list; //line directives mean the
// positions already point into the .grsh source.
func goParseErr(err error) error {
	if list, ok := err.(scanner.ErrorList); ok && len(list) > 0 {
		e := list[0]
		return serr.New(e.Msg, "loc", fmt.Sprintf("%s:%d:%d", e.Pos.Filename, e.Pos.Line, e.Pos.Column))
	}
	return err
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i] + " ..."
		}
	}
	return s
}
