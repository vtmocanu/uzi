package main

import "testing"

func TestParseStatPPidPgid(t *testing.T) {
	// A normal stat line: pid (comm) state ppid pgid ...
	stat := "42 (codex) S 7 42 42 0 -1 4194304 100 0 0 0 1 2 0 0 20 0 1 0 999 0 0"
	ppid, pgid, err := parseStatPPidPgid(stat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ppid != 7 || pgid != 42 {
		t.Errorf("ppid/pgid = %d/%d, want 7/42", ppid, pgid)
	}
}

func TestParseStatPPidPgidCommWithSpacesAndParens(t *testing.T) {
	// A hostile comm containing spaces AND a ") " sequence must not confuse the
	// last-") " split — the fields after the FINAL ") " are the real ones.
	stat := "99 (weird ) name) S 7 55 55 0 -1 0 0 0"
	ppid, pgid, err := parseStatPPidPgid(stat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ppid != 7 || pgid != 55 {
		t.Errorf("ppid/pgid = %d/%d, want 7/55", ppid, pgid)
	}
}

func TestParseStatPPidPgidMalformed(t *testing.T) {
	for _, s := range []string{"", "no-comm-terminator", "42 (comm) S"} {
		if _, _, err := parseStatPPidPgid(s); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
}

func TestSanitizeComm(t *testing.T) {
	if got := sanitizeComm("codex\n"); got != "codex" {
		t.Errorf("got %q, want codex", got)
	}
	// Cap at the kernel's 15-char comm length.
	long := "abcdefghijklmnopqrstuvwxyz"
	if got := sanitizeComm(long); len(got) != 15 || got != long[:15] {
		t.Errorf("got %q (len %d), want first 15 chars", got, len(got))
	}
}
