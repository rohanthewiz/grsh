package shellexec

import (
	"bytes"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startStoppedJob launches sleep in its own pgroup, stops it, and adopts
// it — the same state a ^Z suspension produces, minus the tty.
func startStoppedJob(t *testing.T, st *State, dur string) (*Job, int) {
	t.Helper()
	c := exec.Command("sleep", dur)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pid := c.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	j := st.Jobs.AdoptStopped("sleep "+dur, pid, []int{pid}, map[int]int{pid: 0}, []int{0}, false)
	return j, pid
}

func waitDone(t *testing.T, j *Job, within time.Duration) {
	t.Helper()
	select {
	case <-j.Done:
	case <-time.After(within):
		t.Fatal("job did not finish in time")
	}
}

func TestBgResumesStoppedJob(t *testing.T) {
	st := NewState()
	j, _ := startStoppedJob(t, st, "0.2")

	var out bytes.Buffer
	code, err := runBuiltin(st, "bg", nil, Stdio{Out: &out, Err: &out})
	if err != nil || code != 0 {
		t.Fatalf("bg: code=%d err=%v out=%q", code, err, out.String())
	}
	if !strings.Contains(out.String(), "[1]") {
		t.Errorf("bg output %q, want job announcement", out.String())
	}
	waitDone(t, j, 5*time.Second)
	if status := st.Jobs.Wait(j); status != 0 {
		t.Errorf("resumed job status = %d, want 0", status)
	}
}

func TestFgResumesStoppedJobWithoutTTY(t *testing.T) {
	st := NewState()
	startStoppedJob(t, st, "0.2")

	var out bytes.Buffer
	code, err := runBuiltin(st, "fg", nil, Stdio{In: strings.NewReader(""), Out: &out, Err: &out})
	if err != nil || code != 0 {
		t.Fatalf("fg: code=%d err=%v out=%q", code, err, out.String())
	}
	if !strings.Contains(out.String(), "sleep 0.2") {
		t.Errorf("fg output %q, want cmdline echoed", out.String())
	}
	if len(st.Jobs.All()) != 0 {
		t.Error("fg did not reap the finished job")
	}
}

func TestJobsListsStopped(t *testing.T) {
	st := NewState()
	j, _ := startStoppedJob(t, st, "5")

	out, _ := runLine(t, st, `jobs`)
	if !strings.Contains(out, "Stopped") || !strings.Contains(out, "sleep 5") {
		t.Errorf("jobs = %q, want Stopped listing", out)
	}
	// A stopped job survives the listing (only Done jobs reap).
	if len(st.Jobs.All()) != 1 {
		t.Error("stopped job was reaped by jobs listing")
	}
	st.Jobs.ResumeBackground(j) // avoid leaking a stopped sleep past cleanup
}

func TestKillStoppedJobIsCollected(t *testing.T) {
	st := NewState()
	j, _ := startStoppedJob(t, st, "5")

	out, status := runLine(t, st, `kill %1`)
	if status != 0 {
		t.Fatalf("kill: status=%d out=%q", status, out)
	}
	waitDone(t, j, 5*time.Second)
	if s := st.Jobs.Wait(j); s != 128+int(syscall.SIGTERM) {
		t.Errorf("killed job status = %d, want %d", s, 128+int(syscall.SIGTERM))
	}
	notes := st.Jobs.Notifications()
	if len(notes) != 1 || !strings.Contains(notes[0], "Exit") {
		t.Errorf("notes = %q, want an Exit notification", notes)
	}
}

func TestWaitSkipsStoppedJob(t *testing.T) {
	st := NewState()
	j, _ := startStoppedJob(t, st, "5")

	done := make(chan struct{})
	var out string
	go func() {
		out, _ = runLine(t, st, `wait`)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("wait blocked on a stopped job (deadlock guard failed)")
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("wait output %q, want stopped-job warning", out)
	}
	st.Jobs.ResumeBackground(j)
}

func TestBgOnRunningJobRefused(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())
	runLine(t, st, `sleep 0.3 &`)
	out, status := runLine(t, st, `bg %1`)
	if status != 1 || !strings.Contains(out, "already in background") {
		t.Errorf("bg on running &-job: status=%d out=%q", status, out)
	}
	runLine(t, st, `wait`)
}
