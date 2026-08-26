//go:build darwin

package repl

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// This file is the end-to-end verification for the reeflective editor
// swap: it builds the real grsh binary, attaches it to a pty as its
// controlling terminal, and drives an interactive session with paced
// writes (each step waits for expected output before typing the next —
// readline must never be fed input it hasn't painted a prompt for).

// openPTYPair opens a master/slave pty pair via the BSD /dev/ptmx
// protocol (same helper pattern as shellexec's tty_test.go).
func openPTYPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	fd := m.Fd()
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, unix.TIOCPTYGRANT, 0); e != 0 {
		m.Close()
		t.Skipf("pty grant failed: %v", e)
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, unix.TIOCPTYUNLK, 0); e != 0 {
		m.Close()
		t.Skipf("pty unlock failed: %v", e)
	}
	var buf [128]byte
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		m.Close()
		t.Skipf("pty name failed: %v", e)
	}
	name := string(buf[:bytes.IndexByte(buf[:], 0)])
	s, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		m.Close()
		t.Skipf("open pty slave: %v", err)
	}
	// The display engine needs a real window size; a fresh pty reports
	// 0x0, which breaks line-wrap math.
	ws := unix.Winsize{Row: 40, Col: 120}
	_ = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &ws)
	t.Cleanup(func() { m.Close(); s.Close() })
	return m, s
}

// buildGrsh compiles the grsh binary once per test run.
func buildGrsh(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "grsh")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/grsh")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build grsh: %v\n%s", err, out)
	}
	return bin
}

// ptyShell accumulates everything the shell writes to the pty and lets
// steps block until an expected substring appears.
type ptyShell struct {
	t      *testing.T
	master *os.File
	mu     sync.Mutex
	out    bytes.Buffer
	cmd    *exec.Cmd
	home   string // the isolated $HOME (unit history lands in it)
	// writeMu serializes test keystrokes with the DSR responder: both
	// write to the master, and an unserialized reply could land BETWEEN
	// the bytes of one UTF-8 rune, corrupting the key stream mid-char.
	writeMu sync.Mutex
}

func startShell(t *testing.T, env ...string) *ptyShell {
	t.Helper()
	bin := buildGrsh(t)
	master, slave := openPTYPair(t)

	cmd := exec.Command(bin)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// New session with the pty as controlling terminal — job control and
	// raw-mode handling need the real thing, not just tty-shaped stdio.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	home := t.TempDir()
	cmd.Env = append([]string{
		"HOME=" + home, // isolate rc files and history
		"PATH=" + os.Getenv("PATH"),
		"TERM=xterm-256color",
	}, env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start grsh: %v", err)
	}
	p := &ptyShell{t: t, master: master, cmd: cmd, home: home}
	go func() {
		buf := make([]byte, 4096)
		var tail []byte // carry partial DSR sequences across read boundaries
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				p.mu.Lock()
				p.out.Write(chunk)
				p.mu.Unlock()
				// The editor probes the cursor position with DSR (ESC[6n)
				// and blocks until the terminal answers. A real emulator
				// replies automatically; this harness must play terminal.
				// Scan tail+chunk so a request split across reads is
				// still seen exactly once.
				scan := append(tail, chunk...)
				replies := strings.Count(string(scan), "\x1b[6n")
				if keep := len(scan); keep > 3 {
					tail = append(tail[:0], scan[keep-3:]...)
				} else {
					tail = append(tail[:0], scan...)
				}
				p.writeMu.Lock()
				for range replies {
					_, _ = master.WriteString("\x1b[1;1R")
				}
				p.writeMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return p
}

func (p *ptyShell) send(s string) {
	p.t.Helper()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if _, err := p.master.WriteString(s); err != nil {
		p.t.Fatalf("write %q: %v", s, err)
	}
}

// waitFor blocks until want appears in output produced AFTER the previous
// waitFor match (a moving offset — prompts repeat, so matching the whole
// transcript would race earlier occurrences).
func (p *ptyShell) waitFor(want string) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		if i := strings.Index(p.out.String(), want); i >= 0 {
			// Consume through the match so the next waitFor starts after it.
			p.out.Next(i + len(want))
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	p.mu.Lock()
	got := p.out.String()
	p.mu.Unlock()
	p.t.Fatalf("timeout waiting for %q; pending output:\n%s", want, got)
}

// TestReefEditorEndToEnd drives the default (reeflective) editor through
// the behaviors the editor swap must preserve: eval, multiline units with
// classifier-driven acceptance, ^C recovery, ^Z inertness at the prompt,
// and ^D exit with the last command's status.
func TestReefEditorEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and drives a pty")
	}
	p := startShell(t)

	p.waitFor("Ctrl+D to quit") // banner
	p.waitFor("grsh ")          // first prompt

	// Go eval round-trip. (Bare `x + 1` is deliberately shell by the
	// classifier's rules — Go usage needs a selector/assign/call shape.)
	p.send("x := 41\r")
	p.waitFor("grsh ")
	p.send("fmt.Println(x + 1)\r")
	p.waitFor("42")
	p.waitFor("grsh ")

	// NOTE on markers: every assertion below waits for output only EVAL
	// can produce. Keystroke echo (and reeflective's full-buffer repaints)
	// replay the typed source many times, so expected strings are built
	// by concatenation — the contiguous result never appears in the echo.

	// Multiline unit: Enter inside an open func must continue in-buffer.
	// The construct-breadcrumb hint below the input proves AcceptMultiline
	// consulted the classifier and the unit is pending.
	p.send("func hi() string {\r")
	p.waitFor("… func hi")
	p.send("return \"yo-\" + \"ho\"\r")
	p.send("}\r")
	p.send("fmt.Println(hi())\r")
	p.waitFor("yo-ho")

	// Shell leg still works through the same editor (external command;
	// %s formatting keeps the result out of the echo).
	p.send("printf 'sh%s\\n' ell-ok\r")
	p.waitFor("shell-ok")

	// ^C mid-line drops the input; the shell must survive and keep
	// evaluating — and the dropped text must NOT execute.
	p.send("echo doomed")
	p.send("\x03")
	p.send("fmt.Println(\"al\" + \"ive\")\r")
	p.waitFor("alive")

	// ^Z at the prompt is inert (bound to abort): no parent SIGTSTP, no
	// literal SUB byte in the buffer.
	p.send("\x1a")
	p.send("fmt.Println(\"after\" + \"-tstp\")\r")
	p.waitFor("after-tstp")

	// Unicode / wide runes through the editor and eval.
	p.send("wide := \"héllo — 世\" + \"界\"\r")
	p.send("fmt.Println(wide)\r")
	p.waitFor("héllo — 世界")

	// ^D on an empty line exits with the last status (0 here).
	p.send("true\r")
	p.waitFor("grsh ")
	p.send("\x04")

	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("grsh exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("grsh did not exit on ^D")
	}
}

// TestReefHighlightAndIndentEndToEnd drives the Round 2 display features
// through a real pty: syntax-highlight SGR sequences in the repaint
// stream (color is live — the pty makes colorEnabled true), auto-indent
// seeding real spaces into the buffer, and the electric } dedent. The
// buffer-content assertions read the persisted unit store: what was
// EVALUATED (indent included) is exactly what Append wrote.
func TestReefHighlightAndIndentEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and drives a pty")
	}
	p := startShell(t)
	p.waitFor("Ctrl+D to quit")
	p.waitFor("grsh ")

	// A resolvable command paints green in the input line repaint.
	p.send("true")
	p.waitFor("\x1b[32mtrue")
	p.send("\r")
	p.waitFor("grsh ")

	// An unresolvable one paints red; ^C drops it unrun.
	p.send("qzqxjw")
	p.waitFor("\x1b[31mqzqxjw")
	p.send("\x03")
	p.waitFor("grsh ")

	// Go literals: the number takes the numeric color in the repaint.
	p.send("n := 41")
	p.waitFor("\x1b[36m41")
	p.send("\r")
	p.waitFor("grsh ")

	// Auto-indent + electric brace, proven through evaluation and the
	// persisted unit: Enter after { seeds two real spaces, the typed }
	// dedents back to column 0, and the unit still evaluates.
	p.send("func hi() string {\r")
	p.waitFor("… func hi") // pending-unit breadcrumb: multiline mode is on
	p.send("return \"in\" + \"dent\"\r")
	p.send("}\r")
	p.send("fmt.Println(hi())\r")
	p.waitFor("indent")
	p.waitFor("grsh ") // back at the prompt: raw mode owns the tty again, so ^D below reaches readline

	// The unit store escapes newlines as literal \n, one unit per line.
	units, err := os.ReadFile(filepath.Join(p.home, ".grsh_units"))
	if err != nil {
		t.Fatalf("read unit store: %v", err)
	}
	want := `func hi() string {\n  return "in" + "dent"\n}`
	if !strings.Contains(string(units), want) {
		t.Fatalf("persisted unit lacks seeded indent + dedented brace:\nwant %q in:\n%s", want, units)
	}

	p.send("\x04")
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("grsh exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		p.mu.Lock()
		pending := p.out.String()
		p.mu.Unlock()
		t.Fatalf("grsh did not exit on ^D; pending output:\n%q", pending)
	}
}

// TestReefGhostTextEndToEnd drives fish-style ghost text through a real
// pty: a unit run at one prompt must be offered, dimmed, as an inline
// suggestion when its prefix is retyped at the next one, and ^F must
// accept it into the buffer so Enter runs the WHOLE command.
//
// The `%s` split in the seeded command is deliberate: keystroke echo and
// the editor's full-buffer repaints replay the typed source constantly, so
// only real evaluation can produce the contiguous string "ghost-seed".
func TestReefGhostTextEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and drives a pty")
	}
	p := startShell(t)
	p.waitFor("Ctrl+D to quit")
	p.waitFor("grsh ")

	const seed = `printf 'gho%s\n' st-seed`
	p.send(seed + "\r")
	p.waitFor("ghost-seed") // ran, and the loop appended it to the unit store
	p.waitFor("grsh ")

	// Retype a prefix: the remainder is painted after the cursor in the
	// library's suggestion color (dim + 256-color grey 242), unhighlighted.
	const prefix = "printf 'gho"
	p.send(prefix)
	p.waitFor("\x1b[38;05;242m" + strings.TrimPrefix(seed, prefix))

	// ^F (forward-char at the end of the line) accepts the whole ghost.
	p.send("\x06\r")
	p.waitFor("ghost-seed")
	p.waitFor("grsh ")

	p.send("\x04")
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("grsh exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		p.mu.Lock()
		pending := p.out.String()
		p.mu.Unlock()
		t.Fatalf("grsh did not exit on ^D; pending output:\n%q", pending)
	}
}

// TestReefHintLineEndToEnd drives the hint lane through a real pty: typing a
// registry symbol must print its reflected signature under the input, and an
// alias name must print its expansion — both dimmed, both without touching
// the buffer (Enter still runs exactly what was typed).
//
// Going through a pty is what proves the lane is real: the hint is painted
// BELOW the input line, so the display engine has to reserve a row for it and
// walk back up, and a hint that broke that math would corrupt the prompt
// rather than merely look wrong.
func TestReefHintLineEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and drives a pty")
	}
	p := startShell(t)
	p.waitFor("Ctrl+D to quit")
	p.waitFor("grsh ")

	// Signature help: dim (\x1b[2m), reflected from the registry, appearing
	// while the call is still open.
	p.send("fmt.Println(strings.ToUpper(")
	p.waitFor("\x1b[2mstrings.ToUpper(string) string")

	// The hint is display-only: finishing the call runs it unchanged.
	p.send(`"hi"))` + "\r")
	p.waitFor("HI")
	p.waitFor("grsh ")

	// Alias expansion, on the shell side of the same lane.
	p.send("alias gsay='printf'\r")
	p.waitFor("grsh ")
	p.send("gsay")
	p.waitFor("\x1b[2mgsay → printf")

	p.send(" 'ali%s\\n' as-ran\r")
	p.waitFor("alias-ran")
	p.waitFor("grsh ")

	p.send("\x04")
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("grsh exited with error: %v", err)
		}
	case <-time.After(10 * time.Second):
		p.mu.Lock()
		pending := p.out.String()
		p.mu.Unlock()
		t.Fatalf("grsh did not exit on ^D; pending output:\n%q", pending)
	}
}

// TestReefEditorNonzeroExitStatus checks the ^D exit code carries the
// last command's status, matching script semantics.
func TestReefEditorNonzeroExitStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary and drives a pty")
	}
	p := startShell(t)
	p.waitFor("grsh ")
	p.send("false\r")
	p.waitFor("[1]") // prompt flags the failed status
	p.send("exit\r")

	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		var xe *exec.ExitError
		if err == nil {
			t.Fatal("expected nonzero exit status")
		} else if !errors.As(err, &xe) || xe.ExitCode() != 1 {
			t.Fatalf("want exit 1, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("grsh did not exit")
	}
}
