package runner

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/shellexec"
)

var update = flag.Bool("update", false, "rewrite golden .want files")

// TestGolden runs every testdata/scripts/*.grsh in a fresh temp dir and
// compares combined output against the matching .want file. An optional
// .exit file holds the expected exit code (default 0).
func TestGolden(t *testing.T) {
	dir, err := filepath.Abs("../../testdata/scripts")
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := filepath.Glob(filepath.Join(dir, "*.grsh"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no golden scripts found in %s (err=%v)", dir, err)
	}

	for _, script := range scripts {
		name := strings.TrimSuffix(filepath.Base(script), ".grsh")
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			// Optional .args file supplies script arguments.
			var args []string
			if b, err := os.ReadFile(strings.TrimSuffix(script, ".grsh") + ".args"); err == nil {
				args = strings.Fields(string(b))
			}

			var out bytes.Buffer
			sess := NewSession(Options{
				Stdin:      strings.NewReader(""),
				Stdout:     &out,
				Stderr:     &out,
				ScriptName: script,
				ScriptArgs: args,
			})
			runErr := sess.RunFile(script)

			gotExit := 0
			if runErr != nil {
				if xe, ok := errors.AsType[shellexec.ExitErr](runErr); ok {
					gotExit = xe.Code
				} else {
					t.Fatalf("run error: %v\noutput so far:\n%s", runErr, out.String())
				}
			}

			wantExit := 0
			if b, err := os.ReadFile(strings.TrimSuffix(script, ".grsh") + ".exit"); err == nil {
				wantExit, err = strconv.Atoi(strings.TrimSpace(string(b)))
				if err != nil {
					t.Fatalf("bad .exit file: %v", err)
				}
			}
			if gotExit != wantExit {
				t.Errorf("exit code = %d, want %d", gotExit, wantExit)
			}

			wantFile := strings.TrimSuffix(script, ".grsh") + ".want"
			if *update {
				if err := os.WriteFile(wantFile, out.Bytes(), 0644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(wantFile)
			if err != nil {
				t.Fatalf("missing golden file (run with -update to create): %v", err)
			}
			if out.String() != string(want) {
				t.Errorf("output mismatch\n--- got ---\n%s--- want ---\n%s", out.String(), want)
			}
		})
	}
}
