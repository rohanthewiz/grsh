package repl

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/chzyer/readline"
	"github.com/rohanthewiz/grsh/internal/runner"
)

type step struct {
	line string
	err  error
}

// fakeReader scripts a Readline sequence and records the prompts shown.
type fakeReader struct {
	steps   []step
	prompts []string
}

func (f *fakeReader) Readline() (string, error) {
	if len(f.steps) == 0 {
		return "", io.EOF
	}
	s := f.steps[0]
	f.steps = f.steps[1:]
	return s.line, s.err
}

func (f *fakeReader) SetPrompt(p string) { f.prompts = append(f.prompts, p) }

func run(t *testing.T, steps ...step) (stdout, stderr string, code int, prompts []string) {
	t.Helper()
	var out, errB bytes.Buffer
	sess := runner.NewSession(runner.Options{Stdout: &out, Stderr: &errB, ScriptName: "repl"})
	rd := &fakeReader{steps: steps}
	code = loop(sess, rd, &errB)
	return out.String(), errB.String(), code, rd.prompts
}

func TestLoopMultiLineBlock(t *testing.T) {
	stdout, stderr, code, prompts := run(t,
		step{line: "x := 41"},
		step{line: "if x > 40 {"},
		step{line: `fmt.Println("big")`},
		step{line: "}"},
	)
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if stdout != "big\n" {
		t.Errorf("stdout %q, want %q", stdout, "big\n")
	}
	// Prompts shown before lines 3 and 4 must be continuations.
	for i, p := range prompts {
		wantCont := i == 2 || i == 3
		if isCont := strings.Contains(p, "..."); isCont != wantCont {
			t.Errorf("prompt %d = %q, continuation = %v, want %v", i, p, isCont, wantCont)
		}
	}
}

func TestLoopShellContinuation(t *testing.T) {
	stdout, _, code, _ := run(t,
		step{line: "echo one |"},
		step{line: "wc -l"},
	)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if strings.TrimSpace(stdout) != "1" {
		t.Errorf("stdout %q, want 1", stdout)
	}
}

func TestLoopExitBuiltin(t *testing.T) {
	_, _, code, _ := run(t, step{line: "exit 3"})
	if code != 3 {
		t.Errorf("exit code %d, want 3", code)
	}
}

func TestLoopEOFReturnsLastStatus(t *testing.T) {
	_, _, code, _ := run(t, step{line: "false"})
	if code != 1 {
		t.Errorf("exit code %d, want last status 1", code)
	}
}

func TestLoopRuntimeErrorContinues(t *testing.T) {
	stdout, stderr, code, _ := run(t,
		step{line: "fmt.Println(unknownIdent)"},
		step{line: `fmt.Println("still here")`},
	)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stderr, "undefined") {
		t.Errorf("stderr %q, want undefined-identifier error", stderr)
	}
	if strings.Contains(stderr, "<eval>") || strings.Contains(stderr, "line 1:") {
		t.Errorf("stderr %q leaks the eval location on a single-line input", stderr)
	}
	if stdout != "still here\n" {
		t.Errorf("stdout %q — loop did not continue after the error", stdout)
	}
}

func TestLoopInterruptDropsContinuation(t *testing.T) {
	stdout, _, code, _ := run(t,
		step{line: "if true {"},
		step{line: "", err: readline.ErrInterrupt},
		step{line: `fmt.Println("fresh")`},
	)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if stdout != "fresh\n" {
		t.Errorf("stdout %q — ^C should abandon the open block", stdout)
	}
}

func TestLoopEOFMidContinuationAbandons(t *testing.T) {
	stdout, _, code, _ := run(t,
		step{line: "if true {"},
		step{line: "", err: io.EOF},
		step{line: `fmt.Println("after")`},
	)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if stdout != "after\n" {
		t.Errorf("stdout %q — ^D mid-block should abandon it, not exit", stdout)
	}
}

func TestLoopStatePersistsAcrossInputs(t *testing.T) {
	stdout, stderr, code, _ := run(t,
		step{line: "n := 2"},
		step{line: "n++"},
		step{line: "fmt.Println(n)"},
	)
	if code != 0 {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}
	if stdout != "3\n" {
		t.Errorf("stdout %q, want 3", stdout)
	}
}
