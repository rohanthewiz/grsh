package shellexec

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rohanthewiz/grsh/internal/shellparse"
)

// expand parses a line and expands the first command's words.
func expand(t *testing.T, st *State, line string) []string {
	t.Helper()
	list, err := shellparse.Parse(line)
	if err != nil {
		t.Fatalf("Parse(%q): %v", line, err)
	}
	argv, err := ExpandWords(st, list.Items[0].Pipes[0].Cmds[0].Words, nil)
	if err != nil {
		t.Fatalf("ExpandWords(%q): %v", line, err)
	}
	return argv
}

func TestExpandWords(t *testing.T) {
	st := NewState()
	st.ScriptName = "test.grsh"
	st.ScriptArgs = []string{"one", "two three"}
	t.Setenv("GRSH_TV", "hello world")
	t.Setenv("GRSH_EMPTY", "")

	home, _ := os.UserHomeDir()

	tests := []struct {
		line string
		want []string
	}{
		{`echo plain`, []string{"echo", "plain"}},
		{`echo 'a b' c`, []string{"echo", "a b", "c"}},
		{`echo a"b c"d`, []string{"echo", "ab cd"}},
		// $VAR is never field-split (deliberate, zsh-like).
		{`echo $GRSH_TV`, []string{"echo", "hello world"}},
		{`echo "$GRSH_TV!"`, []string{"echo", "hello world!"}},
		// Empty expansions drop the field; explicit empties survive.
		{`echo $GRSH_UNSET_XYZ end`, []string{"echo", "end"}},
		{`echo "" end`, []string{"echo", "", "end"}},
		{`echo pre$GRSH_EMPTY`, []string{"echo", "pre"}},
		// Script args.
		{`echo $1`, []string{"echo", "one"}},
		{`echo $2`, []string{"echo", "two three"}},
		{`echo $#`, []string{"echo", "2"}},
		{`echo $@`, []string{"echo", "one", "two three"}},
		{`echo $0`, []string{"echo", "test.grsh"}},
		// Tilde.
		{`echo ~`, []string{"echo", home}},
		{`echo ~/x`, []string{"echo", filepath.Join(home, "x")}},
		{`echo '~'`, []string{"echo", "~"}},
		// Escaped glob chars stay literal.
		{`echo \*.go`, []string{"echo", "*.go"}},
		{`echo '*.go'`, []string{"echo", "*.go"}},
	}
	for _, tc := range tests {
		got := expand(t, st, tc.line)
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("expand(%q)\n got: %q\nwant: %q", tc.line, got, tc.want)
		}
	}
}

func TestExpandGlob(t *testing.T) {
	st := NewState()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, f := range []string{"a1.txt", "a2.txt", "b.log"} {
		if err := os.WriteFile(f, nil, 0644); err != nil {
			t.Fatal(err)
		}
	}

	if got := expand(t, st, `echo a*.txt`); strings.Join(got, " ") != "echo a1.txt a2.txt" {
		t.Errorf("glob got %q", got)
	}
	// No match passes through literally.
	if got := expand(t, st, `echo z*.none`); strings.Join(got, " ") != "echo z*.none" {
		t.Errorf("no-match glob got %q", got)
	}
	// Quoted pattern never globs.
	if got := expand(t, st, `echo "a*.txt"`); strings.Join(got, " ") != "echo a*.txt" {
		t.Errorf("quoted glob got %q", got)
	}
}

// runLine parses and runs a line, returning combined output and status.
func runLine(t *testing.T, st *State, line string) (string, int) {
	t.Helper()
	list, err := shellparse.Parse(line)
	if err != nil {
		t.Fatalf("Parse(%q): %v", line, err)
	}
	var buf bytes.Buffer
	status, err := Run(st, list, nil, Stdio{In: strings.NewReader(""), Out: &buf, Err: &buf})
	if err != nil {
		t.Fatalf("Run(%q): %v", line, err)
	}
	return buf.String(), status
}

func TestRun(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	out, status := runLine(t, st, `printf 'a\nb\nc\n' | grep -v b | wc -l | tr -d ' '`)
	if strings.TrimSpace(out) != "2" || status != 0 {
		t.Errorf("pipeline: out=%q status=%d", out, status)
	}

	if _, status = runLine(t, st, `false`); status != 1 {
		t.Errorf("false: status=%d, want 1", status)
	}
	if out, status = runLine(t, st, `definitely_not_a_cmd_xyz`); status != 127 || !strings.Contains(out, "command not found") {
		t.Errorf("not found: out=%q status=%d", out, status)
	}
	if out, _ = runLine(t, st, `false && echo yes || echo no`); strings.TrimSpace(out) != "no" {
		t.Errorf("and-or: out=%q", out)
	}

	// Redirections round-trip through files.
	runLine(t, st, `printf 'one\n' > f.txt`)
	runLine(t, st, `printf 'two\n' >> f.txt`)
	out, _ = runLine(t, st, `cat < f.txt`)
	if out != "one\ntwo\n" {
		t.Errorf("redirs: out=%q", out)
	}

	// 2>&1 folds stderr into a capturable stream.
	out, _ = runLine(t, st, `sh -c 'echo oops 1>&2' 2>&1 | tr a-z A-Z`)
	if strings.TrimSpace(out) != "OOPS" {
		t.Errorf("dup: out=%q", out)
	}
}

func TestExitBuiltin(t *testing.T) {
	st := NewState()
	list, err := shellparse.Parse("exit 3")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, err = Run(st, list, nil, Stdio{In: strings.NewReader(""), Out: &buf, Err: &buf})
	xe, ok := err.(ExitErr)
	if !ok || xe.Code != 3 {
		t.Fatalf("exit 3: err=%v", err)
	}
}

func TestAlias(t *testing.T) {
	st := NewState()
	runLine(t, st, `alias greet='echo hello from'`)
	out, _ := runLine(t, st, `greet alias`)
	if strings.TrimSpace(out) != "hello from alias" {
		t.Errorf("alias: out=%q", out)
	}
}
