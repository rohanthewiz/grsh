package classify

import (
	"strings"
	"testing"
)

var testPkgs = []string{"fmt", "strings", "strconv", "os", "filepath", "time", "regexp", "json", "sort", "math", "errors", "serr", "logger"}

// TestCorpus classifies real-world lines. This corpus grows with every
// reported ambiguity — it is the contract for the classifier's feel.
func TestCorpus(t *testing.T) {
	type line struct {
		text string
		want Kind
	}
	// Each entry classifies independently unless a prior line declares an
	// identifier (grouped cases below handle that).
	corpus := []line{
		// Everyday commands stay shell.
		{`ls -la ~/projs`, Shell},
		{`git status`, Shell},
		{`git commit -m "fix: parser"`, Shell},
		{`cat access.log | grep 500 > errs.txt`, Shell},
		{`docker compose up -d`, Shell},
		{`kubectl get pods -n prod`, Shell},
		{`make build && make test`, Shell},
		{`curl -s https://example.com/api | jq .items`, Shell},
		{`tar -xzf release.tgz -C /opt`, Shell},
		{`find . -name '*.go' -exec wc -l {} \;`, Shell},
		{`awk '{print $1}' access.log`, Shell},
		{`dd if=/dev/zero of=/tmp/blk bs=1m count=1`, Shell},
		{`ssh host 'uptime'`, Shell},
		{`grep -r "TODO" src/`, Shell},
		{`FOO=bar env`, Shell},
		{`./run.sh --flag`, Shell},
		{`/usr/local/bin/tool`, Shell},
		{`echo $HOME`, Shell},
		{`time ls`, Shell},        // package name NOT followed by '.'
		{`go build ./...`, Shell}, // `go` is deliberately shell
		{`go test -v`, Shell},
		{`sh echo forced shell`, Shell}, // escape hatch
		// Go logic.
		{`x := 5`, Go},
		{`x, y := 1, 2`, Go},
		{`for i := 0; i < 10; i++ {`, Go},
		{`for _, f := range glob("*.go") {`, Go},
		{`if lines(out) > 500 {`, Go},
		{`}`, Go},
		{`} else {`, Go},
		{`var count int`, Go},
		{`const limit = 10`, Go},
		{`func greet(name string) string {`, Go},
		{`return count`, Go},
		{`break`, Go},
		{`continue`, Go},
		{`switch mode {`, Go},
		{`case "fast":`, Go},
		{`default:`, Go},
		{`fmt.Println("hello")`, Go},
		{`strings.ToUpper(s)`, Go},
		{`(x + 1)`, Go}, // escape hatch for a bare expression
		{`defer cleanup()`, Go},
		{`type Point struct { X, Y int }`, Go},
	}

	for _, tc := range corpus {
		c := New(testPkgs)
		chunks, err := c.File(tc.text)
		if err != nil {
			t.Errorf("File(%q) error: %v", tc.text, err)
			continue
		}
		if len(chunks) != 1 {
			t.Errorf("File(%q) = %d chunks, want 1", tc.text, len(chunks))
			continue
		}
		if chunks[0].Kind != tc.want {
			t.Errorf("classify(%q) = %v (rule %s), want %v", tc.text, chunks[0].Kind, chunks[0].Rule, tc.want)
		}
	}
}

// TestScopeSensitive verifies rule 6a: declared identifiers flip to Go.
func TestScopeSensitive(t *testing.T) {
	src := strings.Join([]string{
		`x := 5`,
		`x = x + 1`, // declared → Go
		`x++`,       // declared → Go
		`ls x`,      // ls not declared → shell
		`count(x)`,  // count not declared... shell? no: undeclared ident + ( → shell
		`myfn := func(a int) int { return a }`,
		`myfn(3)`, // declared → Go
	}, "\n")
	c := New(testPkgs)
	chunks, err := c.File(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []Kind{Go, Go, Go, Shell, Shell, Go, Go}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks, want %d: %+v", len(chunks), len(want), chunks)
	}
	for i, k := range want {
		if chunks[i].Kind != k {
			t.Errorf("line %d (%q): got %v (rule %s), want %v", i+1, chunks[i].Text, chunks[i].Kind, chunks[i].Rule, k)
		}
	}
}

// TestBlocksAndContinuations verifies multi-line Go handling.
func TestBlocksAndContinuations(t *testing.T) {
	src := strings.Join([]string{
		`for _, f := range files {`, // block opens
		`    wc -l`,                 // shell INSIDE a Go block
		`    total := 1 +`,          // Go continuation (trailing +)
		`        2`,
		`    fmt.Println(total,`, // continuation (open paren)
		`        f)`,
		`}`,
		`m := map[string]int{`, // composite literal joins lines
		`    "a": 1,`,
		`}`,
		`type Point struct {`, // struct body joins lines too
		`    X int`,
		`}`,
		`echo done`,
	}, "\n")
	c := New(append(testPkgs, "files"))
	// files isn't a package; declare it as a variable instead.
	c.scope.Add("files")
	chunks, err := c.File(src)
	if err != nil {
		t.Fatal(err)
	}
	type exp struct {
		kind       Kind
		start, end int
	}
	want := []exp{
		{Go, 1, 1},    // for ... {
		{Shell, 2, 2}, // wc -l
		{Go, 3, 4},    // total := 1 + 2
		{Go, 5, 6},    // fmt.Println(...)
		{Go, 7, 7},    // }
		{Go, 8, 10},   // composite literal m := ...
		{Go, 11, 13},  // struct declaration
		{Shell, 14, 14},
	}
	if len(chunks) != len(want) {
		for _, ch := range chunks {
			t.Logf("chunk: %v %d-%d %q", ch.Kind, ch.StartLine, ch.EndLine, ch.Text)
		}
		t.Fatalf("got %d chunks, want %d", len(chunks), len(want))
	}
	for i, w := range want {
		ch := chunks[i]
		if ch.Kind != w.kind || ch.StartLine != w.start || ch.EndLine != w.end {
			t.Errorf("chunk %d: got %v %d-%d (rule %s), want %v %d-%d",
				i, ch.Kind, ch.StartLine, ch.EndLine, ch.Rule, w.kind, w.start, w.end)
		}
	}
}

// TestDepthTracking verifies Depth is recorded for the transform stage.
func TestDepthTracking(t *testing.T) {
	src := strings.Join([]string{
		`func helper() {`,
		`fmt.Println("hi")`,
		`}`,
		`x := 1`,
	}, "\n")
	c := New(testPkgs)
	chunks, err := c.File(src)
	if err != nil {
		t.Fatal(err)
	}
	depths := []int{0, 1, 1, 0}
	for i, d := range depths {
		if chunks[i].Depth != d {
			t.Errorf("chunk %d (%q): depth %d, want %d", i, chunks[i].Text, chunks[i].Depth, d)
		}
	}
}
