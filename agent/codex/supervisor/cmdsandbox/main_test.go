package main

import (
	"errors"
	"os"
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

func TestRunOnLockedThreadPinsBeforePolicyWork(t *testing.T) {
	order := make([]string, 0, 2)
	got := runOnLockedThread(
		[]string{"arg"},
		func() { order = append(order, "lock") },
		func(args []string) int {
			order = append(order, "run")
			if !reflect.DeepEqual(args, []string{"arg"}) {
				t.Fatalf("unexpected args: %v", args)
			}
			return 7
		},
	)
	if got != 7 || !reflect.DeepEqual(order, []string{"lock", "run"}) {
		t.Fatalf("runOnLockedThread: code=%d order=%v", got, order)
	}
}

func TestParseArgsRejectsEscape(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "sibling", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/other", "--", "/bin/true"}},
		{name: "prefix collision", args: []string{"--root", "/data/run", "--tmp", "/tmp/run", "--cwd", "/data/runner", "--", "/bin/true"}},
		{name: "filesystem root", args: []string{"--root", "/", "--tmp", "/tmp/run", "--cwd", "/", "--", "/bin/true"}},
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

func TestConfinementDenyProbe(t *testing.T) {
	readable := func(string) (*os.File, error) { return os.Open(os.DevNull) }
	denied := func(string) (*os.File, error) { return nil, os.ErrPermission }
	failed := func(string) (*os.File, error) { return nil, errors.New("probe failed") }

	if err := requireProbeReadable(landlockDenyProbe, readable); err != nil {
		t.Fatalf("readable pre-probe rejected: %v", err)
	}
	if err := requireProbeReadable(landlockDenyProbe, denied); err == nil {
		t.Fatal("pre-probe denial must fail closed")
	}
	if err := requireProbeDenied(landlockDenyProbe, denied); err != nil {
		t.Fatalf("post-probe permission denial rejected: %v", err)
	}
	if err := requireProbeDenied(landlockDenyProbe, readable); err == nil {
		t.Fatal("readable post-probe must fail closed")
	}
	if err := requireProbeDenied(landlockDenyProbe, failed); err == nil {
		t.Fatal("unexpected post-probe error must fail closed")
	}
}
