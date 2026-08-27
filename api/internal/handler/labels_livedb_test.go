package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
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
