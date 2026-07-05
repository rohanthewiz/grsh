package shellexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Background jobs write to files, never the shared test buffer — the
// buffer is not safe for concurrent writes; the terminal a real session
// uses is.

func TestBackgroundJobAndWait(t *testing.T) {
	st := NewState()
	dir := t.TempDir()
	t.Chdir(dir)

	out, status := runLine(t, st, `echo bg-ran > out.txt &`)
	if status != 0 || out != "" {
		t.Fatalf("launch: status=%d out=%q", status, out)
	}
	if _, status = runLine(t, st, `wait`); status != 0 {
		t.Fatalf("wait: status=%d", status)
	}
	b, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "bg-ran" {
		t.Errorf("out.txt = %q, err %v", b, err)
	}
}

func TestBackgroundChainShortCircuit(t *testing.T) {
	st := NewState()
	dir := t.TempDir()
	t.Chdir(dir)

	// false && ... must skip the write; || must take the fallback.
	runLine(t, st, `false && echo skipped > a.txt || echo fallback > b.txt &`)
	runLine(t, st, `wait`)
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); !os.IsNotExist(err) {
		t.Error("a.txt exists — && did not short-circuit in background")
	}
	b, err := os.ReadFile(filepath.Join(dir, "b.txt"))
	if err != nil || strings.TrimSpace(string(b)) != "fallback" {
		t.Errorf("b.txt = %q, err %v", b, err)
	}
}

func TestBackgroundPipelineStatusViaWaitSpec(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	runLine(t, st, `true | false &`)
	out, status := runLine(t, st, `wait %1`)
	if status != 1 {
		t.Errorf("wait %%1 status = %d (out %q), want pipeline status 1", status, out)
	}
}

func TestJobsListingAndReap(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	runLine(t, st, `sleep 5 &`)
	out, _ := runLine(t, st, `jobs`)
	if !strings.Contains(out, "[1]") || !strings.Contains(out, "Running") || !strings.Contains(out, "sleep 5") {
		t.Errorf("jobs = %q", out)
	}

	runLine(t, st, `kill -9 %1`)
	j := st.Jobs.Find("%1")
	if j == nil {
		t.Fatal("job vanished before wait")
	}
	st.Jobs.Wait(j)

	out, _ = runLine(t, st, `jobs`)
	if !strings.Contains(out, "Exit") {
		t.Errorf("jobs after kill = %q, want Exit line", out)
	}
	// Listing a Done job reaps it.
	if out, _ = runLine(t, st, `jobs`); strings.TrimSpace(out) != "" {
		t.Errorf("jobs after reap = %q, want empty", out)
	}
}

func TestFgWaitsAndReturnsStatus(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	runLine(t, st, `false &`)
	out, status := runLine(t, st, `fg`)
	if status != 1 {
		t.Errorf("fg status = %d, want 1", status)
	}
	if !strings.Contains(out, "false") {
		t.Errorf("fg output = %q, want the cmdline echoed", out)
	}
}

func TestNotifications(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	runLine(t, st, `true &`)
	j := st.Jobs.Find("%1")
	st.Jobs.Wait(j)

	notes := st.Jobs.Notifications()
	if len(notes) != 1 || !strings.Contains(notes[0], "[1]") || !strings.Contains(notes[0], "Done") {
		t.Errorf("notes = %q", notes)
	}
	if notes = st.Jobs.Notifications(); len(notes) != 0 {
		t.Errorf("second drain = %q, want empty", notes)
	}
}

func TestBackgroundBuiltinRejected(t *testing.T) {
	st := NewState()
	out, status := runLine(t, st, `cd /tmp &`)
	if status != 1 || !strings.Contains(out, "cannot run in the background") {
		t.Errorf("status=%d out=%q", status, out)
	}
}

func TestWaitNoSuchJob(t *testing.T) {
	st := NewState()
	out, status := runLine(t, st, `wait %9`)
	if status != 127 || !strings.Contains(out, "no such job") {
		t.Errorf("status=%d out=%q", status, out)
	}
}

func TestAmpersandSeparator(t *testing.T) {
	st := NewState()
	dir := t.TempDir()
	t.Chdir(dir)

	// `a & b` runs a in background AND b immediately in the foreground.
	out, status := runLine(t, st, `sleep 0.3 > /dev/null & echo now`)
	if status != 0 || strings.TrimSpace(out) != "now" {
		t.Errorf("status=%d out=%q", status, out)
	}
	if len(st.Jobs.Running()) != 1 {
		t.Error("background sleep not tracked as running")
	}
	runLine(t, st, `wait`)
}

func TestKillTermSpeedsUpSleep(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	start := time.Now()
	runLine(t, st, `sleep 10 &`)
	runLine(t, st, `kill %1`)
	_, status := runLine(t, st, `wait %1`)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("kill %%1 did not terminate the job (took %v)", elapsed)
	}
	if status != 143 { // 128 + SIGTERM, bash convention
		t.Errorf("wait after kill: status = %d, want 143", status)
	}
}
