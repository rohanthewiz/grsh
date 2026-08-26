package interp

import (
	"bytes"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/shellexec"
)

// ---- test harness ----
//
// These tests drive the evaluator DIRECTLY: source is wrapped in the
// __main func that transform would normally emit, parsed with go/parser,
// and handed to Interp.Run. Nothing from classify or transform is in the
// loop, so a failure here is an interpreter failure and nothing else --
// which is the whole point of having them alongside the golden scripts in
// internal/runner, where every stage is implicated at once.
//
// The shell legs (__shell/__capture) are deliberately NOT exercised here;
// they need a populated side table and a real process, and they already
// have coverage in internal/shellexec and the golden suite.

// newTestInterp builds an interpreter whose script output lands in out.
// extra is merged into the global scope as Go functions, which is how the
// reflect call boundary gets driven with signatures a test controls.
func newTestInterp(out *bytes.Buffer, extra map[string]any) *Interp {
	st := shellexec.NewState()
	stdio := shellexec.Stdio{In: strings.NewReader(""), Out: out, Err: out}
	st.CaptureErr = out
	return New(st, stdio, extra)
}

// eval runs body as a script and returns everything it printed.
// A run error is returned, not fataled: many cases assert on the error.
func eval(t *testing.T, body string, extra map[string]any) (string, error) {
	t.Helper()
	var out bytes.Buffer
	in := newTestInterp(&out, extra)
	src := "package main\n\nfunc __main() {\n" + body + "\n}\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "t.grsh", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("harness: the test source is not valid Go: %v\n%s", err, src)
	}
	runErr := in.Run(fset, f)
	return out.String(), runErr
}

// evalKeep is eval plus the interpreter itself, for the cases that
// inspect global state after the run rather than only its output.
func evalKeep(t *testing.T, body string, extra map[string]any) (*Interp, string, error) {
	t.Helper()
	var out bytes.Buffer
	in := newTestInterp(&out, extra)
	src := "package main\n\nfunc __main() {\n" + body + "\n}\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "t.grsh", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("harness: the test source is not valid Go: %v\n%s", err, src)
	}
	runErr := in.Run(fset, f)
	return in, out.String(), runErr
}

// mustEval fails the test on a run error; for cases that only care about
// the printed result.
func mustEval(t *testing.T, body string) string {
	t.Helper()
	out, err := eval(t, body, nil)
	if err != nil {
		t.Fatalf("run: %v\noutput so far:\n%s", err, out)
	}
	return out
}

// wantOut is the common assertion: run and compare printed output.
func wantOut(t *testing.T, body, want string) {
	t.Helper()
	if got := mustEval(t, body); got != want {
		t.Errorf("got %q, want %q\nsource:\n%s", got, want, body)
	}
}

// wantErr asserts the run fails and the message contains substr. An empty
// substr only requires failure.
func wantErr(t *testing.T, body, substr string) {
	t.Helper()
	out, err := eval(t, body, nil)
	if err == nil {
		t.Fatalf("expected an error, got none\noutput: %q\nsource:\n%s", out, body)
	}
	if substr != "" && !strings.Contains(err.Error(), substr) {
		t.Errorf("error %q does not contain %q\nsource:\n%s", err.Error(), substr, body)
	}
}

func TestHarnessSmoke(t *testing.T) {
	wantOut(t, `fmt.Println("hi", 1+2)`, "hi 3\n")
}
