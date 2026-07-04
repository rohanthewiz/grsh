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

		// --- extended corpus: everyday commands must stay shell ---
		{`brew install ripgrep`, Shell},
		{`npm run build -- --watch`, Shell},
		{`npx tsc --noEmit`, Shell},
		{`cargo test --release`, Shell},
		{`python3 -m venv .venv`, Shell},
		{`pip install -r requirements.txt`, Shell},
		{`rsync -avz src/ host:/dst/`, Shell},
		{`scp file.tgz host:~/`, Shell},
		{`ps aux | grep grsh`, Shell},
		{`kill -9 12345`, Shell},
		{`chmod +x deploy.grsh`, Shell},
		{`chown -R user:staff .`, Shell},
		{`ln -sf ../bin/tool /usr/local/bin/tool`, Shell},
		{`sed -i '' 's/foo/bar/g' file.txt`, Shell},
		{`sed -n '1,10p' big.log`, Shell},
		{`xargs -n1 basename < paths.txt`, Shell},
		{`head -n 20 report.csv`, Shell},
		{`tail -f /var/log/system.log`, Shell},
		{`du -sh node_modules`, Shell},
		{`df -h /`, Shell},
		{`uname -a`, Shell},
		{`which grsh`, Shell},
		{`date +%Y-%m-%d`, Shell},
		{`base64 -d < blob.b64`, Shell},
		{`openssl rand -hex 16`, Shell},
		{`sort -u names.txt | uniq -c`, Shell},
		{`cut -d: -f1 /etc/passwd`, Shell},
		{`tr a-z A-Z < input.txt`, Shell},
		{`wc -l *.go`, Shell},
		{`touch .keep`, Shell},
		{`mkdir -p a/b/c`, Shell},
		{`rm -rf build/`, Shell},
		{`cp -R assets dist/`, Shell},
		{`mv old.name new.name`, Shell},
		{`cat <<gone.txt`, Shell}, // heredoc-ish still lexes as shell redirect
		{`diff -u a.txt b.txt`, Shell},
		{`tee out.log`, Shell},
		{`sleep 2`, Shell},
		{`true && false || true`, Shell},
		{`yes | head -3`, Shell},
		{`man 2 open`, Shell},
		{`git log --oneline -10`, Shell},
		{`git rebase -i HEAD~3`, Shell},
		{`git diff --stat main...feature`, Shell},
		{`git push -u origin main`, Shell},
		{`docker run --rm -it -v $PWD:/w alpine sh`, Shell},
		{`docker build -t app:latest .`, Shell},
		{`kubectl logs -f deploy/api`, Shell},
		{`terraform plan -out tf.plan`, Shell},
		{`aws s3 sync ./public s3://bucket`, Shell},
		{`psql -c 'select 1'`, Shell},
		{`redis-cli ping`, Shell},
		{`jq -r '.items[].name' resp.json`, Shell},
		{`curl -fsSL https://example.com | tar -xz`, Shell},
		{`wget -qO- https://example.com/health`, Shell},
		{`printf '%s\n' one two`, Shell},
		{`test -d .git`, Shell},
		{`[ -f config.toml ] && echo have config`, Shell},
		{`command -v go`, Shell},
		{`PATH=/opt/bin:$PATH tool run`, Shell},
		{`CGO_ENABLED=0 GOOS=linux go build`, Shell},
		{`go vet ./...`, Shell},
		{`go mod tidy`, Shell},
		{`gofmt -l .`, Shell},
		{`make build`, Shell},   // make is a Go builtin name, but no Go punctuation follows
		{`make -j4 all`, Shell}, // ditto
		{`time make test`, Shell},
		{`env | sort`, Shell},
		{`export EDITOR=vim`, Shell},
		{`source ~/.env.grsh`, Shell},
		{`ssh -p 2222 user@host uptime`, Shell},
		{`tar -tzf release.tgz | head`, Shell},
		{`unzip -o bundle.zip -d out`, Shell},
		{`say done`, Shell},
		{`open .`, Shell},
		{`pbcopy < token.txt`, Shell},
		{`sqlite3 app.db '.tables'`, Shell},
		{`./gradlew assemble`, Shell},
		{`~/bin/custom-tool --flag`, Shell},
		{`echo {} empty braces are literal`, Shell},
		{`grep -E '^(GET|POST) /api' access.log`, Shell},

		// --- extended corpus: Go lines ---
		{`delete(ages, "zoe")`, Go}, // Go builtin in call position
		{`total += price * qty`, Go},
		{`i--`, Go},
		{`s = strings.TrimSpace(s)`, Go},
		{`out, err := $(git status --porcelain)`, Go},
		{`n := len(items)`, Go},
		{`ok := exists("go.mod")`, Go},
		{`m := map[string]int{"a": 1}`, Go},
		{`xs := []string{}`, Go},
		{`var buf []byte`, Go},
		{`const banner = "== report =="`, Go},
		{`if err != nil {`, Go},
		{`} else if retries < 3 {`, Go},
		{`for {`, Go},
		{`for i := range 10 {`, Go},
		{`switch status() {`, Go},
		{`case 0:`, Go},
		{`return nil`, Go},
		{`defer fmt.Println("done")`, Go},
		{`json.Parse(payload)`, Go},
		{`sort.Strings(names)`, Go},
		{`logger.Info("starting", "pid", "1")`, Go},
		{`serr.New("boom")`, Go},
		{`(count > 0)`, Go},
		{`func retry(n int, f func() error) error {`, Go},
		{`import "strings"`, Go},
		{`_ = result`, Go},

		// --- documented tie-breaks ---
		// httpie-style `a:=1` unquoted contains := and classifies Go;
		// quote it or use the sh prefix to force shell.
		{`http POST :3000 a:=1`, Go},
		{`http POST :3000 'a:=1'`, Shell},
		{`sh http POST :3000 a:=1`, Shell},
	}

	scoped := map[string]bool{
		`delete(ages, "zoe")`: true, `total += price * qty`: true,
		`i--`: true, `s = strings.TrimSpace(s)`: true,
		`ok := exists("go.mod")`: true, `make build`: true,
		`make -j4 all`: true, `[ -f config.toml ] && echo have config`: true,
	}
	for _, tc := range corpus {
		c := New(testPkgs)
		// Mirror the runner: interp helpers and Go builtin funcs are
		// predeclared; some cases also rely on prior declarations.
		c.Predeclare("glob", "lines", "fields", "trim", "exists", "status",
			"ok", "errexit", "args", "sh", "capture",
			"len", "cap", "append", "delete", "copy", "make", "min", "max")
		if scoped[tc.text] {
			c.Predeclare("total", "price", "qty", "i", "s", "ages")
		}
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
