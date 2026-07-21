package workersvc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// Message sanitation (PRD #108 M2). A worker is untrusted input on the message
// route — the same reasoning that produced sanitizeSelfReported in
// handler/worker_protocol.go — and three byte patterns it can legitimately emit
// are ones Postgres refuses to store. Each one wedges a run: AppendMessages
// returns the store error, the handler's default: arm answers 500, and the
// worker's batcher treats any throw as retryable and re-posts the identical batch
// forever. Measured on postgres:17, against the unfixed tree:
//
//	payload (jsonb), the six-byte u0000 escape .. SQLSTATE 22P05
//	payload (jsonb), an unpaired surrogate escape .. SQLSTATE 22P02
//	payload (jsonb), a raw invalid UTF-8 byte ...... SQLSTATE 22021
//	agent / agent_instance / agent_label, a NUL .... SQLSTATE 22021
//
// The two sinks need DIFFERENT treatment, and the asymmetry is the whole reason
// this file has two functions rather than one. Payload is json.RawMessage, so the
// decoder never touches its bytes and all three patterns arrive verbatim.
// Agent/AgentInstance/AgentLabel are decoded into Go strings, and encoding/json's
// unquoteBytes has ALREADY replaced both unpaired surrogates and invalid UTF-8
// with U+FFFD by the time the value exists — but it turns a u0000 escape into a
// real 0x00 byte. Measured, not assumed: the text columns need NUL stripping and
// nothing else.
//
// What is deliberately NOT stripped: \n, \t and ANSI escapes. They are legal in
// jsonb and load-bearing in tool output — stripping \n would mangle every log the
// UI renders. PRD #90's sanitizeMemoryField strips that wider class because its
// sink is a terminal table printer; this payload lands in a React renderer that
// escapes it. Different sink, different rule; do not harmonize them.

// stripCounts is what was removed from one message, per class. It is reported
// rather than discarded because silently laundering bytes means a future
// NUL-emitting tool is never investigated (PRD #108 Risk 3) — the incident's
// trigger was a headless Chromium nobody knew was writing NULs.
type stripCounts struct {
	payloadNUL       int
	payloadSurrogate int
	payloadBadUTF8   int
	textNUL          int
}

func (c stripCounts) any() bool {
	return c.payloadNUL+c.payloadSurrogate+c.payloadBadUTF8+c.textNUL > 0
}

// replacementEscape is U+FFFD written as a JSON escape rather than as its three
// raw bytes. Emitting the escape keeps the output ASCII wherever the input was an
// escape, which makes "the result is still valid JSON" true by construction
// instead of by argument.
var replacementEscape = []byte(`\` + `ufffd`)

// sanitizePayloadJSON rewrites the three unstorable patterns inside a payload's
// JSON string literals, leaving every other byte alone.
//
// It is a state scanner rather than a decode/walk/re-encode, and NOT because
// "byte fidelity is preserved end to end" — it is not: payload is jsonb, which
// normalizes key order, whitespace and duplicate keys, so the REST replay path
// already returns different bytes than the worker sent. The real reason is that a
// Go round-trip is lossy in a way jsonb is not. encoding/json decodes every number
// through float64, so 12345678901234567890 re-encodes as 12345678901234567000 —
// a corruption NEITHER path has today, introduced by the fix. Avoiding it needs
// Decoder.UseNumber plus Encoder.SetEscapeHTML(false) and still reorders keys:
// more code, worse result.
//
// It also disarms the trap that makes a naive strings.ReplaceAll wrong. The NUL
// arrives as the six-byte escape inside otherwise-valid JSON (a raw 0x00 is
// already invalid JSON and 400s before it gets here), and the wire form \\u0000 —
// an escaped backslash followed by the literal text u0000 — is perfectly legal
// data that a byte-substring replace corrupts. Consuming both bytes of every
// escape as a unit makes that confusion impossible rather than merely unlikely.
//
// The input MUST already be json.Valid; the caller checks that first, and the
// 400-on-invalid-JSON arm is unchanged.
func sanitizePayloadJSON(p []byte) ([]byte, stripCounts) {
	var c stripCounts
	// Fast path for nearly every real message: no escape can hide without a
	// backslash, and no invalid byte can hide in valid UTF-8. Returns the same
	// backing array, so a healthy batch copies nothing.
	if bytes.IndexByte(p, '\\') < 0 && utf8.Valid(p) {
		return p, c
	}
	out := make([]byte, 0, len(p))
	inString := false
	for i := 0; i < len(p); {
		b := p[i]
		if !inString {
			// Outside a string literal JSON is structural ASCII and digits; a
			// backslash cannot legally appear, so nothing here needs rewriting.
			if b == '"' {
				inString = true
			}
			out = append(out, b)
			i++
			continue
		}
		switch {
		case b == '"':
			inString = false
			out = append(out, b)
			i++

		case b == '\\':
			// Consume the escape as a UNIT. This single line is the whole \\u0000
			// trap: the escaped backslash is eaten here, so the u0000 that follows it
			// is read as ordinary text and can never be mistaken for the NUL escape.
			if i+1 >= len(p) {
				out = append(out, b) // unreachable for valid JSON; copy rather than guess
				i++
				continue
			}
			if p[i+1] != 'u' || i+6 > len(p) {
				out = append(out, p[i], p[i+1])
				i += 2
				continue
			}
			r, ok := hex4(p[i+2 : i+6])
			if !ok {
				out = append(out, p[i:i+6]...)
				i += 6
				continue
			}
			switch {
			case r == 0:
				c.payloadNUL++
				i += 6 // dropped entirely: there is no character to stand in for it
			case r >= 0xD800 && r <= 0xDBFF:
				// A HIGH surrogate is only unpaired if no low half follows. Peeking is
				// what keeps every emoji intact — a strip that flattened all \udXXX
				// escapes would pass an unpaired-surrogate test while destroying every
				// astral character in every tool result.
				if i+12 <= len(p) && p[i+6] == '\\' && p[i+7] == 'u' {
					if lo, okLo := hex4(p[i+8 : i+12]); okLo && lo >= 0xDC00 && lo <= 0xDFFF {
						out = append(out, p[i:i+12]...)
						i += 12
						continue
					}
				}
				c.payloadSurrogate++
				out = append(out, replacementEscape...)
				i += 6
			case r >= 0xDC00 && r <= 0xDFFF:
				// A LOW surrogate reached on its own is unpaired by construction: a
				// paired one was consumed with its high half above.
				c.payloadSurrogate++
				out = append(out, replacementEscape...)
				i += 6
			default:
				out = append(out, p[i:i+6]...)
				i += 6
			}

		case b < utf8.RuneSelf:
			out = append(out, b)
			i++

		default:
			// json.Valid does not validate UTF-8 (Go's scanner does not look), so an
			// invalid byte rides all the way here and Postgres is the first thing to
			// object. Replace exactly the one bad byte and keep scanning.
			r, size := utf8.DecodeRune(p[i:])
			if r == utf8.RuneError && size == 1 {
				c.payloadBadUTF8++
				out = append(out, replacementEscape...)
				i++
				continue
			}
			out = append(out, p[i:i+size]...)
			i += size
		}
	}
	return out, c
}

// hex4 parses the four hex digits of a \uXXXX escape. ok is false for anything
// else, which valid JSON cannot contain — the caller then copies the bytes
// through rather than inventing a rune.
func hex4(b []byte) (rune, bool) {
	var v rune
	for _, d := range b {
		v <<= 4
		switch {
		case d >= '0' && d <= '9':
			v |= rune(d - '0')
		case d >= 'a' && d <= 'f':
			v |= rune(d-'a') + 10
		case d >= 'A' && d <= 'F':
			v |= rune(d-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

// unstorableSQLSTATEs are the Postgres error codes that mean "this value can
// never be stored", as opposed to "try again". ENUMERATED, deliberately, and not
// matched as a 22* range: 22001 (string too long) and 22003 (numeric out of
// range) live in the same class and are neither this bug nor necessarily
// permanent for a re-shaped batch. Each was MEASURED on postgres:17 against the
// real insert, not read off a table:
//
//	22P05  unsupported Unicode escape sequence  — the six-byte u0000 escape in jsonb
//	22P02  invalid input syntax for type json   — an unpaired surrogate escape in jsonb
//	22021  invalid byte sequence for encoding   — raw invalid UTF-8, in jsonb OR in a text column
//
// EVERYTHING ELSE STAYS ON THE 500 PATH, and that is not a leftover — it is the
// mirror-image bug and it is worse. Misclassifying a transient failure as
// permanent fails healthy runs: connection loss (08*), pool exhaustion and
// admin-limit rejections (53*), serialization failures and deadlocks (40001,
// 40P01), lock-not-available (55P03), statement cancellation and admin shutdown
// (57*), and any non-PgError at all — notably context.Canceled from a
// disconnecting worker — must all be retried. So must 23503 (the run was deleted
// mid-batch): permanent, but not a payload fault, and out of Phase 1's scope.
var unstorableSQLSTATEs = map[string]bool{
	"22P05": true,
	"22P02": true,
	"22021": true,
}

// classifyStoreError tags a store error as permanently unstorable when its
// SQLSTATE says so, and passes everything else through untouched. The original
// error is wrapped alongside the sentinel, so a caller can still reach the
// *pgconn.PgError for logging.
func classifyStoreError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && unstorableSQLSTATEs[pgErr.Code] {
		return fmt.Errorf("%w: %w", ErrUnstorableMessage, err)
	}
	return err
}

// stripNUL removes 0x00 from one of the worker-controlled TEXT columns. NUL only:
// by the time these strings exist, encoding/json has already replaced every
// unpaired surrogate and every invalid UTF-8 byte with U+FFFD, so there is
// nothing else for this to find through the decoder it is fed by.
func stripNUL(s string) (string, int) {
	n := strings.Count(s, "\x00")
	if n == 0 {
		return s, 0
	}
	return strings.ReplaceAll(s, "\x00", ""), n
}
