package codexauth

import (
	"bytes"
	"encoding/json"
	"io"
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
// top-level `code`. An empty body or one carrying no code yields ""; malformed JSON
// yields OAuthCodeUnknown. A found code is passed through normaliseOAuthCode, so the
// result is always "" or a member of the closed set.
func parseOAuthErrorCode(buf []byte) string {
	if len(bytes.TrimSpace(buf)) == 0 {
		return ""
	}
	var top struct {
		Error json.RawMessage `json:"error"`
		Code  json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(buf, &top); err != nil {
		return OAuthCodeUnknown
	}
	if s, ok := nonEmptyJSONString(top.Error); ok {
		return normaliseOAuthCode(s)
	}
	var nested struct {
		Code json.RawMessage `json:"code"`
	}
	if len(top.Error) > 0 && top.Error[0] == '{' && json.Unmarshal(top.Error, &nested) == nil {
		if s, ok := nonEmptyJSONString(nested.Code); ok {
			return normaliseOAuthCode(s)
		}
	}
	if s, ok := nonEmptyJSONString(top.Code); ok {
		return normaliseOAuthCode(s)
	}
	return ""
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
