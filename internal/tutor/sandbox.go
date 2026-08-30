package tutor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sandbox is the lesson's playground: a throwaway directory, seeded with
// fixtures, that the tutor cd's into before the first prompt.
//
// It exists because output-based grading is only as reliable as the
// directory it runs in. "Count the Go files" is a real exercise in a
// directory that provably holds three of them and a lottery in the user's
// home. Fixtures also let the curriculum reuse the README's own examples
// (grep 500 out of access.log) instead of inventing toy data.
//
// Nothing here touches the user's real files. The one deliberate
// exception is the capstone, which writes a saved script the graduate
// keeps — that step names its path explicitly.
type sandbox struct {
	dir  string
	prev string // cwd to restore on cleanup
}

// fixtures is the playground's contents, path → body. Generated in Go
// rather than go:embed'ed because they are small, and because a
// tutorial's fixtures are part of the lesson's logic (the counts and the
// error-line total below are what several steps grade against) — keeping
// them beside the engine means a content change and its fixture change
// land in one file.
//
//	.
//	├── access.log      120 lines, 17 of them 500s (chapter 1 + capstone)
//	├── data.json       one small object (chapter 5, json.Parse)
//	├── greet.go        three .go files, so `ls *.go | wc -l` is 3
//	├── main.go
//	├── util.go
//	├── notes/          a subtree, for glob and filepath work
//	│   ├── monday.md
//	│   ├── tuesday.md
//	│   └── archive/old.md
//	└── old logs/       a name with a space (chapter 6)
//	    └── legacy.log
func fixtures() map[string]string {
	f := map[string]string{
		"access.log": accessLog(),
		"data.json":  "{\n  \"service\": \"api\",\n  \"port\": 8080,\n  \"debug\": false\n}\n",

		// Three .go files, each a couple of lines, so the line-counting
		// exercises have small, memorable numbers.
		"main.go":  "package main\n\nfunc main() {\n\tgreet(\"world\")\n}\n",
		"greet.go": "package main\n\nimport \"fmt\"\n\nfunc greet(who string) {\n\tfmt.Println(\"hello,\", who)\n}\n",
		"util.go":  "package main\n\nfunc double(n int) int {\n\treturn n * 2\n}\n",

		"notes/monday.md":      "# Monday\n\n- ship the parser\n",
		"notes/tuesday.md":     "# Tuesday\n\n- ship the classifier\n- write the tutor\n",
		"notes/archive/old.md": "# Old\n\nsuperseded\n",

		// A path with a space in it, for chapter 6. In bash this is where
		// the quoting dance begins; the chapter's whole point is that
		// `d := "old logs"` then `ls {d}` needs no quoting at all, so the
		// fixture has to be a real directory with a real space in its name.
		"old logs/legacy.log": "10.0.0.9 - - [01/Jan/2020:00:00:00 +0000] \"GET /old HTTP/1.0\" 200 11\n",
	}
	return f
}

// accessLogErrors is how many 500 lines the fixture log holds. Steps
// grade against this number, so it lives next to the generator rather
// than being counted by hand in a lesson file.
const accessLogErrors = 17

// accessLog builds a deterministic 120-line web log. Determinism is the
// whole point: no timestamps from time.Now, no shuffling — the same
// bytes on every machine, so `grep -c 500 access.log` is a number a
// lesson can assert.
func accessLog() string {
	paths := []string{"/", "/api/users", "/api/orders", "/health", "/static/app.js"}
	// A fixed status cycle whose 500s land at a stable, non-adjacent
	// spread: 17 of the 120 lines, which is enough to be worth grepping
	// and few enough to eyeball.
	codes := []int{200, 200, 304, 200, 500, 200, 404, 200, 200, 500, 200, 301, 200, 200}
	var b strings.Builder
	for i := range 120 {
		fmt.Fprintf(&b, "10.0.0.%d - - [12/Aug/2026:%02d:%02d:00 +0000] \"GET %s HTTP/1.1\" %d %d\n",
			1+i%9, 8+i/30, i%60, paths[i%len(paths)], codes[i%len(codes)], 120+i*7)
	}
	return b.String()
}

// newSandbox creates the playground, writes the fixtures, and chdir's
// into it.
//
// The process cwd is what moves, not just a session field: the student's
// `ls`, `cat` and `glob("*.go")` all resolve through the real working
// directory, and a lesson that lied about where it was would teach the
// wrong thing the first time a path didn't resolve.
func newSandbox() (*sandbox, error) {
	prev, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "grsh-tutor-*")
	if err != nil {
		return nil, err
	}
	for rel, body := range fixtures() {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	// Resolve symlinks so the path the tutor reports and the path `pwd`
	// prints agree: on macOS os.MkdirTemp hands back /var/..., which is a
	// symlink to /private/var, and a `file` verifier comparing the two
	// forms would disagree with the student's own eyes.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	if err := os.Chdir(dir); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &sandbox{dir: dir, prev: prev}, nil
}

// cleanup restores the working directory and removes the playground.
//
// Called on a normal exit only. A panic leaves the directory behind on
// purpose: the fixtures plus whatever the student's last command wrote
// are the entire reproduction for a crash report, and reclaiming a few
// kilobytes of $TMPDIR is worth less than that.
func (s *sandbox) cleanup() {
	if s == nil {
		return
	}
	if s.prev != "" {
		_ = os.Chdir(s.prev)
	}
	_ = os.RemoveAll(s.dir)
}
