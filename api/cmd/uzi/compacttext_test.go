package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCompactTextRuneSafe pins that compactText truncates on RUNES, not bytes (issue
// #554): a multibyte rune straddling the 200-unit cap must not be split into an orphan
// continuation byte. This guards the DIRECT callers (compactPayload, the steer-queue
// body column) — unlike TestVersionCommandOutputStaysValidUTF8, whose cellText path
// re-encodes an orphan to U+FFFD downstream and so could not catch a byte-slice here.
//
// The payload is 199 ASCII bytes then U+20AC (3 bytes): a byte slice at 200 would keep
// exactly one byte of the euro sign, yielding invalid UTF-8 and no clean truncation.
func TestCompactTextRuneSafe(t *testing.T) {
	in := strings.Repeat("A", 199) + strings.Repeat("€", 4)
	got := compactText(in)

	if !utf8.ValidString(got) {
		t.Errorf("compactText emitted invalid UTF-8 — the cap split a multibyte rune:\n%q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("compactText left a U+FFFD replacement char at the cut — the rune was split:\n%q", got)
	}
	// Truncation still happened (the input exceeds 200 runes) and the ellipsis marks it.
	if !strings.HasSuffix(got, "…") {
		t.Errorf("compactText did not truncate an over-length input (no ellipsis):\n%q", got)
	}
	// The cap is 200 CONTENT runes plus the ellipsis: 199 'A' + one '€' = 200, then '…'.
	if n := utf8.RuneCountInString(got); n != 201 {
		t.Errorf("compactText kept %d runes, want 201 (200 content + ellipsis)", n)
	}
}
