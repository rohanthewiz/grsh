package shellexec

import "testing"

func TestPipeFail(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	if _, status := runLine(t, st, `false | true`); status != 0 {
		t.Errorf("default false|true = %d, want 0 (last command wins)", status)
	}

	st.PipeFail = true
	if _, status := runLine(t, st, `false | true`); status != 1 {
		t.Errorf("pipefail false|true = %d, want 1", status)
	}
	if _, status := runLine(t, st, `true | false`); status != 1 {
		t.Errorf("pipefail true|false = %d, want 1", status)
	}
	if _, status := runLine(t, st, `true | true`); status != 0 {
		t.Errorf("pipefail true|true = %d, want 0", status)
	}
	// Rightmost nonzero wins.
	if _, status := runLine(t, st, `sh -c 'exit 3' | true | true`); status != 3 {
		t.Errorf("pipefail exit3|true|true = %d, want 3", status)
	}
}

func TestPipeFailCapturedAtJobLaunch(t *testing.T) {
	st := NewState()
	t.Chdir(t.TempDir())

	st.PipeFail = true
	runLine(t, st, `sh -c 'exit 3' | true &`)
	st.PipeFail = false // must not affect the already-launched job
	if _, status := runLine(t, st, `wait %1`); status != 3 {
		t.Errorf("background pipefail status = %d, want 3 (captured at launch)", status)
	}
}
