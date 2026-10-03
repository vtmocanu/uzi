package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBrewOwnerFromExecutable(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"Cellar/uzi-cli/0.85.0/bin/uzi", "uzi-cli"},
		{"Cellar/uzi-cli-rc/0.85.0/bin/uzi", "uzi-cli-rc"},
		{"opt/uzi-cli-rc/bin/uzi", "uzi-cli-rc"},
		{"Cellar/uzi-cli-rc-extra/0.85.0/bin/uzi", ""},
		{"Cellar/uzi-cli-rc/0.85.0/libexec/uzi", ""},
		{"Cellar/uzi-cli-rc/0.85.0/bin/wrapper", ""},
		{"handbuilt/uzi-cli-rc/bin/uzi", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), tc.path)
			if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(exe, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if got := brewOwnerFromExecutable(func() (string, error) { return exe, nil }); got != tc.want {
				t.Fatalf("owner = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBrewOwnerResolvesOptSymlink(t *testing.T) {
	dir := t.TempDir()
	keg := filepath.Join(dir, "Cellar", "uzi-cli-rc", "0.85.0")
	if err := os.MkdirAll(filepath.Join(keg, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keg, "bin", "uzi"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "opt"), 0700); err != nil {
		t.Fatal(err)
	}
	opt := filepath.Join(dir, "opt", "uzi-cli-rc")
	if err := os.Symlink(keg, opt); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(opt, "bin", "uzi")
	if got := brewOwnerFromExecutable(func() (string, error) { return exe, nil }); got != "uzi-cli-rc" {
		t.Fatalf("symlink owner = %q", got)
	}
}

func TestBrewOwnerUnknown(t *testing.T) {
	for _, executable := range []func() (string, error){
		nil,
		func() (string, error) { return "", errors.New("unavailable") },
		func() (string, error) { return "Cellar/uzi-cli-rc/1/bin/uzi", nil },
		func() (string, error) { return filepath.Join(t.TempDir(), "missing", "uzi"), nil },
	} {
		if got := brewOwnerFromExecutable(executable); got != "" {
			t.Fatalf("unknown executable owner = %q", got)
		}
	}
}
