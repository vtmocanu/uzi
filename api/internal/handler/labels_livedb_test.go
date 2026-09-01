package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Owner-scoping for the PRD #589 M4 sweep-label endpoints: a repo the caller does not own
// is a 404, and repoForRequest rejects it BEFORE any forge call — so this needs a real
// Postgres for the owner-scoped query but no live forge. Skipped unless UZI_TEST_DATABASE_URL
// is set; ./e2e/run-store-it.sh provides one. (The WARN/CONFIRM label logic itself is covered
// without a DB by the fake-forge unit tests in labels_test.go.)
func TestRepoLabelsForeignRepo404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// A repo owned by the stranger, POSTed to by the owner → 404 for both endpoints.
	strangerRepo := f.insertRepo(ctx, t, f.stranger, 7, "g/labels-stranger")

	for _, tc := range []struct {
		name string
		path string
		h    http.HandlerFunc
	}{
		{"check", "/labels/check", f.h.CheckRepoLabels},
		{"ensure", "/labels/ensure", f.h.EnsureRepoLabels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := userReq(http.MethodPost, "/api/repos/"+strangerRepo.String()+tc.path, `{"labels":["bug"]}`,
				f.owner.ID, map[string]string{"id": strangerRepo.String()})
			rec := httptest.NewRecorder()
			tc.h(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s on a foreign repo status = %d, want 404 (body %s)", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestRepoLabelsOversizeBody413LiveDB is the handler-level half of PRD #954 M3 (S3):
// an over-cap body on an OWNED labels route (so repoForRequest passes and DecodeJSONLimited
// is what fails) answers a truthful 413 carrying uzi's own prose — not the 400 the site
// used to return, and not net/http's "http: request body too large" literal. Needs a live
// DB because the 413 fires only AFTER the owner-scoped repoForRequest lookup.
func TestRepoLabelsOversizeBody413LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newScheduleFixture(ctx, t)

	// A well-formed JSON body strictly larger than the 1 MiB cap, so it crosses on SIZE
	// rather than on any malformation (a truncated body would 400 and prove nothing).
	body := `{"labels":["` + strings.Repeat("a", 1<<20) + `"]}`
	if len(body) <= 1<<20 {
		t.Fatalf("oversize fixture is %d bytes, not over the 1 MiB cap it exists to cross", len(body))
	}

	req := userReq(http.MethodPost, "/api/repos/"+f.repoID.String()+"/labels/check", body,
		f.owner.ID, map[string]string{"id": f.repoID.String()})
	rec := httptest.NewRecorder()
	f.h.CheckRepoLabels(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body status = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("413 body %q does not carry uzi's oversize prose", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "http:") {
		t.Fatalf("413 body leaked net/http's stdlib literal: %q", rec.Body.String())
	}
}
