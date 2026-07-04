package scan

import (
	"reflect"
	"testing"
)

func TestLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Line
	}{
		{
			"simple",
			"ls\npwd",
			[]Line{{"ls", 1}, {"pwd", 2}},
		},
		{
			"backslash continuation",
			"echo one \\\n  two",
			[]Line{{"echo one two", 1}},
		},
		{
			"pipe continuation",
			"cat f |\n  grep x",
			[]Line{{"cat f | grep x", 1}},
		},
		{
			"and-or continuation",
			"make &&\n  echo ok ||\n  echo no",
			[]Line{{"make && echo ok || echo no", 1}},
		},
		{
			"line numbers after continuation",
			"a \\\nb\nc",
			[]Line{{"a b", 1}, {"c", 3}},
		},
		{
			"comment not continued",
			"# trailing pipe |\nls",
			[]Line{{"# trailing pipe |", 1}, {"ls", 2}},
		},
		{
			"blank preserved",
			"ls\n\npwd",
			[]Line{{"ls", 1}, {"", 2}, {"pwd", 3}},
		},
	}
	for _, tc := range tests {
		got := Lines(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Lines(%q)\n got: %#v\nwant: %#v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestIsBlankOrComment(t *testing.T) {
	for in, want := range map[string]bool{
		"":                    true,
		"   ":                 true,
		"# hi":                true,
		"#!/usr/bin/env grsh": true,
		"ls":                  false,
		"  ls # x":            false,
	} {
		if got := IsBlankOrComment(in); got != want {
			t.Errorf("IsBlankOrComment(%q) = %v, want %v", in, got, want)
		}
	}
}
