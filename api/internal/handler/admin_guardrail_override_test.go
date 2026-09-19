package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// postOverrideReason drives SetRepoGuardrailOverride with the given raw reason and an
// admin actor, WITHOUT a DB: the reason validation runs before any h.q call, so a nil
// store is never dereferenced on the rejection paths. It returns the recorder.
func postOverrideReason(t *testing.T, reason string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{} // nil q: the validation paths below never reach it
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	body, _ := json.Marshal(setGuardrailOverrideRequest{Reason: reason})
	r := httptest.NewRequest(http.MethodPost, "/admin/repos/x/guardrail-override", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.SetRepoGuardrailOverride(w, r)
	return w
}

// TestGuardrailOverrideReasonValidation pins the write-side backstop (PRD #66 M8
// audit hardening): the admin's reason is rejected BEFORE it is stored when it is
// empty, over-long, or carries a control character — so a forged audit line cannot be
// smuggled into M9's badge / admin list / a plain-text CLI or log sink. All three
// rejections happen before the DB write (a nil store would panic otherwise).
func TestGuardrailOverrideReasonValidation(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   int
	}{
		{"empty", "", http.StatusBadRequest},
		{"whitespace only", "   \t  ", http.StatusBadRequest}, // TrimSpace → empty
		{"newline (forged audit line)", "looks fine\nActor: someone-else approved", http.StatusBadRequest},
		{"ansi escape", "reason\x1b[31mred", http.StatusBadRequest},
		{"carriage return", "a\rb", http.StatusBadRequest},
		// Trojan-Source display spoof: a right-to-left override (U+202E, Unicode
		// category Cf) that unicode.IsControl does NOT catch — it must still be
		// rejected write-side so a member's reason cannot visually reorder the text
		// the approving admin reads (issue #1432, via termsafe.Unsafe).
		{"bidi override (trojan source)", "allow" + string(rune(0x202e)) + "detsurt", http.StatusBadRequest},
		{"zero-width space", "al" + string(rune(0x200b)) + "low", http.StatusBadRequest},
		{"too long", strings.Repeat("x", maxGuardrailOverrideReasonBytes+1), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postOverrideReason(t, tc.reason)
			if w.Code != tc.want {
				t.Fatalf("reason %q → status %d, want %d", tc.reason, w.Code, tc.want)
			}
		})
	}
}

// postDecisionNote drives ApproveGuardrailOverrideRequest with the given raw
// decision_note and an admin actor, WITHOUT a DB: the optional-note validation
// (validateOptionalGuardrailNote) runs before h.pool.Begin, so a nil pool/store is
// never reached on the rejection paths this test exercises. It returns the recorder.
func postDecisionNote(t *testing.T, note string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{} // nil pool/q: an INVALID note is rejected before any DB access
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	body, _ := json.Marshal(decideGuardrailOverrideRequestBody{DecisionNote: note})
	r := httptest.NewRequest(http.MethodPost, "/admin/override-requests/x/approve", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.ApproveGuardrailOverrideRequest(w, r)
	return w
}

// TestGuardrailDecisionNoteValidation pins the write-side screen on the OPTIONAL admin
// decision note (issue #1432 M3). The note is admin-authored text surfaced back to the
// requester on their Repos page, a cross-user boundary, so it must get the same
// termsafe.Unsafe + length treatment as the override reason. Only INVALID notes are
// tested here — they reject (400/422) before the handler reaches the DB, so a nil pool
// is never dereferenced (a valid/empty note would proceed to the transaction).
func TestGuardrailDecisionNoteValidation(t *testing.T) {
	cases := []struct {
		name string
		note string
		want int
	}{
		{"newline (forged audit line)", "ok\nActor: someone else", http.StatusBadRequest},
		{"ansi escape", "note\x1b[31mred", http.StatusBadRequest},
		{"bidi override (trojan source)", "allow" + string(rune(0x202e)) + "detsurt", http.StatusBadRequest},
		{"zero-width space", "al" + string(rune(0x200b)) + "low", http.StatusBadRequest},
		{"too long", strings.Repeat("x", maxGuardrailOverrideReasonBytes+1), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postDecisionNote(t, tc.note)
			if w.Code != tc.want {
				t.Fatalf("note %q → status %d, want %d", tc.note, w.Code, tc.want)
			}
		})
	}
}
