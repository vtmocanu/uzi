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
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeProjectSync records the last Adopt/Disable call and returns a scripted error,
// standing in for *forgesvc.ProjectSyncService so the handler is exercised without
// the forge/store machinery.
type fakeProjectSync struct {
	adoptErr   error
	disableErr error

	gotRepoID    uuid.UUID
	gotNumber    int
	gotOwnerKind forge.ProjectV2OwnerKind
	adoptCalls   int
	disableCalls int
}

func (f *fakeProjectSync) Adopt(_ context.Context, repoID uuid.UUID, number int, kind forge.ProjectV2OwnerKind) error {
	f.adoptCalls++
	f.gotRepoID, f.gotNumber, f.gotOwnerKind = repoID, number, kind
	return f.adoptErr
}

func (f *fakeProjectSync) Disable(_ context.Context, repoID uuid.UUID) error {
	f.disableCalls++
	f.gotRepoID = repoID
	return f.disableErr
}

// postAdopt drives AdoptGithubProjectSync with an admin actor and the given repo id
// + raw body.
func postAdopt(t *testing.T, h *Handler, repoID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	r := httptest.NewRequest(http.MethodPost, "/admin/repos/x/github-project-sync", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID)
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.AdoptGithubProjectSync(w, r)
	return w
}

func adoptBody(t *testing.T, number int, ownerKind string) []byte {
	t.Helper()
	b, err := json.Marshal(adoptGithubProjectSyncRequest{ProjectNumber: number, OwnerKind: ownerKind})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestAdoptRouteDecodesAndDelegates: a valid request decodes the body, passes the
// path repo id + parsed owner kind through to the service, and returns 200.
func TestAdoptRouteDecodesAndDelegates(t *testing.T) {
	sync := &fakeProjectSync{}
	h := &Handler{projectSync: sync}
	repoID := uuid.New()
	w := postAdopt(t, h, repoID.String(), adoptBody(t, 7, "org"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if sync.adoptCalls != 1 {
		t.Fatalf("want 1 Adopt call, got %d", sync.adoptCalls)
	}
	if sync.gotRepoID != repoID {
		t.Errorf("repo id = %v, want %v (must come from the path)", sync.gotRepoID, repoID)
	}
	if sync.gotNumber != 7 {
		t.Errorf("project number = %d, want 7", sync.gotNumber)
	}
	if sync.gotOwnerKind != forge.OwnerOrg {
		t.Errorf("owner kind = %v, want OwnerOrg", sync.gotOwnerKind)
	}
}

// TestAdoptOwnerKindDefault: an omitted owner_kind defaults to OwnerUser.
func TestAdoptOwnerKindDefault(t *testing.T) {
	sync := &fakeProjectSync{}
	h := &Handler{projectSync: sync}
	w := postAdopt(t, h, uuid.New().String(), adoptBody(t, 3, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if sync.gotOwnerKind != forge.OwnerUser {
		t.Errorf("default owner kind = %v, want OwnerUser", sync.gotOwnerKind)
	}
}

// TestAdoptRequestValidation: bad inputs are rejected BEFORE the service is called.
func TestAdoptRequestValidation(t *testing.T) {
	cases := []struct {
		name   string
		repoID string
		body   []byte
		want   int
	}{
		{"invalid repo id", "not-a-uuid", adoptBody(t, 7, "user"), http.StatusBadRequest},
		{"malformed body", uuid.New().String(), []byte("{"), http.StatusBadRequest},
		{"non-positive number", uuid.New().String(), adoptBody(t, 0, "user"), http.StatusBadRequest},
		{"unknown owner kind", uuid.New().String(), adoptBody(t, 7, "team"), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sync := &fakeProjectSync{}
			h := &Handler{projectSync: sync}
			w := postAdopt(t, h, tc.repoID, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if sync.adoptCalls != 0 {
				t.Errorf("service must not be called on a rejected request")
			}
		})
	}
}

// TestAdoptErrorMapping: each service sentinel maps to its documented 4xx; an
// unknown error is a 500 that does not leak its text.
func TestAdoptErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"disabled", forgesvc.ErrProjectSyncDisabled, http.StatusConflict},
		{"not github", forgesvc.ErrProjectSyncNotGitHub, http.StatusUnprocessableEntity},
		{"unsupported", forgesvc.ErrProjectSyncUnsupported, http.StatusUnprocessableEntity},
		{"missing scope", forgesvc.ErrProjectSyncMissingScope, http.StatusUnprocessableEntity},
		{"unknown repo", pgx.ErrNoRows, http.StatusNotFound},
		{"internal", errAny, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sync := &fakeProjectSync{adoptErr: tc.err}
			h := &Handler{projectSync: sync}
			w := postAdopt(t, h, uuid.New().String(), adoptBody(t, 7, "user"))
			if w.Code != tc.want {
				t.Fatalf("err %v → status %d, want %d", tc.err, w.Code, tc.want)
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			if _, ok := body["error"]; !ok {
				t.Errorf("error response must carry an {\"error\":...} body, got %s", w.Body.String())
			}
			// A 500 must not leak the raw internal error text to the client.
			if tc.want == http.StatusInternalServerError && strings.Contains(w.Body.String(), "boom") {
				t.Errorf("500 body leaked the raw error text: %s", w.Body.String())
			}
		})
	}
}

// TestAdoptServiceNotWired: a nil projectSync returns a clean 500 rather than panics.
func TestAdoptServiceNotWired(t *testing.T) {
	h := &Handler{}
	w := postAdopt(t, h, uuid.New().String(), adoptBody(t, 7, "user"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// TestDisableRoute delegates to Disable and returns 204.
func TestDisableRoute(t *testing.T) {
	sync := &fakeProjectSync{}
	h := &Handler{projectSync: sync}
	repoID := uuid.New()
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	r := httptest.NewRequest(http.MethodDelete, "/admin/repos/x/github-project-sync", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.DisableGithubProjectSync(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if sync.disableCalls != 1 || sync.gotRepoID != repoID {
		t.Errorf("Disable not delegated with the path repo id: calls=%d id=%v", sync.disableCalls, sync.gotRepoID)
	}
}

// errAny is a non-sentinel error for the internal-error mapping case.
var errAny = errAnyErr{}

type errAnyErr struct{}

func (errAnyErr) Error() string { return "boom" }
