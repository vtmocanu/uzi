package codexauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
)

// The closed set of OAuth error codes AuthError.OAuthCode may carry. The first four
// are refresh-material rejections (see RefreshMaterialRejected); the next five are
// RFC 6749 section 5.2 codes that are recognised but do not by themselves prove the
// refresh token is dead. OAuthCodeUnknown stands for any code outside this set, a
// malformed body, or a body too large to parse.
const (
	OAuthCodeRefreshTokenExpired     = "refresh_token_expired"
	OAuthCodeRefreshTokenReused      = "refresh_token_reused"
	OAuthCodeRefreshTokenInvalidated = "refresh_token_invalidated"
	OAuthCodeInvalidGrant            = "invalid_grant"

	OAuthCodeInvalidRequest       = "invalid_request"
	OAuthCodeInvalidClient        = "invalid_client"
	OAuthCodeUnauthorizedClient   = "unauthorized_client"
	OAuthCodeUnsupportedGrantType = "unsupported_grant_type"
	OAuthCodeInvalidScope         = "invalid_scope"

	OAuthCodeUnknown = "unknown"
)

// maxOAuthErrorBodyBytes bounds how much of a non-2xx refresh body is parsed for an
// OAuth error code. A real OAuth error object is a few hundred bytes; a body longer
// than this is not parsed at all (a truncated JSON prefix is not trusted).
const maxOAuthErrorBodyBytes = 4 << 10

// readOAuthErrorCode reads a non-2xx refresh body, bounded, and returns its
// normalised closed-set OAuth error code. It reads at most maxOAuthErrorBodyBytes+1
// bytes to detect an oversized body, then drains the remainder up to maxBodyBytes
// in total so the connection can be reused.
func readOAuthErrorCode(body io.Reader) string {
	buf, err := io.ReadAll(io.LimitReader(body, maxOAuthErrorBodyBytes+1))
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxBodyBytes-int64(len(buf))))
	if err != nil || len(buf) > maxOAuthErrorBodyBytes {
		return OAuthCodeUnknown
	}
	return parseOAuthErrorCode(buf)
}

// parseOAuthErrorCode extracts the OAuth error code from a JSON error body in the
// order the upstream Codex CLI uses (codex-rs login oauth error.rs,
// TokenErrorDetail::parse): `error` when it is a non-empty string; else `error.code`
// when `error` is an object with a non-empty string `code`; else a non-empty string
// top-level `code`. Any other `error` value (null, number, bool, array, an object
// with no usable `code`) falls through to the top-level `code`.
//
// Keys match EXACTLY and case-sensitively, like upstream serde: `ERROR` or `Code`
// is not a recognised key. (encoding/json struct decoding would fold key case, so
// objects are decoded key by key instead.) A recognised key that appears more than
// once in the same object is ambiguous and yields OAuthCodeUnknown rather than
// relying on first- or last-wins.
//
// An empty body, JSON null, or an object carrying no code yields ""; malformed JSON
// or a non-object top level yields OAuthCodeUnknown. A found code is passed through
// normaliseOAuthCode, so the result is always "" or a member of the closed set.
func parseOAuthErrorCode(buf []byte) string {
	if len(bytes.TrimSpace(buf)) == 0 {
		return ""
	}
	top, isNull, err := decodeJSONObject(buf, "error", "code")
	if err != nil {
		return OAuthCodeUnknown
	}
	if isNull {
		return ""
	}
	errVal := top["error"]
	if s, ok := nonEmptyJSONString(errVal); ok {
		return normaliseOAuthCode(s)
	}
	if len(errVal) > 0 && errVal[0] == '{' {
		nested, _, err := decodeJSONObject(errVal, "code")
		if err != nil {
			return OAuthCodeUnknown
		}
		if s, ok := nonEmptyJSONString(nested["code"]); ok {
			return normaliseOAuthCode(s)
		}
	}
	if s, ok := nonEmptyJSONString(top["code"]); ok {
		return normaliseOAuthCode(s)
	}
	return ""
}

// errOAuthBodyShape reports a body that is not a single JSON object (or null), or an
// object repeating one of the keys the caller asked for.
var errOAuthBodyShape = errors.New("codexauth: oauth error body is not an unambiguous JSON object")

// decodeJSONObject decodes buf, which must hold exactly one JSON value, as an object
// and returns the raw values of the requested keys, matched exactly (case-sensitive).
// isNull reports a bare JSON null. A requested key occurring more than once, a
// non-object value, malformed JSON or trailing data is an error.
func decodeJSONObject(buf []byte, keys ...string) (fields map[string]json.RawMessage, isNull bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(buf))
	tok, err := dec.Token()
	if err != nil {
		return nil, false, err
	}
	if tok == nil {
		isNull = true
	} else {
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			return nil, false, errOAuthBodyShape
		}
		fields = make(map[string]json.RawMessage, len(keys))
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, false, err
			}
			key, _ := keyTok.(string)
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, false, err
			}
			if !slices.Contains(keys, key) {
				continue
			}
			if _, dup := fields[key]; dup {
				return nil, false, errOAuthBodyShape
			}
			fields[key] = raw
		}
		if _, err := dec.Token(); err != nil { // the closing '}'
			return nil, false, err
		}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false, errOAuthBodyShape
	}
	return fields, isNull, nil
}

// nonEmptyJSONString decodes raw as a JSON string and reports whether it is one
// with non-whitespace content.
func nonEmptyJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
		return "", false
	}
	return s, true
}

// normaliseOAuthCode trims and ASCII-lowercases a provider code and maps it into the
// closed set: an allowlisted value is returned as that constant, anything else is
// OAuthCodeUnknown. Lowering is ASCII-only on purpose: strings.ToLower would fold
// non-ASCII look-alikes (e.g. KELVIN SIGN to 'k') onto an allowlisted spelling.
func normaliseOAuthCode(raw string) string {
	s := strings.TrimSpace(raw)
	// Fast path, not a safety bound: nothing longer than the longest allowlisted code
	// can match the switch below, so skip the copy. Correctness rests on the switch.
	if len(s) > len(OAuthCodeRefreshTokenInvalidated) {
		return OAuthCodeUnknown
	}
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	switch string(b) {
	case OAuthCodeRefreshTokenExpired:
		return OAuthCodeRefreshTokenExpired
	case OAuthCodeRefreshTokenReused:
		return OAuthCodeRefreshTokenReused
	case OAuthCodeRefreshTokenInvalidated:
		return OAuthCodeRefreshTokenInvalidated
	case OAuthCodeInvalidGrant:
		return OAuthCodeInvalidGrant
	case OAuthCodeInvalidRequest:
		return OAuthCodeInvalidRequest
	case OAuthCodeInvalidClient:
		return OAuthCodeInvalidClient
	case OAuthCodeUnauthorizedClient:
		return OAuthCodeUnauthorizedClient
	case OAuthCodeUnsupportedGrantType:
		return OAuthCodeUnsupportedGrantType
	case OAuthCodeInvalidScope:
		return OAuthCodeInvalidScope
	default:
		return OAuthCodeUnknown
	}
}
