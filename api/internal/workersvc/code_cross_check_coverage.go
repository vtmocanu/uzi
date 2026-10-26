package workersvc

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

const (
	codeCoverageBytes             = 128 << 10
	codeCoverageEntries           = 128
	codeCoveragePathBytes         = 512
	codeCoverageReasonBytes       = 256
	codeCoverageSafeInteger int64 = 9007199254740991
)

// NormalizeCodeCrossCheckCoverage independently validates a worker account.
// Parsing and entry traversal are bounded by codeCoverageBytes. Invalid entries
// are dropped independently; ambiguous accounts discard all entries. Neither
// changes the verdict findings.
func NormalizeCodeCrossCheckCoverage(outcome string, input ...json.RawMessage) []byte {
	if outcome != "completed" || len(input) == 0 {
		return nil
	}
	fallback := func(reason string) []byte {
		raw, _ := json.Marshal(apitypes.CodeCrossCheckCoverage{
			Version: 1, Entries: []apitypes.CodeCrossCheckCoverageEntry{},
			InventoryIncomplete: true, Reasons: []string{reason},
		})
		return raw
	}
	if len(input) != 1 {
		return fallback("coverage_invalid")
	}
	raw := input[0]
	if len(raw) > codeCoverageBytes {
		return fallback("coverage_too_large")
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	if !utf8.Valid(raw) {
		return fallback("coverage_invalid")
	}
	var account apitypes.CodeCrossCheckCoverage
	fields, ok := coverageObject(raw, []string{"version", "entries", "inventory_incomplete", "omitted_paths", "omitted_count_exact", "unknown_judgments", "invalid_judgments", "omitted_judgments", "activity_complete", "reasons"})
	if !ok {
		return fallback("coverage_invalid")
	}
	entryJSON := fields["entries"]
	fields["entries"] = json.RawMessage("[]")
	metadata, err := json.Marshal(fields)
	if err != nil || json.Unmarshal(metadata, &account) != nil || account.Version != 1 || account.Reasons == nil {
		return fallback("coverage_invalid")
	}
	for _, n := range []int64{account.OmittedPaths, account.UnknownJudgments, account.InvalidJudgments, account.OmittedJudgments} {
		if n < 0 || n > codeCoverageSafeInteger {
			return fallback("coverage_invalid")
		}
	}
	allowedReasons := map[string]bool{
		"inventory_truncated": true, "inventory_failed": true, "invalid_judgments": true,
		"unknown_judgments": true, "judgments_truncated": true, "activity_incomplete": true,
		"coverage_invalid": true, "coverage_too_large": true,
	}
	if len(account.Reasons) > 8 {
		return fallback("coverage_invalid")
	}
	seenReasons := map[string]bool{}
	reasons := []string{}
	for _, reason := range account.Reasons {
		if !allowedReasons[reason] {
			return fallback("coverage_invalid")
		}
		if !seenReasons[reason] {
			reasons = append(reasons, reason)
			seenReasons[reason] = true
		}
	}
	account.Reasons = reasons
	var entries []json.RawMessage
	if json.Unmarshal(entryJSON, &entries) != nil {
		return fallback("coverage_invalid")
	}
	kept := []apitypes.CodeCrossCheckCoverageEntry{}
	seen := map[string]bool{}
	dropped := int64(0)
	invalid := false
	for _, rawEntry := range entries {
		var entry apitypes.CodeCrossCheckCoverageEntry
		_, valid := coverageObject(rawEntry, []string{"path", "status", "reason", "recorded_read"})
		decodeErr := json.Unmarshal(rawEntry, &entry)
		// Even a malformed judgment can duplicate a later, valid path.
		// Ambiguous accounts retain no examined claims.
		if entry.Path != "" {
			if seen[entry.Path] {
				return fallback("coverage_invalid")
			}
			seen[entry.Path] = true
		}
		if !valid || decodeErr != nil || entry.Path == "" || hasLoneSurrogateEscape(rawEntry) ||
			len(entry.Path) > codeCoveragePathBytes || len(entry.Reason) > codeCoverageReasonBytes ||
			!utf8.ValidString(entry.Path) || !utf8.ValidString(entry.Reason) ||
			strings.ContainsFunc(entry.Path, func(r rune) bool {
				return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
			}) || strings.ContainsRune(entry.Reason, 0) ||
			(entry.Status != "examined" && entry.Status != "excluded" && entry.Status != "unassessed") ||
			(entry.Status != "unassessed" && entry.Reason == "") ||
			(entry.RecordedRead && entry.Status != "examined") {
			dropped++
			invalid = true
			continue
		}
		if len(kept) == codeCoverageEntries {
			dropped++
			continue
		}
		kept = append(kept, entry)
	}
	account.Entries = kept
	if dropped > 0 {
		if account.OmittedPaths > codeCoverageSafeInteger-dropped {
			account.OmittedPaths = codeCoverageSafeInteger
			account.OmittedCountExact = false
		} else {
			account.OmittedPaths += dropped
		}
		account.InventoryIncomplete = true
		reason := "inventory_truncated"
		if invalid {
			reason = "coverage_invalid"
			account.OmittedCountExact = false
		}
		if !seenReasons[reason] {
			account.Reasons = append(account.Reasons, reason)
		}
	}
	if account.OmittedPaths > 0 || !account.OmittedCountExact || account.UnknownJudgments > 0 ||
		account.InvalidJudgments > 0 || account.OmittedJudgments > 0 || !account.ActivityComplete || len(account.Reasons) > 0 {
		account.InventoryIncomplete = true
	}
	normalized, err := json.Marshal(account)
	if err != nil || len(normalized) > codeCoverageBytes {
		return fallback("coverage_too_large")
	}
	return normalized
}

// coverageObject requires every exact key once and rejects null values. Its
// caller bounds the bytes, including all nested values, before decoding.
func coverageObject(raw []byte, keys []string) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	fields := make(map[string]json.RawMessage, len(keys))
	for dec.More() {
		tok, err = dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok || !allowed[key] {
			return nil, false
		}
		if _, exists := fields[key]; exists {
			return nil, false
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, false
		}
		fields[key] = value
	}
	if tok, err = dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, false
	}
	return fields, len(fields) == len(keys)
}

// hasLoneSurrogateEscape reports a \uXXXX surrogate escape that is not a
// high/low pair. encoding/json silently decodes one to U+FFFD, which would
// record a path the worker never named.
func hasLoneSurrogateEscape(raw []byte) bool {
	hex := func(i int) (rune, bool) {
		if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
			return 0, false
		}
		var r rune
		for _, c := range raw[i+2 : i+6] {
			switch {
			case '0' <= c && c <= '9':
				r = r<<4 | rune(c-'0')
			case 'a' <= c && c <= 'f':
				r = r<<4 | rune(c-'a'+10)
			case 'A' <= c && c <= 'F':
				r = r<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return r, true
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		r, ok := hex(i)
		if !ok {
			i++ // skip the escaped byte, so \\ never starts a new escape
			continue
		}
		switch {
		case 0xDC00 <= r && r <= 0xDFFF:
			return true
		case 0xD800 <= r && r <= 0xDBFF:
			low, ok := hex(i + 6)
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			i += 11
		default:
			i += 5
		}
	}
	return false
}
