// Package shellparse parses shell command lines into an AST.
// One logical line of shell (e.g. `cat a.log | grep 500 > errs.txt && echo ok`)
// becomes a CmdList. Words are sequences of segments so that quoting,
// $VAR expansion, $(...) substitution, and {expr} Go interpolation retain
// their contexts until expansion time.
package shellparse

// CmdList is a `;`-separated sequence of and-or chains.
type CmdList struct {
	Items []*AndOr
}

// AndOr is a chain of pipelines joined by && and ||.
// len(Ops) == len(Pipes)-1; Ops[i] joins Pipes[i] and Pipes[i+1].
// Background marks a trailing & — the whole chain runs as a job.
type AndOr struct {
	Pipes      []*Pipeline
	Ops        []LogicOp
	Background bool
}

type LogicOp int

const (
	AndOp LogicOp = iota // &&
	OrOp                 // ||
)

// Pipeline is one or more commands connected by |.
type Pipeline struct {
	Cmds []*Command
}

// Command is a single command: words plus redirections, in source order.
type Command struct {
	Words  []*Word
	Redirs []Redir
}

// Word is a concatenation of segments, e.g. foo"$BAR"{baz} is three segments.
type Word struct {
	Segs []Segment
}

// Segment is one quoted/expansion context within a word.
type Segment interface{ seg() }

// Lit is literal text. Quoted literals are exempt from glob and tilde expansion.
type Lit struct {
	Text   string
	Quoted bool
}

// EnvVar is $NAME or ${NAME}. Special names: digits "1".."9" (script args),
// "@" (all script args), "#" (script arg count).
type EnvVar struct {
	Name   string
	Quoted bool // inside double quotes
}

// CmdSub is $(...). Unquoted, its trimmed output is split into fields;
// quoted, it stays a single word (bash-compatible).
type CmdSub struct {
	List   *CmdList
	Src    string // original text inside $(...), for error messages
	Quoted bool
}

// GoExpr is {expr}: a Go expression interpolated into the command.
// A string result is exactly one word (no splitting); []string splices
// into multiple words; other values go through fmt.Sprint.
type GoExpr struct {
	Src    string
	Quoted bool
}

func (Lit) seg()    {}
func (EnvVar) seg() {}
func (CmdSub) seg() {}
func (GoExpr) seg() {}

// RedirOp identifies a redirection operator.
type RedirOp int

const (
	RedirOut       RedirOp = iota // >   (fd defaults to 1)
	RedirAppend                   // >>
	RedirIn                       // <   (fd defaults to 0)
	RedirDup                      // N>&M e.g. 2>&1
	RedirOutErr                   // &>  (stdout and stderr to file)
	RedirOutErrApp                // &>>
)

// Redir is one redirection. For RedirDup, DupTo holds M and Target is nil.
type Redir struct {
	Op     RedirOp
	FD     int
	DupTo  int
	Target *Word
}
