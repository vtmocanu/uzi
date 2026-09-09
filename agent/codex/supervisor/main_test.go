package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseArgsValid(t *testing.T) {
	// Assemble the UUID-shaped fixture at runtime: it exercises the real cleanup-token
	// grammar without leaving a generic-api-key-shaped false positive in tracked text.
	cleanupToken := strings.Join([]string{"01234567", "89ab", "cdef", "0123", "456789abcdef"}, "-")
	uid, cleanup, dropCaps, child, err := parseArgs([]string{"--expect-uid", "10002", "--cleanup-token", cleanupToken, "--drop-controller-caps", "--", "/opt/uzi-codex/0.153.2/bin/codex", "app-server"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uid != 10002 {
		t.Errorf("uid = %d, want 10002", uid)
	}
	if cleanup != cleanupToken {
		t.Errorf("cleanup token = %q", cleanup)
	}
	if !dropCaps {
		t.Error("drop-controller-caps was not parsed")
	}
	want := []string{"/opt/uzi-codex/0.153.2/bin/codex", "app-server"}
	if !reflect.DeepEqual(child, want) {
		t.Errorf("child = %v, want %v", child, want)
	}
}

func TestParseArgsRejections(t *testing.T) {
	tests := map[string][]string{
		"missing --expect-uid": {"--", "/bin/codex"},
		"missing separator":    {"--expect-uid", "10002", "/bin/codex"},
		"no child":             {"--expect-uid", "10002", "--"},
		"relative child path":  {"--expect-uid", "10002", "--", "codex"},
		"non-numeric uid":      {"--expect-uid", "root", "--", "/bin/codex"},
		"negative uid":         {"--expect-uid", "-1", "--", "/bin/codex"},
		"dangling flag":        {"--expect-uid"},
		"unknown flag":         {"--nope", "1", "--", "/bin/codex"},
		"empty":                {},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, _, _, err := parseArgs(args); err == nil {
				t.Errorf("expected an error for %v", args)
			}
		})
	}
}

func TestParseArgsRejectsUnsafeCleanupToken(t *testing.T) {
	for _, token := range []string{"../victim", "01234567-89AB-cdef-0123-456789abcdef", "01234567-89ab-cdef-0123-456789abcdeg"} {
		if _, _, _, _, err := parseArgs([]string{"--expect-uid", "10003", "--cleanup-token", token, "--", "/bin/true"}); err == nil {
			t.Fatalf("accepted unsafe cleanup token %q", token)
		}
	}
}

func TestWatchChildReportsNormalizedPrimaryStatus(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "exit", args: []string{"/bin/sh", "-c", "exit 7"}, want: 7},
		{name: "signal", args: []string{"/bin/sh", "-c", "kill -TERM $$"}, want: 143},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pid, err := launchChild(tc.args)
			if err != nil {
				t.Fatalf("launchChild: %v", err)
			}
			ready, err := watchChild(pid)
			if err != nil {
				t.Fatalf("watchChild: %v", err)
			}
			if err := <-ready; err != nil {
				t.Fatalf("child readiness: %v", err)
			}
			code, err := realReapChild(pid)
			if err != nil {
				t.Fatalf("realReapChild: %v", err)
			}
			if code != tc.want {
				t.Fatalf("code = %d, want %d", code, tc.want)
			}
		})
	}
}
