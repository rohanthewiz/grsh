package shellparse

import (
	"fmt"
	"strings"
	"testing"
)

// dump renders a CmdList compactly for table tests.
func dump(l *CmdList) string {
	var items []string
	for _, ao := range l.Items {
		var pipes []string
		for i, pl := range ao.Pipes {
			var cmds []string
			for _, c := range pl.Cmds {
				cmds = append(cmds, dumpCmd(c))
			}
			s := strings.Join(cmds, " | ")
			if i > 0 {
				op := "&&"
				if ao.Ops[i-1] == OrOp {
					op = "||"
				}
				s = op + " " + s
			}
			pipes = append(pipes, s)
		}
		s := strings.Join(pipes, " ")
		if ao.Background {
			s += " &"
		}
		items = append(items, s)
	}
	return strings.Join(items, " ; ")
}

func dumpCmd(c *Command) string {
	var parts []string
	for _, w := range c.Words {
		parts = append(parts, dumpWord(w))
	}
	for _, r := range c.Redirs {
		parts = append(parts, dumpRedir(r))
	}
	return strings.Join(parts, " ")
}

func dumpWord(w *Word) string {
	return dumpSegs(w.Segs)
}

func dumpSegs(segs []Segment) string {
	var b strings.Builder
	for _, seg := range segs {
		switch s := seg.(type) {
		case Lit:
			if s.Quoted {
				fmt.Fprintf(&b, "q(%s)", s.Text)
			} else {
				b.WriteString(s.Text)
			}
		case EnvVar:
			fmt.Fprintf(&b, "var(%s)", s.Name)
		case CmdSub:
			fmt.Fprintf(&b, "sub(%s)", dump(s.List))
		case GoExpr:
			fmt.Fprintf(&b, "go(%s)", s.Src)
		}
	}
	return b.String()
}

func dumpRedir(r Redir) string {
	switch r.Op {
	case RedirOut:
		return fmt.Sprintf("%d>%s", r.FD, dumpWord(r.Target))
	case RedirAppend:
		return fmt.Sprintf("%d>>%s", r.FD, dumpWord(r.Target))
	case RedirIn:
		return fmt.Sprintf("<%s", dumpWord(r.Target))
	case RedirDup:
		return fmt.Sprintf("%d>&%d", r.FD, r.DupTo)
	case RedirOutErr:
		return fmt.Sprintf("&>%s", dumpWord(r.Target))
	case RedirOutErrApp:
		return fmt.Sprintf("&>>%s", dumpWord(r.Target))
	case RedirHeredoc:
		h := r.Here
		op := "<<"
		if h.StripTabs {
			op = "<<-"
		}
		if h.Quoted {
			return fmt.Sprintf("%s'%s'[%s]", op, h.Delim, dumpSegs(h.Segs))
		}
		return fmt.Sprintf("%s%s[%s]", op, h.Delim, dumpSegs(h.Segs))
	}
	return "?"
}

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`ls`, `ls`},
		{`ls -la /tmp`, `ls -la /tmp`},
		{`cat a.log | grep 500 | wc -l`, `cat a.log | grep 500 | wc -l`},
		{`make && echo ok || echo fail`, `make && echo ok || echo fail`},
		{`cd /tmp; ls`, `cd /tmp ; ls`},
		{`echo hi > out.txt`, `echo hi 1>out.txt`},
		{`echo hi >> out.txt`, `echo hi 1>>out.txt`},
		{`wc -l < in.txt`, `wc -l <in.txt`},
		{`cmd 2> err.txt`, `cmd 2>err.txt`},
		{`cmd 2>> err.txt`, `cmd 2>>err.txt`},
		{`cmd > all.txt 2>&1`, `cmd 1>all.txt 2>&1`},
		{`cmd &> all.txt`, `cmd &>all.txt`},
		{`echo 'single $NOEXP'`, `echo q(single $NOEXP)`},
		{`echo "hi $USER"`, `echo q(hi )var(USER)`},
		{`echo $HOME/bin`, `echo var(HOME)/bin`},
		{`echo ${HOME}stuff`, `echo var(HOME)stuff`},
		{`echo $(date +%s)`, `echo sub(date +%s)`},
		{`echo a$(echo b)c`, `echo asub(echo b)c`},
		{`echo {f}`, `echo go(f)`},
		{`echo {x + 1}`, `echo go(x + 1)`},
		{`echo "{x}"`, `echo go(x)`},
		{`find . -name '*.go' -exec wc {} \;`, `find . -name q(*.go) -exec wc {} q(;)`},
		{`awk '{print $1}' f.txt`, `awk q({print $1}) f.txt`},
		{`echo a\ b`, `echo aq( )b`},
		{`echo $@ $# $1`, `echo var(@) var(#) var(1)`},
		{`ls # a comment`, `ls`},
		{`echo hi#nothash`, `echo hi#nothash`},
		{`grep -v foo|sort`, `grep -v foo | sort`},
		{`echo "escaped \" quote"`, `echo q(escaped " quote)`},
		{`echo ""`, `echo q()`},
		{`true;`, `true`},
		{`sleep 5 &`, `sleep 5 &`},
		{`sleep 5&`, `sleep 5 &`},
		{`make build && notify done &`, `make build && notify done &`},
		{`sleep 1 & echo now`, `sleep 1 & ; echo now`},
		{`sleep 1 & sleep 2 &`, `sleep 1 & ; sleep 2 &`},
		{`sleep 1 &; echo hi`, `sleep 1 & ; echo hi`},
		{`cmd | tee log &`, `cmd | tee log &`},
		{`cmd &> all.txt &`, `cmd &>all.txt &`},
	}
	for _, tc := range tests {
		got, err := Parse(tc.in)
		if err != nil {
			t.Errorf("Parse(%q) error: %v", tc.in, err)
			continue
		}
		if d := dump(got); d != tc.want {
			t.Errorf("Parse(%q)\n got: %s\nwant: %s", tc.in, d, tc.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		in      string
		errPart string
	}{
		{`echo 'unterminated`, "unterminated single"},
		{`echo "unterminated`, "unterminated double"},
		{`echo $(no close`, "missing closing"},
		{`echo {no close`, "unmatched '{'"},
		{`| sort`, "missing command before |"},
		{`ls | | sort`, "missing command"},
		{`ls &&`, "missing command after"},
		{`& ls`, "missing command before &"},
		{`ls && &`, "missing command after"},
		{`echo hi \`, "trailing backslash"},
		{`cmd >`, "missing redirection target"},
	}
	for _, tc := range tests {
		_, err := Parse(tc.in)
		if err == nil {
			t.Errorf("Parse(%q): expected error containing %q, got nil", tc.in, tc.errPart)
			continue
		}
		if !strings.Contains(err.Error(), tc.errPart) {
			t.Errorf("Parse(%q) error = %q, want it to contain %q", tc.in, err, tc.errPart)
		}
	}
}

func TestParseHeredocs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"basic", "cat <<EOF\nhello\nworld\nEOF", "cat <<EOF[q(hello\nworld\n)]"},
		{"expansion", "cat <<EOF\nhi $USER\nEOF", "cat <<EOF[q(hi )var(USER)q(\n)]"},
		{"cmdsub", "cat <<EOF\nnow: $(date)\nEOF", "cat <<EOF[q(now: )sub(date)q(\n)]"},
		{"escaped dollar", "cat <<EOF\ncost \\$5\nEOF", "cat <<EOF[q(cost $5\n)]"},
		{"braces literal", "cat <<EOF\n{\"a\": 1}\nEOF", "cat <<EOF[q({\"a\": 1}\n)]"},
		{"quoted delim single", "cat <<'EOF'\nno $EXPAND\nEOF", "cat <<'EOF'[q(no $EXPAND\n)]"},
		{"quoted delim double", "cat <<\"EOF\"\nno $EXPAND\nEOF", "cat <<'EOF'[q(no $EXPAND\n)]"},
		{"strip tabs", "cat <<-EOF\n\tindented\n\tEOF", "cat <<-EOF[q(indented\n)]"},
		{"empty body", "cat <<EOF\nEOF", "cat <<EOF[]"},
		{"delim at eof no newline", "cat <<EOF\nbody\nEOF\n", "cat <<EOF[q(body\n)]"},
		{"two heredocs", "cat <<A <<B\nfirst\nA\nsecond\nB", "cat <<A[q(first\n)] <<B[q(second\n)]"},
		{"in pipeline", "cat <<EOF | wc -l\none\nEOF", "cat <<EOF[q(one\n)] | wc -l"},
		{"command after body", "cat <<EOF\nbody\nEOF\necho done", "cat <<EOF[q(body\n)] ; echo done"},
		{"background", "cat <<EOF &\nbody\nEOF", "cat <<EOF[q(body\n)] &"},
		{"comment on heredoc line", "cat <<EOF # note\nbody\nEOF", "cat <<EOF[q(body\n)]"},
		{"newline separates commands", "echo a\necho b", "echo a ; echo b"},
		{"fd prefix", "cat 0<<EOF\nx\nEOF", "cat <<EOF[q(x\n)]"},
	}
	for _, tc := range tests {
		got, err := Parse(tc.in)
		if err != nil {
			t.Errorf("%s: Parse(%q) error: %v", tc.name, tc.in, err)
			continue
		}
		if d := dump(got); d != tc.want {
			t.Errorf("%s: Parse(%q)\n got: %s\nwant: %s", tc.name, tc.in, d, tc.want)
		}
	}
}

func TestParseHeredocErrors(t *testing.T) {
	tests := []struct {
		in      string
		errPart string
	}{
		{"cat <<EOF", "unterminated heredoc <<EOF (missing body)"},
		{"cat <<EOF\nbody", "missing closing delimiter"},
		{"cat <<EOF\nbody\nEO", "missing closing delimiter"},
		{"cat <<", "missing heredoc delimiter"},
		{"cat <<'EOF\nx\nEOF", "unterminated quote in heredoc delimiter"},
		{"cat <<EOF | wc\nEOF ", "missing closing delimiter"}, // trailing space ≠ delimiter
		{"echo $(cat <<EOF)\nbody\nEOF", "unterminated heredoc"},
	}
	for _, tc := range tests {
		_, err := Parse(tc.in)
		if err == nil {
			t.Errorf("Parse(%q): expected error containing %q, got nil", tc.in, tc.errPart)
			continue
		}
		if !strings.Contains(err.Error(), tc.errPart) {
			t.Errorf("Parse(%q) error = %q, want it to contain %q", tc.in, err, tc.errPart)
		}
	}
}
