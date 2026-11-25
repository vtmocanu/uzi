package workersvc

import (
	"reflect"
	"strings"
	"testing"
)

// TestProviderKeyFormsGoMatchesWorker pins the Go port to the worker's providerKeyForms
// (agent/src/redact.ts). Expected values were produced by the TypeScript function itself.
func TestProviderKeyFormsGoMatchesWorker(t *testing.T) {
	const bs = "\\" // one backslash
	k1 := "gw-key/do-not-scan-7777<abcdefgh&z"
	k2 := "a b\"c\\d\té€ x"
	cases := map[string][]string{
		k1: {
			"gw-key/do-not-scan-7777" + bs + "u003cabcdefgh" + bs + "u0026z",
			"gw-key/do-not-scan-7777" + bs + "u003Cabcdefgh" + bs + "u0026z",
			"gw-key%2Fdo-not-scan-7777%3Cabcdefgh%26z",
			"gw-key%2fdo-not-scan-7777%3cabcdefgh%26z",
			"gw-key" + bs + "/do-not-scan-7777<abcdefgh&z",
			k1,
		},
		k2: {
			"a%20b%22c%5Cd%09%C3%A9%E2%82%AC%E2%80%A8x",
			"a%20b%22c%5cd%09%c3%a9%e2%82%ac%e2%80%a8x",
			"a+b%22c%5Cd%09%C3%A9%E2%82%AC%E2%80%A8x",
			"a b" + bs + "\"c" + bs + bs + "d" + bs + "té€" + bs + "u2028x",
			"a b" + bs + "\"c" + bs + bs + "d" + bs + "té€ x",
			k2,
		},
	}
	for key, want := range cases {
		if got := providerKeyFormsGo(key); !reflect.DeepEqual(got, want) {
			t.Errorf("providerKeyFormsGo(%q):\n got %q\nwant %q", key, got, want)
		}
	}
	if providerKeyFormsGo("") != nil || judgeProviderScrub("") != nil {
		t.Error("an empty credential must yield no forms and no scrub")
	}
}

// TestScanCommandNotFoundScrubsProviderKeyBeforeClip: a long provider key run as a command
// makes the evidence match longer than judgeEvidenceMaxLen, so the clip cuts the key. The
// provider scrub must run before the clip, so no partial prefix reaches the worker, whose
// exact-form redactor could no longer match it (PRD #4322 M1a landing review). Without a
// scrub the evidence is unchanged.
func TestScanCommandNotFoundScrubsProviderKeyBeforeClip(t *testing.T) {
	key := "gwk-" + strings.Repeat("A1b2", 60) // 244 chars, all command-token characters
	rows := rawResultRows(`{"text":"bash: ` + key + `: command not found"}`)

	plain := scanCommandNotFound(rows)
	if len(plain) != 1 || !strings.Contains(plain[0].Evidence, key[:40]) || strings.Contains(plain[0].Evidence, key) {
		t.Fatalf("fixture must leave a cut key in the unscrubbed evidence; got %+v", plain)
	}

	got := scanCommandNotFoundScrubbed(rows, judgeProviderScrub(key))
	if len(got) != 1 {
		t.Fatalf("want one candidate, got %+v", got)
	}
	for _, s := range []string{got[0].Evidence, got[0].Command} {
		if strings.Contains(s, key[:12]) {
			t.Errorf("a key prefix survived: %q", s)
		}
	}
	if got[0].Evidence != judgeProviderRedacted+": command not found" {
		t.Errorf("the key must be replaced by the marker before the clip: %q", got[0].Evidence)
	}
}
