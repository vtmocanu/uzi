package main

import "testing"

func TestParseArgs(t *testing.T) {
	root, tmp, cwd, child, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/run/sub", "--", "/bin/sh", "-c", "true",
	})
	if err != nil || root != "/data/run" || tmp != "/tmp/run" || cwd != "/data/run/sub" || len(child) != 3 {
		t.Fatalf("unexpected parse: root=%q tmp=%q cwd=%q child=%v err=%v", root, tmp, cwd, child, err)
	}
}

func TestParseArgsRejectsEscape(t *testing.T) {
	if _, _, _, _, err := parseArgs([]string{
		"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/other", "--", "/bin/true",
	}); err == nil {
		t.Fatal("expected cwd escape rejection")
	}
}
