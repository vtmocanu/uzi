package workersvc

import (
	"encoding/json"
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
// (its token class stops at `/` and `%`), the 512 KiB scan-budget cut (the zsh form ends in the
// token) and a JSON-escaped ANSI separator inside the key. The provider scan therefore decodes
// each whole row, strips invisible runs and scrubs before matching, and omits a row it cannot
// inspect whole. A plain `jq` miss is the control: the scan still finds real gaps. Without a
// scrub nothing changes.
func TestScanCommandNotFoundScrubsProviderKeyBeforeMatch(t *testing.T) {
	long := "gwk-" + strings.Repeat("A1b2", 60)    // 244 command-token chars: the clip cuts it
	slashed := "gwp/" + strings.Repeat("C3d4", 10) // the exec form basenames it to a key suffix
	control := `{"text":"bash: jq: command not found"}`
	cases := []struct {
		name, key string
		payloads  []string
	}{
		{"evidence clip", long, []string{`{"text":"bash: jq: command not found\nbash: ` + long + `: command not found"}`}},
		{"command token is a key suffix", slashed, []string{`{"text":"bash: jq: command not found\nexec: \"` + slashed + `\": executable file not found in $PATH"}`}},
		{"scan budget cut", long, []string{control, func() string {
			// The budget cut would land 20 bytes into the key; the provider scan omits the row.
			head, lead := `{"text":"`, " zsh: command not found: "
			return head + strings.Repeat(" ", judgeScanByteBudget-len(control)-20-len(head)-len(lead)) + lead + long + `"}`
		}()}},
		{"escaped ANSI separator inside the key", long, []string{
			// A real ESC in tool output is serialized into the JSONB text as \u001b.
			`{"text":"bash: jq: command not found\nbash: ` + long[:20] + `\u001b[0m` + long[20:] + `: command not found"}`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := rawResultRows(tc.payloads...)
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

// TestScanCommandNotFoundProviderKeepsSeparators: the provider scrub keeps every separator outside
// a key, and each decoded string value is matched on its own, so ordinary multiline output and
// adjacent fields never merge into a synthetic command (PRD #4322 M1a landing review).
func TestScanCommandNotFoundProviderKeepsSeparators(t *testing.T) {
	scrub := judgeProviderScrub("gwk-" + strings.Repeat("A1b2", 10))
	for name, payload := range map[string]string{
		"newline before the miss": `{"text":"server ready\njq: command not found"}`,
		"ANSI before the miss":    `{"text":"server ready\u001b[0m\njq: command not found"}`,
		"adjacent fields":         `{"a":"server ready","b":"jq: command not found"}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := scanCommandNotFoundScrubbed(rawResultRows(payload), scrub)
			if len(got) != 1 || got[0].Command != "jq" {
				t.Fatalf("want exactly jq, got %+v", got)
			}
		})
	}
}

// TestJudgeProviderScrubOverlappingMatches: a key built from two identical blocks must leave no
// block behind when a verbatim occurrence overlaps an invisible-separated one, or when
// invisible-separated occurrences overlap each other (PR #4678 review).
func TestJudgeProviderScrubOverlappingMatches(t *testing.T) {
	block := "gwk-A1b2C3d4E5f6G7h8"
	scrub := judgeProviderScrub(block + block)
	const zw = "\u200b"
	for name, in := range map[string]string{
		"verbatim overlaps invisible":  block + block + zw + block + ": command not found",
		"invisible overlaps invisible": block + zw + block + zw + block + ": command not found",
	} {
		t.Run(name, func(t *testing.T) {
			got := scrub(in)
			if strings.Contains(strings.ReplaceAll(got, zw, ""), block) {
				t.Fatalf("a key block survived the scrub: %q", got)
			}
			if !strings.HasSuffix(got, ": command not found") {
				t.Fatalf("text outside the key changed: %q", got)
			}
			payload, _ := json.Marshal(map[string]string{"text": in})
			if cands := scanCommandNotFoundScrubbed(rawResultRows(string(payload)), scrub); len(cands) != 0 {
				t.Fatalf("a key block reached a candidate: %+v", cands)
			}
		})
	}
}
