package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// GetBoardPrefs / PutBoardPrefs (PRD #196 M3) end-to-end through the handler, against
// a real Postgres. Handler.q is a concrete type, so there is no fake-store seam — and
// the two things most worth pinning need a live DB anyway: the nullable extra_labels
// sentinel (NULL "not customized" vs a stored `[]` absolute set) surviving a
// round-trip through JSONB, and the ownership join in UpsertBoardPrefsForOwner.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

type boardPrefsFixture struct {
	h        *Handler
	pool     *pgxpool.Pool
	owner    store.User
	stranger store.User
	repoID   uuid.UUID
	// strangerRepoID is a repo owned by `stranger`; the owner writing to it must be
	// rejected by the upsert's ownership join (404), never silently written.
	strangerRepoID uuid.UUID
}

func newBoardPrefsFixture(ctx context.Context, t *testing.T) boardPrefsFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)

	f := boardPrefsFixture{
		pool:     pool,
		owner:    store.User{ID: uuid.New(), Email: fmt.Sprintf("bp-owner-%s@e2e", uuid.NewString()[:8])},
		stranger: store.User{ID: uuid.New(), Email: fmt.Sprintf("bp-other-%s@e2e", uuid.NewString()[:8])},
	}
	f.h = &Handler{
		pool: pool,
		q:    q,
		box:  box,
		cfg:  config.Config{},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyPRDLabel, Value: "PRD"},
		}}, time.Minute),
		svc:  forgesvc.New(q, box, 5*time.Second, nil),
		wsvc: workersvc.New(q, box, workersvc.Params{}),
	}

	sealed, err := box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.stranger.ID, f.stranger.Email)

	seedRepo := func(userID uuid.UUID, projectID int, path, bot string) uuid.UUID {
		connID, repoID := uuid.New(), uuid.New()
		mustExecT(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.example', $3, $4, $5)`, connID, userID, bot, projectID, sealed)
		mustExecT(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.example/g/bp', 'main', true)`, repoID, connID, projectID, path)
		return repoID
	}
	f.repoID = seedRepo(f.owner.ID, 1, "g/bp", "bot")
	f.strangerRepoID = seedRepo(f.stranger.ID, 2, "g/bp-stranger", "bot2")
	return f
}

func (f boardPrefsFixture) get(t *testing.T, user store.User, repoID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/repos/x/board/prefs", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.GetBoardPrefs(w, r)
	return w
}

func (f boardPrefsFixture) put(t *testing.T, user store.User, repoID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/repos/x/board/prefs", bytes.NewBufferString(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.PutBoardPrefs(w, r)
	return w
}

// decodePrefs unmarshals a response body into the raw JSON of extra_labels + show_all.
// extra_labels is kept as json.RawMessage so a test can tell JSON `null` from `[]` —
// the sentinel this whole feature turns on.
func decodePrefs(t *testing.T, w *httptest.ResponseRecorder) (extraLabelsRaw string, showAll bool) {
	t.Helper()
	var resp struct {
		ExtraLabels json.RawMessage `json:"extra_labels"`
		ShowAll     bool            `json:"show_all"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode prefs: %v (body %s)", err, w.Body.String())
	}
	return string(resp.ExtraLabels), resp.ShowAll
}

// No row yet ⇒ the unset defaults, at 200 (not 404): a fresh board is not an error,
// and extra_labels=null tells the client to fall back to the admin default.
func TestGetBoardPrefsNoRowReturnsNullDefaultsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardPrefsFixture(ctx, t)

	w := f.get(t, f.owner, f.repoID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	labels, showAll := decodePrefs(t, w)
	if labels != "null" {
		t.Errorf("extra_labels = %s, want null for an uncustomized board", labels)
	}
	if showAll {
		t.Errorf("show_all = true, want false when no row exists")
	}
}

// PUT then GET round-trips an absolute set and the show_all boolean.
func TestPutThenGetBoardPrefsRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardPrefsFixture(ctx, t)

	w := f.put(t, f.owner, f.repoID, `{"extra_labels":["bug","docs"],"show_all":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if labels, showAll := decodePrefs(t, w); labels != `["bug","docs"]` || !showAll {
		t.Errorf("put echo: extra_labels=%s show_all=%v, want [\"bug\",\"docs\"]/true", labels, showAll)
	}

	g := f.get(t, f.owner, f.repoID)
	if g.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", g.Code)
	}
	if labels, showAll := decodePrefs(t, g); labels != `["bug","docs"]` || !showAll {
		t.Errorf("get after put: extra_labels=%s show_all=%v, want [\"bug\",\"docs\"]/true", labels, showAll)
	}
}

// The sentinel that this feature turns on: `[]` (absolute empty set) and `null` (reset
// to "not customized") are DIFFERENT stored states and must survive the JSONB round
// trip distinct. A second PUT of null after an array reset clears the customization.
func TestPutBoardPrefsEmptyVsNullSentinelLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardPrefsFixture(ctx, t)

	// Absolute empty set: array present but empty. GET must return `[]`, not `null`.
	if w := f.put(t, f.owner, f.repoID, `{"extra_labels":[],"show_all":false}`); w.Code != http.StatusOK {
		t.Fatalf("put [] status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if labels, _ := decodePrefs(t, f.get(t, f.owner, f.repoID)); labels != "[]" {
		t.Errorf("extra_labels = %s, want [] — the empty absolute set must not collapse to null", labels)
	}

	// Reset: extra_labels null ⇒ stored SQL NULL ⇒ GET returns null again.
	if w := f.put(t, f.owner, f.repoID, `{"extra_labels":null,"show_all":false}`); w.Code != http.StatusOK {
		t.Fatalf("put null status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if labels, _ := decodePrefs(t, f.get(t, f.owner, f.repoID)); labels != "null" {
		t.Errorf("extra_labels = %s, want null after a reset", labels)
	}
}

// An extra_labels array key omitted entirely (only show_all sent) is treated as null /
// not customized, same as an explicit null.
func TestPutBoardPrefsAbsentExtraLabelsIsNullLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardPrefsFixture(ctx, t)

	if w := f.put(t, f.owner, f.repoID, `{"show_all":true}`); w.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	labels, showAll := decodePrefs(t, f.get(t, f.owner, f.repoID))
	if labels != "null" {
		t.Errorf("extra_labels = %s, want null when the key is absent", labels)
	}
	if !showAll {
		t.Errorf("show_all = false, want true")
	}
}

// A label containing a comma is rejected (ValidateLabel), and an over-cap array is
// rejected — both 400, both before any write.
func TestPutBoardPrefsRejectsInvalidLabelsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardPrefsFixture(ctx, t)

	if w := f.put(t, f.owner, f.repoID, `{"extra_labels":["bug,docs"],"show_all":false}`); w.Code != http.StatusBadRequest {
		t.Errorf("comma label status = %d, want 400", w.Code)
	}
	if w := f.put(t, f.owner, f.repoID, `{"extra_labels":[""],"show_all":false}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty label status = %d, want 400", w.Code)
	}

	labels := make([]string, maxBoardExtraLabels+1)
	for i := range labels {
		labels[i] = fmt.Sprintf("l%d", i)
	}
	body, err := json.Marshal(map[string]any{"extra_labels": labels, "show_all": false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if w := f.put(t, f.owner, f.repoID, string(body)); w.Code != http.StatusBadRequest {
		t.Errorf("over-cap status = %d, want 400 for %d labels", w.Code, len(labels))
	}

	// None of the rejected writes may have created a row.
	if labels, _ := decodePrefs(t, f.get(t, f.owner, f.repoID)); labels != "null" {
		t.Errorf("extra_labels = %s, want null — a rejected PUT must not write", labels)
	}
}

// Owner-scoping: writing prefs for a repo the caller does not own is a 404 (the upsert's
// ownership join matches nothing), and nothing is written under the true owner either.
func TestPutBoardPrefsForeignRepoIs404LiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardPrefsFixture(ctx, t)

	// A repo id that does not exist at all → repoForRequest 404.
	if w := f.put(t, f.owner, uuid.New(), `{"extra_labels":["bug"],"show_all":true}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown repo status = %d, want 404", w.Code)
	}

	// repoForRequest is per (repo, user) via GetRepoForUser, so the owner cannot even
	// address the stranger's repo — 404 there too, and no row for the stranger.
	if w := f.put(t, f.owner, f.strangerRepoID, `{"extra_labels":["bug"],"show_all":true}`); w.Code != http.StatusNotFound {
		t.Errorf("foreign repo status = %d, want 404", w.Code)
	}
	if labels, _ := decodePrefs(t, f.get(t, f.stranger, f.strangerRepoID)); labels != "null" {
		t.Errorf("stranger extra_labels = %s, want null — a foreign write must not land", labels)
	}
}
