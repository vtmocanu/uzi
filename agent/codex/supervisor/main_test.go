package main

import (
	"reflect"
	"testing"
)

func TestParseArgsValid(t *testing.T) {
	uid, child, err := parseArgs([]string{"--expect-uid", "10002", "--", "/opt/uzi-codex/0.153.2/bin/codex", "app-server"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uid != 10002 {
		t.Errorf("uid = %d, want 10002", uid)
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
			if _, _, err := parseArgs(args); err == nil {
				t.Errorf("expected an error for %v", args)
			}
		})
	}
}
