package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1907 M4, D11: product name and description are untrusted display strings,
// validated on write. These are DB-free: every refusal happens before the handler
// touches h.q / h.pool (a zero Handler would panic on a nil pool if it got that far,
// which is itself the assertion that validation runs first).

// productValidationCases are the refusals both fields share. The literals are built
// from escapes, never raw bytes in source.
var productValidationCases = []struct {
	name string
	in   string
}{
	{"newline forges a listing row", "Acme\nroot  admin@x"},
	{"carriage return", "Acme\rX"},
	{"tab", "Acme\tX"},
	{"escape sequence", "Acme\x1b[31mred"},
	{"NUL", "Acme\x00X"},
	{"DEL", "Acme\x7fX"},
	{"C1 control (CSI)", "Acme\u009bX"},
	{"bidi override RLO", "Acme\u202Egnp.exe"},
	{"bidi isolate LRI", "Acme\u2066X"},
	{"zero-width space", "Ac\u200Bme"},
	{"zero-width joiner", "Ac\u200Dme"},
	{"byte-order mark", "\uFEFFAcme"},
	{"invalid UTF-8", "Acme\xff"},
}

func TestValidateProductName(t *testing.T) {
	for _, tc := range productValidationCases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := validateProductName(tc.in); err == nil {
				t.Fatalf("validateProductName(%q) = %q, nil; want a refusal", tc.in, got)
			}
		})
	}
	refused := map[string]string{
		"empty":                  "",
		"whitespace only":        "   \t  ",
		"201 ASCII bytes":        strings.Repeat("a", 201),
		"201 bytes of 67 x euro": strings.Repeat("\u20AC", 67), // 3 bytes each: 67 runes, 201 bytes
	}
	for name, in := range refused {
		t.Run(name, func(t *testing.T) {
			if got, err := validateProductName(in); err == nil {
				t.Fatalf("validateProductName(%q) = %q, nil; want a refusal", in, got)
			}
		})
	}
	accepted := map[string]struct{ in, want string }{
		"plain":                    {"Acme CRM", "Acme CRM"},
		"trimmed":                  {"  Acme CRM \n", "Acme CRM"},
		"exactly 200 bytes":        {strings.Repeat("a", 200), strings.Repeat("a", 200)},
		"non-ASCII letters":        {"Caf\u00E9 \u6771\u4EAC", "Caf\u00E9 \u6771\u4EAC"},
		"200 bytes, multibyte cap": {strings.Repeat("\u20AC", 66) + "ab", strings.Repeat("\u20AC", 66) + "ab"},
	}
	for name, tc := range accepted {
		t.Run(name, func(t *testing.T) {
			got, err := validateProductName(tc.in)
			if err != nil {
				t.Fatalf("validateProductName(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("validateProductName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateProductDescription(t *testing.T) {
	for _, tc := range productValidationCases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := validateProductDescription(tc.in); err == nil {
				t.Fatalf("validateProductDescription(%q) = %q, nil; want a refusal", tc.in, got)
			}
		})
	}
	if got, err := validateProductDescription(strings.Repeat("d", 1001)); err == nil {
		t.Fatalf("a 1001-byte description was accepted (%d bytes)", len(got))
	}
	// 1001 bytes in 334 runes: the cap is bytes, not characters.
	if _, err := validateProductDescription(strings.Repeat("\u20AC", 333) + "ab"); err == nil {
		t.Fatal("a 1001-byte multibyte description was accepted")
	}
	accepted := map[string]struct{ in, want string }{
		"empty is allowed":   {"", ""},
		"whitespace trims":   {"   ", ""},
		"exactly 1000 bytes": {strings.Repeat("d", 1000), strings.Repeat("d", 1000)},
		"trimmed":            {" Syncs tickets. ", "Syncs tickets."},
	}
	for name, tc := range accepted {
		t.Run(name, func(t *testing.T) {
			got, err := validateProductDescription(tc.in)
			if err != nil {
				t.Fatalf("validateProductDescription(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("validateProductDescription(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// serveAdminProduct runs one handler with an admin in the context (the write group's
// RequireAuth + RequireAdmin are routing, not under test here) and a chi {id} param.
func serveAdminProduct(t *testing.T, hf http.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	if id != "" {
		rctx.URLParams.Add("id", id)
	}
	ctx := mw.ContextWithUser(req.Context(), store.User{ID: uuid.New(), IsAdmin: true, IsActive: true})
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	hf(rec, req.WithContext(ctx))
	return rec
}

func productJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The create and patch handlers refuse a bad body with a 400 before any store call.
func TestAdminProductWritesRejectBadInputBeforeTheStore(t *testing.T) {
	h := &Handler{} // nil q and pool: reaching the store would panic
	id := uuid.NewString()
	long := strings.Repeat("a", 201)
	cases := []struct {
		name   string
		hf     http.HandlerFunc
		method string
		id     string
		body   string
	}{
		{"create: not JSON", h.AdminCreateProduct, http.MethodPost, "", "{"},
		{"create: unknown field", h.AdminCreateProduct, http.MethodPost, "", `{"name":"a","created_by":"x"}`},
		{"create: empty name", h.AdminCreateProduct, http.MethodPost, "", `{"name":""}`},
		{"create: blank name", h.AdminCreateProduct, http.MethodPost, "", `{"name":"   "}`},
		{"create: name over cap", h.AdminCreateProduct, http.MethodPost, "", productJSON(t, map[string]string{"name": long})},
		{"create: control char in name", h.AdminCreateProduct, http.MethodPost, "", productJSON(t, map[string]string{"name": "a\x1b[2Jb"})},
		{"create: bidi in name", h.AdminCreateProduct, http.MethodPost, "", productJSON(t, map[string]string{"name": "a\u202Eb"})},
		{"create: newline in description", h.AdminCreateProduct, http.MethodPost, "",
			productJSON(t, map[string]string{"name": "ok", "description": "line1\nline2"})},
		{"create: description over cap", h.AdminCreateProduct, http.MethodPost, "",
			productJSON(t, map[string]string{"name": "ok", "description": strings.Repeat("d", 1001)})},
		{"create: unknown job type", h.AdminCreateProduct, http.MethodPost, "", `{"name":"ok","allowed_job_types":["research","code_review"]}`},
		{"create: allowed_job_types wrong shape", h.AdminCreateProduct, http.MethodPost, "", `{"name":"ok","allowed_job_types":"research"}`},
		{"patch: unknown job type", h.AdminPatchProduct, http.MethodPatch, id, `{"allowed_job_types":["nope"]}`},
		{"patch: empty job type entry", h.AdminPatchProduct, http.MethodPatch, id, `{"allowed_job_types":[""]}`},
		{"patch: bad id", h.AdminPatchProduct, http.MethodPatch, "not-a-uuid", `{"enabled":false}`},
		{"patch: empty body object", h.AdminPatchProduct, http.MethodPatch, id, `{}`},
		{"patch: name is immutable", h.AdminPatchProduct, http.MethodPatch, id, `{"name":"renamed"}`},
		{"patch: zero-width in description", h.AdminPatchProduct, http.MethodPatch, id,
			productJSON(t, map[string]string{"description": "a\u200Bb"})},
		{"patch: description over cap", h.AdminPatchProduct, http.MethodPatch, id,
			productJSON(t, map[string]string{"description": strings.Repeat("d", 1001)})},
		{"delete: bad id", h.AdminDeleteProduct, http.MethodDelete, "nope", ""},
		{"revoke: bad id", h.AdminRevokeProductToken, http.MethodPost, "nope", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAdminProduct(t, tc.hf, tc.method, tc.id, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d (%s), want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestValidateAllowedJobTypes (PRD #1908 D-C): each entry must be a known job type; the result is
// de-duplicated in first-seen order and never nil.
func TestValidateAllowedJobTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
		bad  bool
	}{
		{"nil is the empty list", nil, []string{}, false},
		{"empty stays empty", []string{}, []string{}, false},
		{"one known type", []string{"research"}, []string{"research"}, false},
		{"duplicates collapse", []string{"research", "research"}, []string{"research"}, false},
		{"unknown type", []string{"translate"}, nil, true},
		{"unknown after a known one", []string{"research", "translate"}, nil, true},
		{"empty entry", []string{""}, nil, true},
		{"case matters", []string{"Research"}, nil, true},
		{"whitespace is not trimmed", []string{" research"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateAllowedJobTypes(tc.in)
			if (err != nil) != tc.bad {
				t.Fatalf("err = %v, want error %v", err, tc.bad)
			}
			if !tc.bad && (got == nil || !slices.Equal(got, tc.want)) {
				t.Fatalf("got %#v, want %#v (non-nil)", got, tc.want)
			}
		})
	}
	if got := jobTypesOrEmpty(nil); got == nil || len(got) != 0 {
		t.Fatalf("jobTypesOrEmpty(nil) = %#v, want a non-nil empty slice (the wire is never null)", got)
	}
}
