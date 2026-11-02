package main

import (
	"os"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// TestRecoveryRestoreDriver is compiled into a test binary by the handler fixture.
// It has no production entry point, flags, default store, or implicit home writes.
func TestRecoveryRestoreDriver(t *testing.T) {
	if os.Getenv("RECOVERY_RESTORE_DRIVER") != "1" {
		return
	}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "UZI_") {
			t.Fatal("unexpected inherited UZI input")
		}
	}
	split := -1
	for i, arg := range os.Args {
		if arg == "--" {
			split = i
			break
		}
	}
	if split < 0 {
		t.Fatal("missing driver arguments")
	}
	env := Env{
		Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin,
		NewClient:        func(s uzicli.Settings) uzicli.Client { return uzicli.NewHTTPClient(s) },
		Store:            uzicli.NewStore(os.Getenv("RECOVERY_RESTORE_CONFIG")),
		SkillHome:        os.Getenv("RECOVERY_RESTORE_SKILL_HOME"),
		Getenv:           func(string) string { return "" },
		AutoUpgradeSkill: false, CheckServerVersion: false,
	}
	os.Exit(Main(env, os.Args[split+1:]))
}
