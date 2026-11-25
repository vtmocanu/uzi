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

// TestScanCommandNotFoundScrubsProviderKeyBeforeMatch: a provider judge's key must never reach a
// miss candidate, even partially (PRD #4322 M1a landing review). Three transformations can cut a
// key before an evidence-level scrub sees it whole: the 200-byte evidence clip, the pattern match
// (its token class stops at `/` and `%`), and the 512 KiB scan-budget cut (the zsh form ends in
// the token). The scrub therefore runs on each whole row first. A plain `jq` miss in the same
// row is the control: the scan still finds real gaps. Without a scrub nothing changes.
func TestScanCommandNotFoundScrubsProviderKeyBeforeMatch(t *testing.T) {
	long := "gwk-" + strings.Repeat("A1b2", 60)    // 244 command-token chars: the clip cuts it
	slashed := "gwp/" + strings.Repeat("C3d4", 10) // the exec form basenames it to a key suffix
	zshAt := judgeScanByteBudget - 20              // the budget cut lands 20 bytes into the key
	cases := []struct {
		name, key, payload string
	}{
		{"evidence clip", long, `{"text":"bash: jq: command not found\nbash: ` + long + `: command not found"}`},
		{"command token is a key suffix", slashed, `{"text":"bash: jq: command not found\nexec: \"` + slashed + `\": executable file not found in $PATH"}`},
		{"scan budget cut", long, func() string {
			head := `{"text":"bash: jq: command not found\n`
			lead := " zsh: command not found: "
			return head + strings.Repeat(" ", zshAt-len(head)-len(lead)) + lead + long + `"}`
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := rawResultRows(tc.payload)
			leaked := func(ms []missCandidate) bool {
				for _, m := range ms {
					for _, s := range []string{m.Command, m.Evidence} {
						for i := 0; i+12 <= len(tc.key); i++ {
							if len(s) >= 12 && strings.Contains(s, tc.key[i:i+12]) {
								return true
							}
						}
					}
				}
				return false
			}
			if !leaked(scanCommandNotFound(rows)) {
				t.Fatal("fixture must leak key material without a scrub")
			}
			got := scanCommandNotFoundScrubbed(rows, judgeProviderScrub(tc.key))
			if leaked(got) {
				t.Fatalf("key material reached a candidate: %+v", got)
			}
			if len(got) != 1 || got[0].Command != "jq" {
				t.Fatalf("want only the jq control candidate, got %+v", got)
			}
		})
	}
}
