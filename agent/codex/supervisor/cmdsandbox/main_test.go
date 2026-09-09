package main

import (
	"reflect"
	"testing"
)

func TestParseArgs(t *testing.T) {
	root, tmp, cwd, child, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run/sub", "--", "/bin/sh", "-c", "true",
	})
	if err != nil || root != "/data/run" || tmp != "/tmp/run" || cwd != "/data/run/sub" || !reflect.DeepEqual(child, []string{"/bin/sh", "-c", "true"}) {
		t.Fatalf("unexpected parse: root=%q tmp=%q cwd=%q child=%v err=%v", root, tmp, cwd, child, err)
	}
}

func TestParseArgsRejectsEscape(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "sibling", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/other", "--", "/bin/true"}},
		{name: "prefix collision", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/runner", "--", "/bin/true"}},
		{name: "relative root", args: []string{"--root", "data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--", "/bin/true"}},
		{name: "relative tmp", args: []string{"--root", "/data/run", "--tmp", "tmp/run", "--cwd", "/data/run", "--", "/bin/true"}},
		{name: "relative cwd", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "data/run", "--", "/bin/true"}},
		{name: "relative child", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run", "--", "bin/true"}},
		{name: "malformed flag order", args: []string{"--tmp", "/tmp/run", "--root", "/data/run", "--cwd", "/data/run", "--", "/bin/true"}},
		{name: "too short", args: []string{"--root", "/data/run", "--tmp", "/tmp/run"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, _, err := parseArgs(test.args); err == nil {
				t.Fatal("expected argument rejection")
			}
		})
	}
}
