package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeProjectSync records the last Adopt/Disable call and returns a scripted error,
// standing in for *forgesvc.ProjectSyncService so the handler is exercised without
// the forge/store machinery.
type fakeProjectSync struct {
	adoptErr     error
	disableErr   error
	provisionErr error

	gotRepoID      uuid.UUID
	gotNumber      int
	gotOwnerKind   forge.ProjectV2OwnerKind
	gotTitle       string
	adoptCalls     int
	disableCalls   int
	provisionCalls int

	forwardErr   error
	forwardCalls int
	gotIssueIID  int64
	gotTarget    string

	status      forgesvc.ProjectSyncStatus
	statusErr   error
	statusCalls int

	// PRD #557 board-access methods: record args + scripted err, mirroring above.
	visibilityReturn   bool
	visibilityErr      error
	visibilityCalls    int
	setVisibilityErr   error
	gotPublic          bool
	setVisibilityCalls int
	shareErr           error
	unshareErr         error
	gotUsername        string
	shareCalls         int
	unshareCalls       int

	// PRD #576 M1: scripted owner-type return + error for the Provision nudge.
	ownerType      forge.ProjectV2OwnerType
	ownerTypeErr   error
	ownerTypeCalls int
}

func (f *fakeProjectSync) Adopt(_ context.Context, repoID uuid.UUID, number int, kind forge.ProjectV2OwnerKind) error {
	f.adoptCalls++
	f.gotRepoID, f.gotNumber, f.gotOwnerKind = repoID, number, kind
	return f.adoptErr
}

func (f *fakeProjectSync) Provision(_ context.Context, repoID uuid.UUID, kind forge.ProjectV2OwnerKind, title string) error {
	f.provisionCalls++
	f.gotRepoID, f.gotOwnerKind, f.gotTitle = repoID, kind, title
	return f.provisionErr
}

func (f *fakeProjectSync) Disable(_ context.Context, repoID uuid.UUID) error {
	f.disableCalls++
	f.gotRepoID = repoID
	return f.disableErr
}

func (f *fakeProjectSync) ForwardMove(_ context.Context, repoID uuid.UUID, issueIID int64, target string) error {
	f.forwardCalls++
	f.gotRepoID, f.gotIssueIID, f.gotTarget = repoID, issueIID, target
	return f.forwardErr
}

func (f *fakeProjectSync) ProjectSyncStatus(_ context.Context, repoID uuid.UUID) (forgesvc.ProjectSyncStatus, error) {
	f.statusCalls++
	f.gotRepoID = repoID
	return f.status, f.statusErr
}

func (f *fakeProjectSync) GetVisibility(_ context.Context, repoID uuid.UUID) (bool, error) {
	f.visibilityCalls++
	f.gotRepoID = repoID
	return f.visibilityReturn, f.visibilityErr
}

func (f *fakeProjectSync) RepoOwnerType(_ context.Context, repoID uuid.UUID) (forge.ProjectV2OwnerType, error) {
	f.ownerTypeCalls++
	f.gotRepoID = repoID
	return f.ownerType, f.ownerTypeErr
}

func (f *fakeProjectSync) SetVisibility(_ context.Context, repoID uuid.UUID, public bool) error {
	f.setVisibilityCalls++
	f.gotRepoID, f.gotPublic = repoID, public
	return f.setVisibilityErr
}

func (f *fakeProjectSync) ShareWithUser(_ context.Context, repoID uuid.UUID, username string) error {
	f.shareCalls++
	f.gotRepoID, f.gotUsername = repoID, username
	return f.shareErr
}

func (f *fakeProjectSync) Unshare(_ context.Context, repoID uuid.UUID, username string) error {
	f.unshareCalls++
	f.gotRepoID, f.gotUsername = repoID, username
	return f.unshareErr
}

// postAdopt drives AdoptGithubProjectSync with an admin actor and the given repo id
// + raw body.
func postAdopt(t *testing.T, h *Handler, repoID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	r := httptest.NewRequest(http.MethodPost, "/repos/x/github-project-sync", bytes.NewReader(body))
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
	r := httptest.NewRequest(http.MethodDelete, "/repos/x/github-project-sync", nil)
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

// getStatus drives GetGithubProjectSyncStatus with an admin actor and the given repo id.
func getStatus(t *testing.T, h *Handler, repoID string) *httptest.ResponseRecorder {
	t.Helper()
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	r := httptest.NewRequest(http.MethodGet, "/repos/x/github-project-sync", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID)
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.GetGithubProjectSyncStatus(w, r)
	return w
}

// TestStatusRouteReturnsFields: the GET status route delegates to the service and
// renders its fields.
func TestStatusRouteReturnsFields(t *testing.T) {
	sync := &fakeProjectSync{status: forgesvc.ProjectSyncStatus{
		ProjectNumber: 42,
		OwnedByUzi:    true,
		LastSyncedAt:  pgtype.Timestamptz{Time: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), Valid: true},
		LastError:     pgtype.Text{String: "graphql boom", Valid: true},
		ItemCount:     3,
	}}
	h := &Handler{projectSync: sync}
	repoID := uuid.New()
	w := getStatus(t, h, repoID.String())
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if sync.statusCalls != 1 || sync.gotRepoID != repoID {
		t.Errorf("Status not delegated with path repo id: calls=%d id=%v", sync.statusCalls, sync.gotRepoID)
	}
	var body struct {
		ProjectNumber int64      `json:"project_number"`
		OwnedByUzi    bool       `json:"owned_by_uzi"`
		LastSyncedAt  *time.Time `json:"last_synced_at"`
		LastError     *string    `json:"last_error"`
		ItemCount     int        `json:"item_count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if body.ProjectNumber != 42 || !body.OwnedByUzi || body.ItemCount != 3 {
		t.Errorf("body = %+v, want project 42 owned item_count 3", body)
	}
	if body.LastSyncedAt == nil {
		t.Errorf("last_synced_at should be non-null")
	}
	if body.LastError == nil || *body.LastError != "graphql boom" {
		t.Errorf("last_error = %v, want graphql boom", body.LastError)
	}
}

// getOwnerType drives GetGithubProjectOwnerType with an admin actor and the given repo id.
func getOwnerType(t *testing.T, h *Handler, repoID string) *httptest.ResponseRecorder {
	t.Helper()
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	r := httptest.NewRequest(http.MethodGet, "/repos/x/github-project-sync/owner-type", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID)
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.GetGithubProjectOwnerType(w, r)
	return w
}

// TestOwnerTypeRouteReturnsType (PRD #576 M1): the owner-type route delegates to the
// service and renders the wire owner_type string.
func TestOwnerTypeRouteReturnsType(t *testing.T) {
	sync := &fakeProjectSync{ownerType: forge.OwnerTypeOrg}
	h := &Handler{projectSync: sync}
	repoID := uuid.New()
	w := getOwnerType(t, h, repoID.String())
	if w.Code != http.StatusOK {
		t.Fatalf("owner-type status = %d, want 200", w.Code)
	}
	if sync.ownerTypeCalls != 1 || sync.gotRepoID != repoID {
		t.Errorf("RepoOwnerType not delegated with path repo id: calls=%d id=%v", sync.ownerTypeCalls, sync.gotRepoID)
	}
	var body struct {
		OwnerType string `json:"owner_type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if body.OwnerType != "Organization" {
		t.Errorf("owner_type = %q, want Organization", body.OwnerType)
	}
}

// TestOwnerTypeRouteValidation: a bad repo id is a 400 before the service; a nil
// service is a clean 500.
func TestOwnerTypeRouteValidation(t *testing.T) {
	t.Run("invalid repo id", func(t *testing.T) {
		sync := &fakeProjectSync{}
		h := &Handler{projectSync: sync}
		w := getOwnerType(t, h, "not-a-uuid")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if sync.ownerTypeCalls != 0 {
			t.Errorf("service must not be called on a bad repo id")
		}
	})
	t.Run("service not wired", func(t *testing.T) {
		h := &Handler{}
		w := getOwnerType(t, h, uuid.New().String())
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}

// TestStatusRouteNoLinkIs404: a repo with no link (pgx.ErrNoRows) returns 404.
func TestStatusRouteNoLinkIs404(t *testing.T) {
	sync := &fakeProjectSync{statusErr: pgx.ErrNoRows}
	h := &Handler{projectSync: sync}
	w := getStatus(t, h, uuid.New().String())
	if w.Code != http.StatusNotFound {
		t.Fatalf("no-link status = %d, want 404", w.Code)
	}
}

// TestStatusRouteValidation: an invalid repo id is a 400 before the service is called;
// a nil service is a clean 500.
func TestStatusRouteValidation(t *testing.T) {
	t.Run("invalid repo id", func(t *testing.T) {
		sync := &fakeProjectSync{}
		h := &Handler{projectSync: sync}
		w := getStatus(t, h, "not-a-uuid")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
		if sync.statusCalls != 0 {
			t.Errorf("service must not be called on a bad repo id")
		}
	})
	t.Run("service not wired", func(t *testing.T) {
		h := &Handler{}
		w := getStatus(t, h, uuid.New().String())
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}

// postProvision drives ProvisionGithubProjectSync with an admin actor and the given
// repo id + raw body.
func postProvision(t *testing.T, h *Handler, repoID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	r := httptest.NewRequest(http.MethodPost, "/repos/x/github-project-sync/provision", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID)
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.ProvisionGithubProjectSync(w, r)
	return w
}

// TestProvisionRouteDecodesAndDelegates: a valid request decodes owner_kind + title,
// passes the path repo id through, and returns 201.
func TestProvisionRouteDecodesAndDelegates(t *testing.T) {
	sync := &fakeProjectSync{}
	h := &Handler{projectSync: sync}
	repoID := uuid.New()
	w := postProvision(t, h, repoID.String(), []byte(`{"owner_kind":"org","title":"My Board"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if sync.provisionCalls != 1 {
		t.Fatalf("want 1 Provision call, got %d", sync.provisionCalls)
	}
	if sync.gotRepoID != repoID {
		t.Errorf("repo id = %v, want %v (must come from the path)", sync.gotRepoID, repoID)
	}
	if sync.gotOwnerKind != forge.OwnerOrg {
		t.Errorf("owner kind = %v, want OwnerOrg", sync.gotOwnerKind)
	}
	if sync.gotTitle != "My Board" {
		t.Errorf("title = %q, want My Board", sync.gotTitle)
	}
}

// TestProvisionOwnerKindDefault: an omitted owner_kind defaults to OwnerUser and an
// omitted title passes "" through (the service defaults it).
func TestProvisionOwnerKindDefault(t *testing.T) {
	sync := &fakeProjectSync{}
	h := &Handler{projectSync: sync}
	w := postProvision(t, h, uuid.New().String(), []byte(`{}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	if sync.gotOwnerKind != forge.OwnerUser {
		t.Errorf("default owner kind = %v, want OwnerUser", sync.gotOwnerKind)
	}
	if sync.gotTitle != "" {
		t.Errorf("title = %q, want empty (service defaults it)", sync.gotTitle)
	}
}

// TestProvisionRequestValidation: bad inputs are rejected BEFORE the service is called.
func TestProvisionRequestValidation(t *testing.T) {
	cases := []struct {
		name   string
		repoID string
		body   []byte
		want   int
	}{
		{"invalid repo id", "not-a-uuid", []byte(`{}`), http.StatusBadRequest},
		{"malformed body", uuid.New().String(), []byte("{"), http.StatusBadRequest},
		{"unknown owner kind", uuid.New().String(), []byte(`{"owner_kind":"team"}`), http.StatusBadRequest},
		{"unknown field", uuid.New().String(), []byte(`{"project_number":7}`), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sync := &fakeProjectSync{}
			h := &Handler{projectSync: sync}
			w := postProvision(t, h, tc.repoID, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if sync.provisionCalls != 0 {
				t.Errorf("service must not be called on a rejected request")
			}
		})
	}
}

// TestProvisionErrorMapping: each service sentinel maps to its documented 4xx; an
// unknown error is a 500 that does not leak its text.
func TestProvisionErrorMapping(t *testing.T) {
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
			sync := &fakeProjectSync{provisionErr: tc.err}
			h := &Handler{projectSync: sync}
			w := postProvision(t, h, uuid.New().String(), []byte(`{}`))
			if w.Code != tc.want {
				t.Fatalf("err %v → status %d, want %d", tc.err, w.Code, tc.want)
			}
			if tc.want == http.StatusInternalServerError && strings.Contains(w.Body.String(), "boom") {
				t.Errorf("500 body leaked the raw error text: %s", w.Body.String())
			}
		})
	}
}

// TestProvisionServiceNotWired: a nil projectSync returns a clean 500 rather than panics.
func TestProvisionServiceNotWired(t *testing.T) {
	h := &Handler{}
	w := postProvision(t, h, uuid.New().String(), []byte(`{}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}

// errAny is a non-sentinel error for the internal-error mapping case.
var errAny = errAnyErr{}

type errAnyErr struct{}

func (errAnyErr) Error() string { return "boom" }

// driveBoardAccess drives one of the four PRD #557 board-access handlers with an admin
// actor, the given repo id, method + path, and raw body.
func driveBoardAccess(t *testing.T, hfn http.HandlerFunc, repoID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	r := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID)
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	hfn(w, r)
	return w
}

// TestGetVisibilityRoute: the GET visibility route delegates and renders {"public":...}.
func TestGetVisibilityRoute(t *testing.T) {
	sync := &fakeProjectSync{visibilityReturn: true}
	h := &Handler{projectSync: sync}
	repoID := uuid.New()
	w := driveBoardAccess(t, h.GetGithubProjectVisibility, repoID.String(), http.MethodGet, "/repos/x/github-project-sync/visibility", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if sync.visibilityCalls != 1 || sync.gotRepoID != repoID {
		t.Errorf("GetVisibility not delegated with the path repo id: calls=%d id=%v", sync.visibilityCalls, sync.gotRepoID)
	}
	var body struct {
		Public bool `json:"public"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if !body.Public {
		t.Errorf("public = %v, want true", body.Public)
	}
}

// TestSetVisibilityRoute: PUT {"public":false} delegates the flag and echoes it back.
func TestSetVisibilityRoute(t *testing.T) {
	// Drive both directions with the fake pre-seeded to the OPPOSITE of what is sent,
	// so each case proves the handler overwrote the seed with the decoded flag — not
	// a zero value, and not a value hardcoded in either direction (a single false→false
	// send could not distinguish delegation from a hardcoded false).
	cases := []struct {
		name string
		seed bool
		send bool
	}{
		{"false-overwrites-seeded-true", true, false},
		{"true-overwrites-seeded-false", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sync := &fakeProjectSync{gotPublic: tc.seed}
			h := &Handler{projectSync: sync}
			repoID := uuid.New()
			w := driveBoardAccess(t, h.SetGithubProjectVisibility, repoID.String(), http.MethodPut, "/repos/x/github-project-sync/visibility", fmt.Sprintf(`{"public":%t}`, tc.send))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if sync.setVisibilityCalls != 1 {
				t.Fatalf("want 1 SetVisibility call, got %d", sync.setVisibilityCalls)
			}
			if sync.gotPublic != tc.send {
				t.Errorf("gotPublic = %v, want %v (the decoded flag, overwriting the %v seed)", sync.gotPublic, tc.send, tc.seed)
			}
			if sync.gotRepoID != repoID {
				t.Errorf("repo id = %v, want %v (must come from the path)", sync.gotRepoID, repoID)
			}
			var body struct {
				Public bool `json:"public"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("response not JSON: %v", err)
			}
			if body.Public != tc.send {
				t.Errorf("echoed public = %v, want %v", body.Public, tc.send)
			}
		})
	}
}

// TestShareRoute: POST {"username":"octocat"} grants Reader and returns 204.
func TestShareRoute(t *testing.T) {
	sync := &fakeProjectSync{}
	h := &Handler{projectSync: sync}
	repoID := uuid.New()
	w := driveBoardAccess(t, h.ShareGithubProjectSync, repoID.String(), http.MethodPost, "/repos/x/github-project-sync/collaborators", `{"username":"octocat"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if sync.shareCalls != 1 || sync.gotUsername != "octocat" {
		t.Errorf("ShareWithUser not delegated: calls=%d username=%q", sync.shareCalls, sync.gotUsername)
	}
	if sync.gotRepoID != repoID {
		t.Errorf("repo id = %v, want %v (must come from the path)", sync.gotRepoID, repoID)
	}
}

// TestUnshareRoute: DELETE {"username":"octocat"} revokes and returns 204 (a DELETE
// with a JSON body is read fine by net/http + chi).
func TestUnshareRoute(t *testing.T) {
	sync := &fakeProjectSync{}
	h := &Handler{projectSync: sync}
	repoID := uuid.New()
	w := driveBoardAccess(t, h.UnshareGithubProjectSync, repoID.String(), http.MethodDelete, "/repos/x/github-project-sync/collaborators", `{"username":"octocat"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if sync.unshareCalls != 1 || sync.gotUsername != "octocat" {
		t.Errorf("Unshare not delegated: calls=%d username=%q", sync.unshareCalls, sync.gotUsername)
	}
}

// TestCollaboratorEmptyUsername: an empty/blank username is a 400 on both share and
// unshare, BEFORE the service is called.
func TestCollaboratorEmptyUsername(t *testing.T) {
	cases := []struct {
		name   string
		hfn    func(h *Handler) http.HandlerFunc
		method string
		body   string
	}{
		{"share empty", func(h *Handler) http.HandlerFunc { return h.ShareGithubProjectSync }, http.MethodPost, `{"username":""}`},
		{"share whitespace", func(h *Handler) http.HandlerFunc { return h.ShareGithubProjectSync }, http.MethodPost, `{"username":"   "}`},
		{"unshare empty", func(h *Handler) http.HandlerFunc { return h.UnshareGithubProjectSync }, http.MethodDelete, `{"username":""}`},
		{"unshare whitespace", func(h *Handler) http.HandlerFunc { return h.UnshareGithubProjectSync }, http.MethodDelete, `{"username":"  "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sync := &fakeProjectSync{}
			h := &Handler{projectSync: sync}
			w := driveBoardAccess(t, tc.hfn(h), uuid.New().String(), tc.method, "/repos/x/github-project-sync/collaborators", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			if sync.shareCalls != 0 || sync.unshareCalls != 0 {
				t.Errorf("service must not be called on an empty username: share=%d unshare=%d", sync.shareCalls, sync.unshareCalls)
			}
		})
	}
}

// TestBoardAccessErrorMapping: each service sentinel maps to its documented status on
// every new route, including the new ErrProjectSyncUserNotFound → 422 (asserted on the
// share/unshare routes, where a bad username is the natural failure).
func TestBoardAccessErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"disabled", forgesvc.ErrProjectSyncDisabled, http.StatusConflict},
		{"not github", forgesvc.ErrProjectSyncNotGitHub, http.StatusUnprocessableEntity},
		{"user not found", forgesvc.ErrProjectSyncUserNotFound, http.StatusUnprocessableEntity},
		{"internal", errAny, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// GET visibility.
			gv := &fakeProjectSync{visibilityErr: tc.err}
			if w := driveBoardAccess(t, (&Handler{projectSync: gv}).GetGithubProjectVisibility, uuid.New().String(), http.MethodGet, "/repos/x/github-project-sync/visibility", ""); w.Code != tc.want {
				t.Errorf("GET visibility err %v → %d, want %d", tc.err, w.Code, tc.want)
			}
			// PUT visibility.
			sv := &fakeProjectSync{setVisibilityErr: tc.err}
			if w := driveBoardAccess(t, (&Handler{projectSync: sv}).SetGithubProjectVisibility, uuid.New().String(), http.MethodPut, "/repos/x/github-project-sync/visibility", `{"public":true}`); w.Code != tc.want {
				t.Errorf("PUT visibility err %v → %d, want %d", tc.err, w.Code, tc.want)
			}
			// POST collaborators (share).
			sh := &fakeProjectSync{shareErr: tc.err}
			shw := driveBoardAccess(t, (&Handler{projectSync: sh}).ShareGithubProjectSync, uuid.New().String(), http.MethodPost, "/repos/x/github-project-sync/collaborators", `{"username":"octocat"}`)
			if shw.Code != tc.want {
				t.Errorf("POST collaborators err %v → %d, want %d", tc.err, shw.Code, tc.want)
			}
			if tc.want == http.StatusInternalServerError && strings.Contains(shw.Body.String(), "boom") {
				t.Errorf("500 body leaked the raw error text: %s", shw.Body.String())
			}
			// DELETE collaborators (unshare).
			un := &fakeProjectSync{unshareErr: tc.err}
			if w := driveBoardAccess(t, (&Handler{projectSync: un}).UnshareGithubProjectSync, uuid.New().String(), http.MethodDelete, "/repos/x/github-project-sync/collaborators", `{"username":"octocat"}`); w.Code != tc.want {
				t.Errorf("DELETE collaborators err %v → %d, want %d", tc.err, w.Code, tc.want)
			}
		})
	}
}

// TestBoardAccessServiceNotWired: a nil projectSync returns a clean 500 on every new
// route rather than panics.
func TestBoardAccessServiceNotWired(t *testing.T) {
	cases := []struct {
		name   string
		hfn    func(h *Handler) http.HandlerFunc
		method string
		path   string
		body   string
	}{
		{"get visibility", func(h *Handler) http.HandlerFunc { return h.GetGithubProjectVisibility }, http.MethodGet, "/repos/x/github-project-sync/visibility", ""},
		{"put visibility", func(h *Handler) http.HandlerFunc { return h.SetGithubProjectVisibility }, http.MethodPut, "/repos/x/github-project-sync/visibility", `{"public":true}`},
		{"share", func(h *Handler) http.HandlerFunc { return h.ShareGithubProjectSync }, http.MethodPost, "/repos/x/github-project-sync/collaborators", `{"username":"octocat"}`},
		{"unshare", func(h *Handler) http.HandlerFunc { return h.UnshareGithubProjectSync }, http.MethodDelete, "/repos/x/github-project-sync/collaborators", `{"username":"octocat"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			w := driveBoardAccess(t, tc.hfn(h), uuid.New().String(), tc.method, tc.path, tc.body)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", w.Code)
			}
		})
	}
}
