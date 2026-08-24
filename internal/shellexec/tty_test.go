//go:build darwin

package shellexec

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/rohanthewiz/grsh/internal/shellparse"
)

// openPTYPair opens a pty master/slave pair via the BSD /dev/ptmx
// protocol. The slave is a real terminal fd, which lets the job-control
// gate be exercised without an interactive test run.
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
	t.Cleanup(func() { m.Close(); s.Close() })
	return m, s
}

// TestInteractiveTTYRequiresFileStdio guards the job-control invariant:
// the jc path reaps with Wait4 and never calls exec.Cmd.Wait, so it is
// only safe when every stdio leg is a real file (no exec copy goroutines).
// A $() capture at the REPL (buffer stdout) must therefore never enter it.
func TestInteractiveTTYRequiresFileStdio(t *testing.T) {
	_, slave := openPTYPair(t)

	st := NewState()
	st.Interactive = true

	var buf bytes.Buffer
	if _, ok := interactiveTTY(st, Stdio{In: slave, Out: &buf, Err: os.Stderr}); ok {
		t.Error("buffer stdout (capture) must not enable job control")
	}
	if _, ok := interactiveTTY(st, Stdio{In: slave, Out: os.Stdout, Err: &buf}); ok {
		t.Error("buffer stderr must not enable job control")
	}
	if _, ok := interactiveTTY(st, Stdio{In: slave, Out: slave, Err: slave}); !ok {
		t.Error("all-file tty stdio should enable job control")
	}
	st.Interactive = false
	if _, ok := interactiveTTY(st, Stdio{In: slave, Out: slave, Err: slave}); ok {
		t.Error("non-interactive session must not enable job control")
	}
}

// TestCaptureInteractiveByteExact is the end-to-end regression for the
// capture/job-control bug: with an interactive session whose stdin is a
// real tty, $() captures used to take the Wait4 reaping path, racing the
// exec copier goroutine and intermittently truncating output. Run under
// -race this also proves the copier is properly synchronized.
func TestCaptureInteractiveByteExact(t *testing.T) {
	_, slave := openPTYPair(t)
	// Capture wires the process stdin; swap in the pty so the old gate
	// (stdin-is-a-tty only) would have chosen job control.
	oldStdin := os.Stdin
	os.Stdin = slave
	defer func() { os.Stdin = oldStdin }()

	st := NewState()
	st.Interactive = true
	list, err := shellparse.Parse("seq 1 2000")
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&want, "%d\n", i)
	}
	wantOut := strings.TrimRight(want.String(), "\n")

	for i := 0; i < 20; i++ {
		out, status, err := Capture(st, list, nil)
		if err != nil {
			t.Fatalf("capture error: %v", err)
		}
		if status != 0 {
			t.Fatalf("capture status = %d", status)
		}
		if out != wantOut {
			t.Fatalf("iteration %d: truncated capture: got %d bytes, want %d", i, len(out), len(wantOut))
		}
	}
}
