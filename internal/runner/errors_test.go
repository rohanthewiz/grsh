package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func evalErr(t *testing.T, src string) error {
	t.Helper()
	var out bytes.Buffer
	sess := NewSession(Options{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
	return sess.RunSource("err.grsh", src)
}

// TestErrorPositions is the de-risk gate for the //line strategy: runtime
// and parse errors must point at the correct .grsh line.
func TestErrorPositions(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantLoc string
		wantMsg string
	}{
		{
			"runtime undefined variable",
			"echo one\nx := undefinedVar + 1\n",
			"err.grsh:2", "undefined: undefinedVar",
		},
		{
			"undefined after shell lines",
			"echo one\necho two\necho three\nfmt.Println(mystery)\n",
			"err.grsh:4", "undefined: mystery",
		},
		{
			"undefined on assignment RHS",
			"x := 1\n\nx = missingRHS\n",
			"err.grsh:3", "undefined: missingRHS",
		},
		{
			"division by zero deep in a block",
			"for i := 0; i < 3; i++ {\n    if i == 2 {\n        x := 1 / (i - 2)\n        fmt.Println(x)\n    }\n}\n",
			"err.grsh:3", "division by zero",
		},
		{
			"go parse error",
			"echo hi\nx := ]bad\n",
			"err.grsh:2", "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := evalErr(t, tc.src)
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := UserMessage(err)
			full := err.Error()
			if !strings.Contains(full, tc.wantLoc) && !strings.Contains(msg, tc.wantLoc) {
				t.Errorf("error %q (user msg %q) does not mention %q", full, msg, tc.wantLoc)
			}
			if tc.wantMsg != "" && !strings.Contains(full, tc.wantMsg) {
				t.Errorf("error %q does not mention %q", full, tc.wantMsg)
			}
		})
	}
}

// TestErrorPositionsSurviveASource extends the //line gate across a
// re-entrant run. `source` inside a script calls back into the SAME
// interpreter (shellexec's SourceFn -> Session.RunFile -> Interp.Run), so
// the sourced unit installs its own fileset over the caller's. Before Run
// saved and restored that state, everything after a source resolved
// positions against a file its nodes did not come from and reported
// `loc[:0:1]` -- for the rest of the script, and for every kind of error,
// not just the fragment case that exposed it.
//
// The sub-file's own errors are checked in the same test: a fix that
// simply refused to install the nested fileset would satisfy the first
// half and break this one.
func TestErrorPositionsSurviveASource(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ok := write("ok.grsh", "echo in the sourced file\n")

	tests := []struct {
		name    string
		src     string
		wantLoc string
	}{
		{
			// The Go leg: errAt reads the fileset directly.
			"go error after a source",
			"x := 0\nsource " + ok + "\nfmt.Println(1 / x)\n",
			"outer.grsh:3",
		},
		{
			// The shell leg: a {expr} fragment is parsed INTO the fileset
			// and its line info remapped from the enclosing node's position,
			// so a stale fset corrupts the fragment as well as the report.
			"interpolation after a source",
			"x := 0\nsource " + ok + "\necho \"{1 / x}\"\n",
			"outer.grsh:3",
		},
		{
			// Two levels deep: the restore has to be per-run, not a single
			// remembered original.
			"nested sources",
			"x := 0\nsource " + write("mid.grsh", "source "+ok+"\n") + "\nfmt.Println(1 / x)\n",
			"outer.grsh:3",
		},
		{
			// ...and the sourced file still reports its own lines while it
			// is the one running.
			"error inside the sourced file",
			"source " + write("bad.grsh", "echo fine\ny := 0\nfmt.Println(2 / y)\n") + "\n",
			"bad.grsh:3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			sess := NewSession(Options{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
			err := sess.RunSource("outer.grsh", tc.src)

			// The two legs report through different surfaces: a Go error
			// comes back from RunSource, while a {expr} that fails during
			// word expansion is printed and sets a status, as a failing
			// command does. Which one carried it is not what this test is
			// about -- the position is -- so both are gathered.
			report := out.String()
			if err != nil {
				report += err.Error() + "\n" + UserMessage(err)
			}
			if !strings.Contains(report, "division by zero") {
				t.Fatalf("the division was expected to fail; got %q", report)
			}
			if !strings.Contains(report, tc.wantLoc) {
				t.Errorf("reported as %q, want a position in %q", report, tc.wantLoc)
			}
		})
	}
}
