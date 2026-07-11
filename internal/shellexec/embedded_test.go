package shellexec

import (
	"syscall"
	"testing"
)

// Embedded pgroups apply only when embedded AND not interactive — a
// real tty means real job control should win.
func TestEmbeddedPgroupGating(t *testing.T) {
	cases := []struct {
		embedded, interactive, want bool
	}{
		{false, false, false},
		{true, false, true},
		{true, true, false},
		{false, true, false},
	}
	for _, c := range cases {
		st := NewState()
		st.Embedded, st.Interactive = c.embedded, c.interactive
		if got := embeddedPgroup(st); got != c.want {
			t.Errorf("embedded=%v interactive=%v: got %v, want %v",
				c.embedded, c.interactive, got, c.want)
		}
	}
}

// Signaling with no foreground pipeline registered must be a no-op
// reported as false — the host's stop button while idle.
func TestSignalForegroundIdle(t *testing.T) {
	st := NewState()
	if st.SignalForeground(syscall.SIGINT) {
		t.Error("SignalForeground with no registered pgroup should be false")
	}
	st.setForegroundPgid(0)
	if st.SignalForeground(syscall.SIGINT) {
		t.Error("SignalForeground after clear should be false")
	}
}

// eofReader is the embedded stdin: always EOF, no bytes, no blocking.
func TestEOFReader(t *testing.T) {
	n, err := eofReader{}.Read(make([]byte, 8))
	if n != 0 || err == nil {
		t.Errorf("Read = (%d, %v), want (0, io.EOF)", n, err)
	}
}
