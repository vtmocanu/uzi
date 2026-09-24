package codexauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// refreshWithBody runs Refresh against a fake transport that answers the token
// endpoint with status and body, and returns the resulting *AuthError.
func refreshWithBody(t *testing.T, refreshToken string, status int, body string) *AuthError {
	t.Helper()
	tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
		return jsonResponse(status, body), nil
	})
	_, err := newTestClient(tr).Refresh(context.Background(), refreshToken)
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("err = %v, want *AuthError", err)
	}
	if tr.counts[oauthKey] != 1 {
		t.Fatalf("oauth endpoint hit %d times, want 1", tr.counts[oauthKey])
	}
	return authErr
}

var allowlistedOAuthCodes = []string{
	OAuthCodeRefreshTokenExpired,
	OAuthCodeRefreshTokenReused,
	OAuthCodeRefreshTokenInvalidated,
	OAuthCodeInvalidGrant,
	OAuthCodeInvalidRequest,
	OAuthCodeInvalidClient,
	OAuthCodeUnauthorizedClient,
	OAuthCodeUnsupportedGrantType,
	OAuthCodeInvalidScope,
}

// bodyShapes are the three places the upstream Codex CLI looks for a code.
var bodyShapes = map[string]func(code string) string{
	"error_string": func(c string) string { return fmt.Sprintf(`{"error":%q,"error_description":"desc"}`, c) },
	"error_object": func(c string) string { return fmt.Sprintf(`{"error":{"code":%q,"message":"msg"}}`, c) },
	"top_code":     func(c string) string { return fmt.Sprintf(`{"code":%q,"message":"msg"}`, c) },
}

func TestRefreshOAuthCodeAllowlistedShapes(t *testing.T) {
	for _, code := range allowlistedOAuthCodes {
		for shape, build := range bodyShapes {
			for _, variant := range []string{code, strings.ToUpper(code), "  " + code + "\t"} {
				t.Run(shape+"/"+variant, func(t *testing.T) {
					authErr := refreshWithBody(t, assembleToken("refresh"), http.StatusBadRequest, build(variant))
					if authErr.OAuthCode != code {
						t.Fatalf("OAuthCode = %q, want %q", authErr.OAuthCode, code)
					}
					if authErr.Op != "refresh" || authErr.StatusCode != http.StatusBadRequest {
						t.Fatalf("op/status = %q/%d", authErr.Op, authErr.StatusCode)
					}
					want := fmt.Sprintf("codexauth: refresh: provider returned status 400 (oauth_error=%s)", code)
					if authErr.Error() != want {
						t.Fatalf("Error() = %q, want %q", authErr.Error(), want)
					}
				})
			}
		}
	}
}

func TestRefreshOAuthCodeUnrecognisedOrAbsent(t *testing.T) {
	padded := `{"error":"invalid_grant","pad":"` + strings.Repeat("x", maxOAuthErrorBodyBytes) + `"}`
	exact := `{"error":"invalid_grant","pad":"`
	exact += strings.Repeat("x", maxOAuthErrorBodyBytes-len(exact)-2) + `"}`
	if len(exact) != maxOAuthErrorBodyBytes {
		t.Fatalf("fixture length %d, want %d", len(exact), maxOAuthErrorBodyBytes)
	}
	cases := []struct {
		name, body, want string
	}{
		{"unknown_code", `{"error":"server_error"}`, OAuthCodeUnknown},
		{"malformed_json", `{"error":"invalid_grant"`, OAuthCodeUnknown},
		{"not_json", `<html>bad gateway</html>`, OAuthCodeUnknown},
		{"json_array", `["invalid_grant"]`, OAuthCodeUnknown},
		{"empty_body", ``, ""},
		{"whitespace_body", " \n ", ""},
		{"json_null", `null`, ""},
		{"no_code", `{"message":"nope"}`, ""},
		{"empty_error_string", `{"error":""}`, ""},
		{"empty_error_falls_through_to_code", `{"error":"","code":"invalid_grant"}`, OAuthCodeInvalidGrant},
		{"error_object_without_code_falls_through", `{"error":{"message":"m"},"code":"refresh_token_reused"}`, OAuthCodeRefreshTokenReused},
		{"error_string_wins_over_code", `{"error":"invalid_client","code":"invalid_grant"}`, OAuthCodeInvalidClient},
		{"non_string_error", `{"error":42}`, ""},
		{"non_string_code", `{"code":42}`, ""},
		{"oversized_body", padded, OAuthCodeUnknown},
		{"exactly_limit_body", exact, OAuthCodeInvalidGrant},
		{"odd_chars", `{"error":"invalid-grant"}`, OAuthCodeUnknown},
		{"inner_space", `{"error":"invalid grant"}`, OAuthCodeUnknown},
		{"suffix", `{"error":"invalid_grant_x"}`, OAuthCodeUnknown},
		{"kelvin_sign_lookalike", `{"error":"refresh_toKen_expired"}`, OAuthCodeUnknown},
		{"overlong", `{"error":"` + strings.Repeat("a", 200) + `"}`, OAuthCodeUnknown},
		{"key_case_upper_error", `{"ERROR":"invalid_grant"}`, ""},
		{"key_case_title_nested", `{"Error":{"Code":"refresh_token_reused"}}`, ""},
		{"key_case_nested_code", `{"error":{"CODE":"refresh_token_reused"}}`, ""},
		{"key_case_top_code", `{"Code":"invalid_grant"}`, ""},
		{"key_case_ignored_beside_exact", `{"ERROR":"invalid_client","error":"invalid_grant"}`, OAuthCodeInvalidGrant},
		{"error_null_falls_through_to_code", `{"error":null,"code":"invalid_grant"}`, OAuthCodeInvalidGrant},
		{"error_array_falls_through_to_code", `{"error":["invalid_client"],"code":"invalid_grant"}`, OAuthCodeInvalidGrant},
		{"error_bool_falls_through_to_code", `{"error":true,"code":"invalid_grant"}`, OAuthCodeInvalidGrant},
		{"error_number_falls_through_to_code", `{"error":42,"code":"invalid_grant"}`, OAuthCodeInvalidGrant},
		{"nested_code_null", `{"error":{"code":null}}`, ""},
		{"nested_code_null_falls_through_to_code", `{"error":{"code":null},"code":"refresh_token_expired"}`, OAuthCodeRefreshTokenExpired},
		{"nested_code_wins_over_top_code", `{"error":{"code":"refresh_token_reused"},"code":"invalid_grant"}`, OAuthCodeRefreshTokenReused},
		{"duplicate_error_key", `{"error":"invalid_client","error":"invalid_grant"}`, OAuthCodeUnknown},
		{"duplicate_top_code_key", `{"code":"invalid_client","code":"invalid_grant"}`, OAuthCodeUnknown},
		{"duplicate_nested_code_key", `{"error":{"code":"invalid_client","code":"invalid_grant"}}`, OAuthCodeUnknown},
		{"duplicate_unrecognised_key_ignored", `{"message":"a","message":"b","error":"invalid_grant"}`, OAuthCodeInvalidGrant},
		{"trailing_value", `{"error":"invalid_grant"} {}`, OAuthCodeUnknown},
		{"json_string_top_level", `"invalid_grant"`, OAuthCodeUnknown},
		{"whitespace_around_values", " { \"error\" :\n { \"code\" : \"invalid_grant\" } } ", OAuthCodeInvalidGrant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authErr := refreshWithBody(t, assembleToken("refresh"), http.StatusUnauthorized, tc.body)
			if authErr.OAuthCode != tc.want {
				t.Fatalf("OAuthCode = %q, want %q", authErr.OAuthCode, tc.want)
			}
			if tc.want == "" && authErr.Error() != "codexauth: refresh: provider returned status 401" {
				t.Fatalf("Error() = %q, want the unchanged message", authErr.Error())
			}
		})
	}
}

// TestRefreshOAuthErrorNeverEchoesProviderText presents a refresh token and a reply
// that echoes it (and a description) in every field the parser looks at; none of the
// error's renderings may carry either.
func TestRefreshOAuthErrorNeverEchoesProviderText(t *testing.T) {
	token := "rt-" + strings.Repeat("a", 8) + "SECRET"
	desc := "description-" + "free-text-marker"
	bodies := []string{
		fmt.Sprintf(`{"error":%q,"error_description":%q}`, token, desc),
		fmt.Sprintf(`{"error":{"code":%q,"message":%q},"error_description":%q}`, token, token+desc, desc),
		fmt.Sprintf(`{"code":%q,"message":%q,"error_description":%q}`, token, token, desc),
		fmt.Sprintf(`{"error":"invalid_grant","error_description":%q,"message":%q}`, token+" "+desc, token),
	}
	for i, body := range bodies {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			authErr := refreshWithBody(t, token, http.StatusBadRequest, body)
			renderings := []string{
				authErr.Error(),
				fmt.Sprintf("%+v", error(authErr)),
				fmt.Sprintf("%#v", authErr),
				fmt.Sprintf("%#v", *authErr),
				authErr.OAuthCode,
			}
			for _, r := range renderings {
				if strings.Contains(r, token) || strings.Contains(r, "SECRET") || strings.Contains(r, desc) {
					t.Fatalf("rendering leaked provider text: %q", r)
				}
			}
		})
	}
}

func TestRefreshMaterialRejectedMatrix(t *testing.T) {
	rejection := []string{
		OAuthCodeRefreshTokenExpired, OAuthCodeRefreshTokenReused,
		OAuthCodeRefreshTokenInvalidated, OAuthCodeInvalidGrant,
	}
	type row struct {
		op     string
		status int
		code   string
		want   bool
	}
	var rows []row
	for _, c := range rejection {
		rows = append(rows,
			row{"refresh", http.StatusBadRequest, c, true},
			row{"refresh", http.StatusUnauthorized, c, true},
			row{"refresh", http.StatusForbidden, c, false},
			row{"refresh", http.StatusInternalServerError, c, false},
			row{"refresh", http.StatusTooManyRequests, c, false},
			row{"discover_identity", http.StatusUnauthorized, c, false},
			row{"read_usage", http.StatusBadRequest, c, false},
		)
	}
	for _, c := range []string{
		OAuthCodeInvalidRequest, OAuthCodeInvalidClient, OAuthCodeUnauthorizedClient,
		OAuthCodeUnsupportedGrantType, OAuthCodeInvalidScope, OAuthCodeUnknown, "",
	} {
		rows = append(rows,
			row{"refresh", http.StatusBadRequest, c, false},
			row{"refresh", http.StatusUnauthorized, c, false},
		)
	}
	for _, r := range rows {
		t.Run(fmt.Sprintf("%s/%d/%s", r.op, r.status, r.code), func(t *testing.T) {
			e := &AuthError{Op: r.op, StatusCode: r.status, OAuthCode: r.code}
			if got := e.RefreshMaterialRejected(); got != r.want {
				t.Fatalf("RefreshMaterialRejected() = %v, want %v", got, r.want)
			}
		})
	}
}

// TestRefreshMaterialRejectedEndToEnd drives the predicate through the real parse.
func TestRefreshMaterialRejectedEndToEnd(t *testing.T) {
	if !refreshWithBody(t, assembleToken("refresh"), http.StatusUnauthorized, `{"error":{"code":"refresh_token_reused"}}`).RefreshMaterialRejected() {
		t.Fatal("reused 401 not classified as material rejection")
	}
	if refreshWithBody(t, assembleToken("refresh"), http.StatusUnauthorized, ``).RefreshMaterialRejected() {
		t.Fatal("code-less 401 classified as material rejection")
	}
}

// countingReader serves n bytes of a JSON-ish body and records how much was read.
type countingReader struct {
	remaining int64
	read      int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = ' '
	}
	r.remaining -= int64(len(p))
	r.read += int64(len(p))
	return len(p), nil
}

func TestRefreshErrorBodyDrainedBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int64
		wantRead int64
	}{
		{"large_drained_fully", 64 << 10, 64 << 10},
		{"huge_drain_capped", 4 << 20, maxBodyBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rd := &countingReader{remaining: tc.size}
			tr := newCountingTransport(func(string, *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(rd), Header: http.Header{}}, nil
			})
			_, err := newTestClient(tr).Refresh(context.Background(), assembleToken("refresh"))
			var authErr *AuthError
			if !errors.As(err, &authErr) || authErr.OAuthCode != OAuthCodeUnknown {
				t.Fatalf("err = %v, want *AuthError with unknown code", err)
			}
			if rd.read != tc.wantRead {
				t.Fatalf("read %d bytes, want %d", rd.read, tc.wantRead)
			}
		})
	}
}
