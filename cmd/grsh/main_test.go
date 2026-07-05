package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles grsh once per test run.
func buildBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "grsh")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

type result struct {
	stdout, stderr string
	code           int
}

func run(t *testing.T, bin string, dir string, args ...string) result {
	return runIn(t, bin, dir, "", args...)
}

func runIn(t *testing.T, bin string, dir string, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.Stdin = strings.NewReader(stdin)
	err := cmd.Run()
	code := 0
	if xe, ok := err.(*exec.ExitError); ok {
		code = xe.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return result{so.String(), se.String(), code}
}

func writeScript(t *testing.T, dir, name, src string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(src), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCLI covers exit codes and user-facing error text end to end.
func TestCLI(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()

	t.Run("dash c", func(t *testing.T) {
		r := run(t, bin, dir, "-c", "echo cli ok")
		if r.stdout != "cli ok\n" || r.code != 0 {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("version", func(t *testing.T) {
		r := run(t, bin, dir, "-version")
		if !strings.HasPrefix(r.stdout, "grsh ") {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("no args empty stdin exits 0", func(t *testing.T) {
		r := run(t, bin, dir)
		if r.code != 0 || r.stderr != "" {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("piped stdin runs as script", func(t *testing.T) {
		r := runIn(t, bin, dir, "msg := \"from stdin\"\necho {msg}\n")
		if r.code != 0 || r.stdout != "from stdin\n" {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("piped stdin exit code propagates", func(t *testing.T) {
		r := runIn(t, bin, dir, "exit 4\n")
		if r.code != 4 {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("shell parse error exits 1 with position", func(t *testing.T) {
		p := writeScript(t, dir, "bad_shell.grsh", "echo fine\necho \"unterminated\n")
		r := run(t, bin, dir, p)
		if r.code != 1 {
			t.Errorf("code = %d, want 1 (%+v)", r.code, r)
		}
		if !strings.Contains(r.stderr, "bad_shell.grsh:2") || !strings.Contains(r.stderr, "unterminated double quote") {
			t.Errorf("stderr = %q", r.stderr)
		}
	})

	t.Run("go parse error exits 1 with position", func(t *testing.T) {
		p := writeScript(t, dir, "bad_go.grsh", "x := 1\ny := ]oops\n")
		r := run(t, bin, dir, p)
		if r.code != 1 || !strings.Contains(r.stderr, "bad_go.grsh:2") {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("runtime error exits 2 with position", func(t *testing.T) {
		p := writeScript(t, dir, "boom.grsh", "echo one\nfmt.Println(missing)\n")
		r := run(t, bin, dir, p)
		if r.code != 2 {
			t.Errorf("code = %d (%+v)", r.code, r)
		}
		if r.stdout != "one\n" {
			t.Errorf("stdout = %q", r.stdout)
		}
		if !strings.Contains(r.stderr, "boom.grsh:2") || !strings.Contains(r.stderr, "undefined: missing") {
			t.Errorf("stderr = %q", r.stderr)
		}
	})

	t.Run("script exit code propagates", func(t *testing.T) {
		p := writeScript(t, dir, "five.grsh", "exit 5\n")
		if r := run(t, bin, dir, p); r.code != 5 {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("script args", func(t *testing.T) {
		p := writeScript(t, dir, "args.grsh", "echo $1-$#\n")
		r := run(t, bin, dir, p, "hi", "there")
		if r.stdout != "hi-2\n" {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("shebang", func(t *testing.T) {
		p := writeScript(t, dir, "sb.grsh", "#!/usr/bin/env grsh\necho via shebang\n")
		cmd := exec.Command(p)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(bin)+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		if err != nil || string(out) != "via shebang\n" {
			t.Errorf("shebang: out=%q err=%v", out, err)
		}
	})

	t.Run("stack trace context with debug", func(t *testing.T) {
		p := writeScript(t, dir, "trace.grsh", "func inner() {\n    fmt.Println(nope)\n}\nfunc outer() {\n    inner()\n}\nouter()\n")
		r := run(t, bin, dir, "--debug", p)
		if r.code != 2 {
			t.Errorf("code = %d", r.code)
		}
		// serr field chain carries the script-level call trail.
		if !strings.Contains(r.stderr, "inner") || !strings.Contains(r.stderr, "outer") || !strings.Contains(r.stderr, "trace.grsh:2") {
			t.Errorf("stderr = %q", r.stderr)
		}
	})
}
