package repl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rohanthewiz/grsh/internal/runner"
)

func TestRenderPrompt(t *testing.T) {
	var out bytes.Buffer
	sess := runner.NewSession(runner.Options{Stdout: &out, Stderr: &out})
	now := time.Date(2026, 8, 24, 9, 30, 0, 0, time.UTC)

	got := renderPrompt("%d%s %t> ", sess, "/somewhere/deep", now, false)
	if got != "/somewhere/deep 09:30> " {
		t.Errorf("prompt = %q", got)
	}

	// Nonzero status renders %s; color tags emit ANSI only when enabled.
	_ = sess.Eval("false")
	got = renderPrompt("{red}%s{reset}>", sess, "/x", now, false)
	if got != " [1]>" {
		t.Errorf("colorless prompt = %q", got)
	}
	got = renderPrompt("{red}%s{reset}>", sess, "/x", now, true)
	if got != "\x1b[31m [1]\x1b[0m>" {
		t.Errorf("colored prompt = %q", got)
	}

	// %% is a literal; unknown escapes pass through.
	if got := renderPrompt("100%% %q", sess, "/x", now, false); got != "100% %q" {
		t.Errorf("literal = %q", got)
	}
}

func TestGitBranch(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/feature/x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(sub); got != "feature/x" {
		t.Errorf("branch = %q (walk-up from subdir)", got)
	}
	if got := gitBranch(t.TempDir()); got != "" {
		t.Errorf("non-repo branch = %q, want empty", got)
	}
	// Detached HEAD shows a short hash.
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("0123456789abcdef\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(sub); got != "01234567" {
		t.Errorf("detached = %q", got)
	}
}

func TestLoadRC(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, "rc.grsh")
	content := "alias ll='ls -la'\nexport GRSH_RC_TEST=fromrc\nrcHelper := func() string { return \"helped\" }\n"
	if err := os.WriteFile(rc, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRSH_RC", rc)
	var out bytes.Buffer
	sess := runner.NewSession(runner.Options{Stdout: &out, Stderr: &out})
	if code, exited := loadRC(sess, &out); exited {
		t.Fatalf("rc exited with %d", code)
	}
	// Go helper defined in the rc is callable afterward.
	out.Reset()
	if err := sess.Eval("fmt.Println(rcHelper())"); err != nil {
		t.Fatalf("rc helper not available: %v", err)
	}
	if !strings.Contains(out.String(), "helped") {
		t.Errorf("helper output %q", out.String())
	}
	if os.Getenv("GRSH_RC_TEST") != "fromrc" {
		t.Error("rc export did not land")
	}
	os.Unsetenv("GRSH_RC_TEST")
}
