package workersvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// The escapes are built by concatenation so the six-character sequence is never a
// literal in this source. That is not stylistic: a literal one that gets folded
// into a real 0x00 byte turns every fixture here into invalid JSON, which this
// function is documented never to receive — the suite would then exercise a code
// path it does not own and pass without testing the trap it exists for.
var (
	escNUL     = `\` + `u0000`
	escHiSurr  = `\` + `ud800`
	escLoSurr  = `\` + `udfff`
	escPairHi  = `\` + `ud83d`
	escPairLo  = `\` + `ude00`
	escLiteral = `\` + `\` + `u0000` // an escaped BACKSLASH then u0000: legal data
)

func TestSanitizePayloadJSON(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  string
		wantC stripCounts
	}{
		{
			name: "clean payload is returned unchanged",
			in:   `{"t":"hello","n":1}`,
			want: `{"t":"hello","n":1}`,
		},
		{
			name:  "the incident: a u0000 escape is dropped",
			in:    `{"t":"a` + escNUL + `b"}`,
			want:  `{"t":"ab"}`,
			wantC: stripCounts{payloadNUL: 1},
		},
		{
			name:  "an unpaired HIGH surrogate becomes U+FFFD",
			in:    `{"t":"a` + escHiSurr + `b"}`,
			want:  `{"t":"a` + `\` + `ufffdb"}`,
			wantC: stripCounts{payloadSurrogate: 1},
		},
		{
			name:  "an unpaired LOW surrogate becomes U+FFFD",
			in:    `{"t":"a` + escLoSurr + `b"}`,
			want:  `{"t":"a` + `\` + `ufffdb"}`,
			wantC: stripCounts{payloadSurrogate: 1},
		},
		{
			// Without this, a strip that flattened every \udXXX escape would pass the
			// two cases above while destroying every astral character in every tool
			// result — the exact "fix that is worse than the bug" shape.
			name: "a well-formed surrogate PAIR is untouched",
			in:   `{"t":"a` + escPairHi + escPairLo + `b"}`,
			want: `{"t":"a` + escPairHi + escPairLo + `b"}`,
		},
		{
			name:  "a HIGH surrogate followed by a NON-low escape is unpaired",
			in:    `{"t":"a` + escHiSurr + `\` + `u0041b"}`,
			want:  `{"t":"a` + `\` + `ufffd` + `\` + `u0041b"}`,
			wantC: stripCounts{payloadSurrogate: 1},
		},
		{
			// THE trap. A byte-substring replace of the six-character escape corrupts
			// this into something that decodes differently.
			name: "an escaped backslash followed by u0000 is legal data and survives",
			in:   `{"t":"a` + escLiteral + `b"}`,
			want: `{"t":"a` + escLiteral + `b"}`,
		},
		{
			name:  "the escape is stripped from KEYS as well as values",
			in:    `{"k` + escNUL + `":"v"}`,
			want:  `{"k":"v"}`,
			wantC: stripCounts{payloadNUL: 1},
		},
		{
			name: "other escapes are copied through",
			in:   `{"t":"a\nb\tc\"d\\e\/f"}`,
			want: `{"t":"a\nb\tc\"d\\e\/f"}`,
		},
		{
			name:  "several classes in one payload are each counted",
			in:    `{"t":"` + escNUL + escNUL + escHiSurr + `","u":"` + escLoSurr + `"}`,
			want:  `{"t":"` + `\` + `ufffd","u":"` + `\` + `ufffd"}`,
			wantC: stripCounts{payloadNUL: 2, payloadSurrogate: 2},
		},
		{
			name: "a lone backslash-u that is not four hex digits is copied through",
			in:   `{"t":"a\uZZZZb"}`,
			want: `{"t":"a\uZZZZb"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, c := sanitizePayloadJSON([]byte(tc.in))
			if string(got) != tc.want {
				t.Errorf("output = %s\n    want %s", got, tc.want)
			}
			if c != tc.wantC {
				t.Errorf("counts = %+v, want %+v", c, tc.wantC)
			}
		})
	}
}

// Raw invalid UTF-8 gets its own test because it cannot be written as a Go string
// literal without becoming the replacement character on the spot — which would
// silently turn this into a test of clean input.
func TestSanitizePayloadJSONRawInvalidUTF8(t *testing.T) {
	in := []byte(`{"t":"aXb"}`)
	in[7] = 0xff
	if utf8.Valid(in) {
		t.Fatal("the fixture is valid UTF-8, so it carries no invalid sequence to strip")
	}
	if !json.Valid(in) {
		t.Fatal("the fixture is not valid JSON — sanitizePayloadJSON is only ever called on valid JSON, " +
			"so this would be testing a contract the caller guarantees cannot happen")
	}
	got, c := sanitizePayloadJSON(in)
	if want := `{"t":"a` + `\` + `ufffdb"}`; string(got) != want {
		t.Errorf("output = %s, want %s", got, want)
	}
	if want := (stripCounts{payloadBadUTF8: 1}); c != want {
		t.Errorf("counts = %+v, want %+v", c, want)
	}
}

// Multi-byte UTF-8 that is VALID must survive byte for byte. Without this, an
// off-by-one in the rune walk would mangle every non-ASCII tool result and only
// the invalid-byte test above would notice — and it would notice by passing.
func TestSanitizePayloadJSONValidMultibyteSurvives(t *testing.T) {
	in := `{"t":"héllo — 日本語 😀","u":"ok"}`
	got, c := sanitizePayloadJSON([]byte(in))
	if string(got) != in {
		t.Errorf("output = %s\n    want %s", got, in)
	}
	if c.any() {
		t.Errorf("counts = %+v, want all zero", c)
	}
}

// The fast path must return the SAME backing array, not a copy: nearly every real
// message takes it, and this is the difference between allocating per message and
// not. A test that only checked equality would pass against a version that copies.
func TestSanitizePayloadJSONFastPathDoesNotCopy(t *testing.T) {
	in := []byte(`{"t":"plain ascii","n":12345678901234567890}`)
	got, c := sanitizePayloadJSON(in)
	if c.any() {
		t.Fatalf("counts = %+v, want all zero for a clean payload", c)
	}
	if &got[0] != &in[0] {
		t.Error("the clean-payload fast path allocated a copy; it must return the input's own backing array")
	}
}

func TestStripNUL(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		wantN int
	}{
		{"clean", "clean", 0},
		{"a\x00b", "ab", 1},
		{"\x00\x00", "", 2},
		{"", "", 0},
		{"héllo", "héllo", 0},
	}
	for _, tc := range cases {
		got, n := stripNUL(tc.in)
		if got != tc.want || n != tc.wantN {
			t.Errorf("stripNUL(%q) = (%q, %d), want (%q, %d)", tc.in, got, n, tc.want, tc.wantN)
		}
	}
}

func TestClassifyStoreError(t *testing.T) {
	// The three permanent codes, each measured against the real insert before being
	// put in the map — see the sanitize.go comment.
	for _, code := range []string{"22P05", "22P02", "22021"} {
		err := classifyStoreError(&pgconn.PgError{Code: code, Message: "boom"})
		if !errors.Is(err, ErrUnstorableMessage) {
			t.Errorf("SQLSTATE %s: not classified as unstorable (%v) — the worker would retry it forever", code, err)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Errorf("SQLSTATE %s: the original PgError is no longer reachable through the wrap", code)
		}
	}
	// The mirror-image bug, and the worse one: a transient failure classified as
	// permanent fails healthy runs. Every one of these MUST stay retryable.
	for _, code := range []string{
		"08000", "08001", "08003", "08004", "08006", // connection
		"53100", "53200", "53300", // disk/memory/connection-slot exhaustion
		"40001", "40P01", "55P03", // serialization, deadlock, lock not available
		"57014", "57P01", "57P02", "57P03", // cancelled, admin shutdown, crash, cannot connect now
		"23503",          // FK violation: the run was deleted mid-batch — permanent, but not a payload fault
		"22001", "22003", // adjacent 22* codes that a range match would have swept up
	} {
		err := classifyStoreError(&pgconn.PgError{Code: code, Message: "boom"})
		if errors.Is(err, ErrUnstorableMessage) {
			t.Errorf("SQLSTATE %s classified as PERMANENT; it must stay on the 500 path or a transient "+
				"failure permanently fails a healthy run", code)
		}
	}
	// A non-Postgres error — context.Canceled from a disconnecting worker is the
	// realistic one — is never permanent.
	plain := errors.New("context canceled")
	if got := classifyStoreError(plain); !errors.Is(got, plain) || errors.Is(got, ErrUnstorableMessage) {
		t.Errorf("a non-PgError was reclassified: %v", got)
	}
	if classifyStoreError(nil) != nil {
		t.Error("classifyStoreError(nil) must be nil")
	}
}

// findNULInJSON walks a decoded JSON value for a string (key or value) still
// holding a 0x00. Object KEYS are checked too: a NUL in a key rejects the insert
// exactly as one in a value does.
func findNULInJSON(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		if strings.ContainsRune(t, 0) {
			return t, true
		}
	case map[string]any:
		for k, val := range t {
			if strings.ContainsRune(k, 0) {
				return k, true
			}
			if s, found := findNULInJSON(val); found {
				return s, true
			}
		}
	case []any:
		for _, val := range t {
			if s, found := findNULInJSON(val); found {
				return s, true
			}
		}
	}
	return "", false
}

// FuzzSanitizePayloadJSON pins the invariants the table above can only sample.
// The trap this exists for is that no hand-written case list can cover the
// interaction between escape parsing and surrogate pairing.
func FuzzSanitizePayloadJSON(f *testing.F) {
	for _, s := range []string{
		`{"t":"a` + escNUL + `b"}`,
		`{"t":"a` + escHiSurr + `b"}`,
		`{"t":"a` + escPairHi + escPairLo + `b"}`,
		`{"t":"a` + escLiteral + `b"}`,
		`{"t":"héllo 😀","n":1.5,"a":[1,2,{"b":null}]}`,
		`"just a string"`,
		`[]`,
		`123`,
		`1E1000`, // legal JSON, overflows float64 — kept as a seed, see the decode below
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		// The function's contract is valid JSON in; AppendMessages checks that first.
		if !json.Valid([]byte(in)) {
			t.Skip()
		}
		out, c := sanitizePayloadJSON([]byte(in))

		// 1. Still valid JSON. Without this the fix trades a 22P05 for a 22P02.
		if !json.Valid(out) {
			t.Fatalf("sanitizing valid JSON produced INVALID JSON\n  in:  %q\n  out: %q", in, out)
		}
		// 2. Nothing unstorable survives — the whole point.
		if !utf8.Valid(out) {
			t.Fatalf("output is not valid UTF-8 (Postgres answers 22021)\n  in: %q\n  out: %q", in, out)
		}
		// Decoded with UseNumber: a plain `any` decode routes every number through
		// float64, so a legal-but-huge literal like 1E1000 fails to decode and the
		// invariant would report a sanitizer bug that is really a float64 overflow.
		// (Found by this fuzzer on its first run, against an earlier version of this
		// check — the failure was in the instrument, not the code under test.)
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		var decoded any
		if err := dec.Decode(&decoded); err != nil {
			t.Fatalf("output does not decode: %v\n  out: %q", err, out)
		}
		// The escape survived only if a DECODED string still holds a 0x00 — which is
		// exactly what Postgres answers 22P05 to. Walking the decoded value rather
		// than the raw bytes is what makes the legal literal text of the escape (the
		// \\u0000 trap) correctly count as clean.
		if s, found := findNULInJSON(decoded); found {
			t.Fatalf("a NUL survived into a decoded string (Postgres answers 22P05)\n  in: %q\n  out: %q\n  string: %q", in, out, s)
		}
		// 3. Clean input round-trips byte-identical. This is what keeps the scanner
		//    honest: it may only touch what it reports touching.
		if !c.any() && string(out) != in {
			t.Fatalf("input was reported clean but the output differs\n  in:  %q\n  out: %q", in, out)
		}
		// 4. Idempotent. A second pass must find nothing left to do; if it does, the
		//    first pass emitted something it would itself reject.
		out2, c2 := sanitizePayloadJSON(out)
		if c2.any() || string(out2) != string(out) {
			t.Fatalf("not idempotent: a second pass changed the output (counts %+v)\n  out:  %q\n  out2: %q", c2, out, out2)
		}
	})
}
