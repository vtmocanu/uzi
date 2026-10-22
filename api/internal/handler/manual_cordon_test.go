package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManualCordonReason(t *testing.T) {
	jsonReason := func(s string) string {
		b, err := json.Marshal(map[string]string{"reason": s})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, tc := range []struct {
		name, body, want, errText string
		valid, ok                 bool
	}{
		{name: "empty body", ok: true},
		{name: "missing reason", body: "{}", ok: true},
		{name: "null reason", body: `{"reason":null}`, ok: true},
		{name: "empty reason", body: `{"reason":""}`, valid: true, ok: true},
		{name: "500 emoji", body: jsonReason(strings.Repeat("😀", 500)), want: strings.Repeat("😀", 500), valid: true, ok: true},
		{name: "null object", body: "null", errText: "invalid cordon request"},
		{name: "array", body: "[]", errText: "invalid cordon request"},
		{name: "unknown field", body: `{"actor":"other"}`, errText: "invalid cordon request"},
		{name: "wrong reason type", body: `{"reason":42}`, errText: "invalid cordon request"},
		{name: "malformed", body: "{", errText: "invalid cordon request"},
		{name: "trailing object", body: "{} {}", errText: "invalid cordon request"},
		{name: "trailing junk", body: "{} x", errText: "invalid cordon request"},
		{name: "oversize", body: jsonReason(strings.Repeat("x", 8192)), errText: "invalid cordon request"},
		{name: "501 emoji", body: jsonReason(strings.Repeat("😀", 501)), errText: "reason must be at most 500 codepoints"},
		{name: "ANSI before sanitization", body: jsonReason(strings.Repeat("\x1b[31m", 101)), errText: "reason must be at most 500 codepoints"},
		{name: "501 before sanitization", body: jsonReason("a" + strings.Repeat("\n", 500)), errText: "reason must be at most 500 codepoints"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			reason, ok := manualCordonReason(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
			if ok != tc.ok {
				t.Fatalf("accepted = %v, want %v; body: %s", ok, tc.ok, rec.Body.String())
			}
			if tc.ok {
				if reason.Valid != tc.valid || reason.String != tc.want {
					t.Fatalf("reason = %#v, want valid=%v value=%q", reason, tc.valid, tc.want)
				}
			} else {
				var body struct {
					Error string `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusBadRequest || body.Error != tc.errText {
					t.Fatalf("response = %d %s, want 400 %q", rec.Code, rec.Body.String(), tc.errText)
				}
			}
		})
	}
}
