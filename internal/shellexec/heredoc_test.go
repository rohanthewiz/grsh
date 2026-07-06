package shellexec

import (
	"os"
	"strings"
	"testing"
)

func TestHeredocBasic(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	out, status := runLine(t, st, "cat <<EOF\nhello\nworld\nEOF")
	if out != "hello\nworld\n" || status != 0 {
		t.Errorf("basic heredoc: got %q status %d", out, status)
	}
}

func TestHeredocExpansion(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())
	t.Setenv("GRSH_HD", "vals here")

	out, _ := runLine(t, st, "cat <<EOF\nvar: $GRSH_HD\nsub: $(echo captured)\nesc: \\$GRSH_HD\nEOF")
	want := "var: vals here\nsub: captured\nesc: $GRSH_HD\n"
	if out != want {
		t.Errorf("expanded heredoc:\n got %q\nwant %q", out, want)
	}

	// Quoted delimiter: fully literal.
	out, _ = runLine(t, st, "cat <<'EOF'\nvar: $GRSH_HD\nsub: $(echo captured)\nEOF")
	want = "var: $GRSH_HD\nsub: $(echo captured)\n"
	if out != want {
		t.Errorf("literal heredoc:\n got %q\nwant %q", out, want)
	}

	// Braces are never Go interpolation inside a heredoc.
	out, _ = runLine(t, st, "cat <<EOF\n{\"key\": [1, 2]}\nEOF")
	if out != "{\"key\": [1, 2]}\n" {
		t.Errorf("JSON body: got %q", out)
	}
}

func TestHeredocStripTabs(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	out, _ := runLine(t, st, "cat <<-EOF\n\t\tindented\n\tEOF")
	if out != "indented\n" {
		t.Errorf("<<- heredoc: got %q", out)
	}
}

func TestHeredocInPipelineAndRedirect(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	out, _ := runLine(t, st, "cat <<EOF | tr a-z A-Z\nshout\nEOF")
	if out != "SHOUT\n" {
		t.Errorf("heredoc into pipeline: got %q", out)
	}

	_, status := runLine(t, st, "cat <<EOF > hd.txt\nsaved\nEOF")
	if status != 0 {
		t.Fatalf("heredoc + redirect status %d", status)
	}
	b, err := os.ReadFile("hd.txt")
	if err != nil || string(b) != "saved\n" {
		t.Errorf("heredoc > file: got %q err %v", b, err)
	}

	// Two heredocs: both bodies parsed, the last one wins fd 0.
	out, _ = runLine(t, st, "cat <<A <<B\nfirst\nA\nsecond\nB")
	if out != "second\n" {
		t.Errorf("double heredoc: got %q (want last body)", out)
	}
}

// A command that never reads its heredoc must not hang, even when the
// body exceeds the kernel pipe buffer (the writer goroutine gets EPIPE
// once the read end closes).
func TestHeredocUnreadBigBody(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	body := strings.Repeat("x", 1<<20) // 1 MiB > 64 KiB pipe buffer
	out, status := runLine(t, st, "true <<EOF\n"+body+"\nEOF")
	if status != 0 || out != "" {
		t.Errorf("unread heredoc: got %q status %d", out, status)
	}
}

// Heredocs work on background jobs; the body is expanded eagerly at
// launch time like every other redirect target.
func TestHeredocBackgroundJob(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())
	t.Setenv("GRSH_BGHD", "before")

	_, status := runLine(t, st, "cat <<EOF > bg.txt &\nval: $GRSH_BGHD\nEOF")
	if status != 0 {
		t.Fatalf("bg heredoc launch status %d", status)
	}
	// Flipping the variable after launch must not affect the body.
	os.Setenv("GRSH_BGHD", "after")
	out, _ := runLine(t, st, "wait %1")
	_ = out
	b, err := os.ReadFile("bg.txt")
	if err != nil || string(b) != "val: before\n" {
		t.Errorf("bg heredoc file: got %q err %v", b, err)
	}
}
