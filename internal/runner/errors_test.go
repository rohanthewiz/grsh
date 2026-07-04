package runner

import (
	"bytes"
	"strings"
	"testing"
)

func evalErr(t *testing.T, src string) error {
	t.Helper()
	var out bytes.Buffer
	sess := NewSession(Options{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out})
	return sess.RunSource("err.grsh", src)
}

// TestErrorPositions is the de-risk gate for the //line strategy: runtime
// and parse errors must point at the correct .grsh line.
func TestErrorPositions(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantLoc string
		wantMsg string
	}{
		{
			"runtime undefined variable",
			"echo one\nx := undefinedVar + 1\n",
			"err.grsh:2", "undefined: undefinedVar",
		},
		{
			"undefined after shell lines",
			"echo one\necho two\necho three\nfmt.Println(mystery)\n",
			"err.grsh:4", "undefined: mystery",
		},
		{
			"undefined on assignment RHS",
			"x := 1\n\nx = missingRHS\n",
			"err.grsh:3", "undefined: missingRHS",
		},
		{
			"division by zero deep in a block",
			"for i := 0; i < 3; i++ {\n    if i == 2 {\n        x := 1 / (i - 2)\n        fmt.Println(x)\n    }\n}\n",
			"err.grsh:3", "division by zero",
		},
		{
			"go parse error",
			"echo hi\nx := ]bad\n",
			"err.grsh:2", "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := evalErr(t, tc.src)
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := UserMessage(err)
			full := err.Error()
			if !strings.Contains(full, tc.wantLoc) && !strings.Contains(msg, tc.wantLoc) {
				t.Errorf("error %q (user msg %q) does not mention %q", full, msg, tc.wantLoc)
			}
			if tc.wantMsg != "" && !strings.Contains(full, tc.wantMsg) {
				t.Errorf("error %q does not mention %q", full, tc.wantMsg)
			}
		})
	}
}
