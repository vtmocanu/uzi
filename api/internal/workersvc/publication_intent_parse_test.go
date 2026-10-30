package workersvc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	parseRunID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	parseRepoID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	parseHead   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// intentDoc builds an intent whose floor, branch and extra members the case controls.
func intentDoc(branch, floor, extra string) string {
	return `{"run_id":"` + parseRunID + `","repo_id":"` + parseRepoID + `","branch":"` + branch +
		`","claim_generation":1,"original_head":"` + parseHead + `","floor":` + floor + `,"candidate":null` + extra + `}`
}

const absentFloor = `{"kind":"absent"}`

func TestParsePublicationIntentTable(t *testing.T) {
	nest := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	// Depth counts the intent object itself (1) and the "mr" member's brackets.
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"baseline", intentDoc("agent/issue-1", absentFloor, ""), true},
		{"pinned floor", intentDoc("agent/issue-1", `{"kind":"pinned","oid":"`+parseHead+`"}`, ""), true},
		{"allowlisted opaque members", intentDoc("agent/issue-1", absentFloor, `,"v":1,"mr":{"t":"x"},"guard":{},"mac":"m"`), true},
		{"escaped backslash before u is not an escape", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"\\u0000"}`), true},
		{"valid surrogate pair", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"😀"}`), true},
		{"valid non-ASCII UTF-8", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"café é"}`), true},
		{"uppercase hex in escape", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"é"}`), true},
		{"depth 32", intentDoc("agent/issue-1", absentFloor, `,"mr":`+nest(31)), true},
		{"depth 33", intentDoc("agent/issue-1", absentFloor, `,"mr":`+nest(32)), false},
		{"64-digit number", intentDoc("agent/issue-1", absentFloor, `,"mr":{"n":`+strings.Repeat("9", 64)+`}`), true},
		{"65-digit number", intentDoc("agent/issue-1", absentFloor, `,"mr":{"n":`+strings.Repeat("9", 65)+`}`), false},

		{"unicode NUL", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"a\u0000b"}`), false},
		{"unicode NUL in branch", intentDoc(`agent/\u0000x`, absentFloor, ""), false},
		{"lone high surrogate at end of string", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"a\ud800"}`), false},
		{"lone high surrogate before text", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"\ud800b"}`), false},
		{"high surrogate then non-low escape", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"\ud800A"}`), false},
		{"high surrogate then high surrogate", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"\ud800\ud800"}`), false},
		{"lone low surrogate", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"\udc00"}`), false},
		{"low surrogate before high", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"\udc00\ud800"}`), false},
		{"truncated escape", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"\u12"}`), false},
		{"invalid UTF-8 byte", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"a`+"\xff"+`b"}`), false},
		{"overlong NUL encoding", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"a`+"\xc0\x80"+`b"}`), false},
		{"overlong slash encoding", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"a`+"\xc0\xaf"+`b"}`), false},
		{"encoded surrogate in UTF-8", intentDoc("agent/issue-1", absentFloor, `,"mr":{"t":"a`+"\xed\xa0\x80"+`b"}`), false},

		{"uppercase run id", strings.Replace(intentDoc("agent/issue-1", absentFloor, ""), parseRunID, strings.ToUpper(parseRunID), 1), false},
		{"uppercase repo id", strings.Replace(intentDoc("agent/issue-1", absentFloor, ""), parseRepoID, strings.ToUpper(parseRepoID), 1), false},
		{"braced run id", strings.Replace(intentDoc("agent/issue-1", absentFloor, ""), parseRunID, "{"+parseRunID+"}", 1), false},
		{"unhyphenated run id", strings.Replace(intentDoc("agent/issue-1", absentFloor, ""), parseRunID, strings.ReplaceAll(parseRunID, "-", ""), 1), false},
		{"nil run id", strings.Replace(intentDoc("agent/issue-1", absentFloor, ""), parseRunID, "00000000-0000-0000-0000-000000000000", 1), false},

		{"absent floor with oid null", intentDoc("agent/issue-1", `{"kind":"absent","oid":null}`, ""), false},
		{"absent floor with oid", intentDoc("agent/issue-1", `{"kind":"absent","oid":"`+parseHead+`"}`, ""), false},
		{"pinned floor with oid null", intentDoc("agent/issue-1", `{"kind":"pinned","oid":null}`, ""), false},
		{"pinned floor without oid", intentDoc("agent/issue-1", `{"kind":"pinned"}`, ""), false},
		{"pinned floor with short oid", intentDoc("agent/issue-1", `{"kind":"pinned","oid":"abc"}`, ""), false},
		{"pinned floor with uppercase oid", intentDoc("agent/issue-1", `{"kind":"pinned","oid":"`+strings.ToUpper(parseHead)+`"}`, ""), false},
		{"unknown floor kind", intentDoc("agent/issue-1", `{"kind":"other"}`, ""), false},
		{"floor key case variant", intentDoc("agent/issue-1", `{"Kind":"absent"}`, ""), false},
		{"floor duplicate key", intentDoc("agent/issue-1", `{"kind":"absent","kind":"absent"}`, ""), false},
		{"floor unknown member", intentDoc("agent/issue-1", `{"kind":"absent","x":1}`, ""), false},

		{"duplicate top-level key", intentDoc("agent/issue-1", absentFloor, `,"branch":"agent/issue-1"`), false},
		{"case-variant top-level key", intentDoc("agent/issue-1", absentFloor, `,"Branch":"x"`), false},
		{"unknown top-level key", intentDoc("agent/issue-1", absentFloor, `,"extra":1`), false},
		{"trailing data", intentDoc("agent/issue-1", absentFloor, "") + ` {}`, false},
		{"not an object", `[]`, false},
		{"empty", ``, false},
		{"missing required member", strings.Replace(intentDoc("agent/issue-1", absentFloor, ""), `"candidate":null`, `"x":null`, 1), false},

		{"empty branch", intentDoc("", absentFloor, ""), false},
		{"ANSI escape in branch", intentDoc(`agent/\u001b[31m`, absentFloor, ""), false},
		{"control character in branch", intentDoc(`agent/\u0007`, absentFloor, ""), false},
		{"newline in branch", intentDoc(`agent/\n`, absentFloor, ""), false},
		{"bidi override in branch", intentDoc(`agent/‮`, absentFloor, ""), false},
		{"bidi isolate in branch", intentDoc(`agent/⁦`, absentFloor, ""), false},
		{"zero-width space in branch", intentDoc(`agent/​`, absentFloor, ""), false},
		{"line separator in branch", intentDoc(`agent/ `, absentFloor, ""), false},
		{"over-long branch", intentDoc(strings.Repeat("a", 256), absentFloor, ""), false},
		{"255-byte branch", intentDoc(strings.Repeat("a", 255), absentFloor, ""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePublicationIntent(json.RawMessage(tc.raw))
			if tc.ok && err != nil {
				t.Fatalf("rejected a valid intent: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidState) {
				t.Fatalf("err=%v, want ErrInvalidState", err)
			}
		})
	}
}

func TestParsePublicationIntentFieldsAndSize(t *testing.T) {
	b, err := parsePublicationIntent(json.RawMessage(intentDoc("agent/issue-1", `{"kind":"pinned","oid":"`+parseHead+`"}`, "")))
	if err != nil {
		t.Fatal(err)
	}
	if b.RunID.String() != parseRunID || b.RepoID.String() != parseRepoID || b.Branch != "agent/issue-1" ||
		b.ClaimGeneration != 1 || b.OriginalHead != parseHead || b.Floor.Kind != "pinned" || b.Floor.Oid == nil || *b.Floor.Oid != parseHead || b.Candidate != nil {
		t.Fatalf("parsed=%+v", b)
	}
	over := intentDoc("agent/issue-1", absentFloor, `,"mr":{"pad":"`+strings.Repeat("x", MaxPublicationIntentBytes)+`"}`)
	if _, err := parsePublicationIntent(json.RawMessage(over)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("oversize err=%v", err)
	}
}

// The escape scan is exercised directly too, so a regression in it cannot hide behind the other
// document checks that also refuse some of these bodies.
func TestCheckIntentEscapes(t *testing.T) {
	for name, tc := range map[string]struct {
		raw string
		ok  bool
	}{
		"no escapes":               {`{"a":"b"}`, true},
		"escaped backslash then u": {`{"a":"\\u0000"}`, true},
		"escaped quote":            {`{"a":"\""}`, true},
		"pair":                     {`{"a":"😀"}`, true},
		"NUL":                      {`{"a":"\u0000"}`, false},
		"high at end":              {`{"a":"\ud800"}`, false},
		"high then plain text":     {`{"a":"\ud800x"}`, false},
		"high then non-low":        {`{"a":"\ud800A"}`, false},
		"lone low":                 {`{"a":"\udfff"}`, false},
		"bad hex":                  {`{"a":"\u00zz"}`, false},
		"dangling backslash":       {`\`, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkIntentEscapes([]byte(tc.raw))
			if tc.ok && err != nil {
				t.Fatalf("err=%v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidState) {
				t.Fatalf("err=%v, want ErrInvalidState", err)
			}
		})
	}
}

func TestCanonicalUUID(t *testing.T) {
	for name, tc := range map[string]struct {
		raw string
		ok  bool
	}{
		"lowercase":   {`"` + parseRunID + `"`, true},
		"uppercase":   {`"` + strings.ToUpper(parseRunID) + `"`, false},
		"mixed case":  {`"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaA"`, false},
		"braced":      {`"{` + parseRunID + `}"`, false},
		"urn":         {`"urn:uuid:` + parseRunID + `"`, false},
		"no hyphens":  {`"` + strings.ReplaceAll(parseRunID, "-", "") + `"`, false},
		"nil uuid":    {`"00000000-0000-0000-0000-000000000000"`, false},
		"not string":  {`1`, false},
		"null":        {`null`, false},
		"empty":       {`""`, false},
		"trailing ws": {`"` + parseRunID + ` "`, false},
	} {
		t.Run(name, func(t *testing.T) {
			id, ok := canonicalUUID(json.RawMessage(tc.raw))
			if ok != tc.ok || (ok && id.String() != parseRunID) {
				t.Fatalf("canonicalUUID(%s) = (%v,%v), want ok=%v", tc.raw, id, ok, tc.ok)
			}
		})
	}
}
