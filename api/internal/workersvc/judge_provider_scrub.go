package workersvc

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// judgeProviderRedacted is the marker the worker's provider redactor uses (agent/src/redact.ts
// REDACTED), so a server-scrubbed span reads the same as a worker-scrubbed one.
const judgeProviderRedacted = "***REDACTED***"

// judgeInvisible mirrors agent/src/redact.ts INVISIBLE_SRC: ANSI CSI sequences and Unicode
// control/format characters.
var judgeInvisible = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|[\p{Cc}\p{Cf}]`)

// providerKeyFormsGo mirrors agent/src/redact.ts providerKeyForms: the raw key, its
// JSON-escaped body (as JSON.stringify writes it), that body with `\/`, the HTML-safe `\u00XX`
// forms Go's encoding/json emits (both hex cases), and the percent-encoded forms
// (encodeURIComponent, both hex cases, plus `+` for a space). Longest first.
func providerKeyFormsGo(key string) []string {
	if key == "" {
		return nil
	}
	body := jsStringifyBody(key)
	htmlSafe := func(upper bool) string {
		var b strings.Builder
		for _, r := range body {
			switch r {
			case '<', '>', '&', ' ', ' ':
				hex := fmt.Sprintf("%04x", r)
				if upper {
					hex = strings.ToUpper(hex)
				}
				b.WriteString(`\u` + hex)
			default:
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	pct := encodeURIComponent(key)
	forms := []string{
		key, body, strings.ReplaceAll(body, "/", `\/`), htmlSafe(false), htmlSafe(true),
		pct, lowerPercentHex(pct),
	}
	if strings.Contains(key, " ") {
		forms = append(forms, strings.ReplaceAll(pct, "%20", "+"))
	}
	seen := map[string]bool{}
	out := forms[:0]
	for _, f := range forms {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// judgeProviderScrub returns the evidence scrub for a provider-bound judge claim: strip
// invisible runs (so a key split by them is contiguous again), then replace every key form.
// It runs BEFORE the pre-scan clips evidence, so a key straddling the clip cannot reach the
// worker as a partial prefix its exact-form redactor no longer matches. A nil result (no
// credential) leaves vendor-mode evidence byte-identical.
func judgeProviderScrub(credential string) func(string) string {
	forms := providerKeyFormsGo(credential)
	if len(forms) == 0 {
		return nil
	}
	return func(s string) string {
		s = judgeInvisible.ReplaceAllString(s, "")
		for _, f := range forms {
			s = strings.ReplaceAll(s, f, judgeProviderRedacted)
		}
		return s
	}
}

// jsStringifyBody is JSON.stringify(s) without its surrounding quotes: `"` and `\` escaped,
// \b \f \n \r \t short forms, other C0 controls as lowercase \u00xx, everything else raw.
func jsStringifyBody(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// encodeURIComponent mirrors the JavaScript function: A-Z a-z 0-9 and -_.!~*'() stay, every
// other UTF-8 byte becomes %XX (uppercase).
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

var percentHex = regexp.MustCompile(`%[0-9A-F]{2}`)

func lowerPercentHex(s string) string {
	return percentHex.ReplaceAllStringFunc(s, strings.ToLower)
}

// decodedPayloadStrings decodes a tool_result payload and joins every string value it holds
// (object keys in sorted order, so the text is deterministic) with newlines. The provider pre-scan
// matches this decoded text, so a JSON escape (`\u001b`, `\/`, `\"`) can no longer hide a key from
// the scrub. A payload that does not decode reports false and is omitted, never matched raw.
func decodedPayloadStrings(payload []byte) (string, bool) {
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return "", false
	}
	var parts []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			parts = append(parts, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		}
	}
	walk(v)
	return strings.Join(parts, "\n"), true
}
