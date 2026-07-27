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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// SetBoardOrder (PRD #102 M5) end-to-end through the handler, against a real Postgres.
//
// Handler.pool and Handler.q are concrete types, so there is no fake-store seam here —
// and the two things most worth pinning are not reachable from one anyway: the
// TRANSACTION spanning both freeze statements, and the empty-list guard, whose absence
// wipes every position on the board because `<> ALL('{}')` is TRUE for every row.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

type boardOrderFixture struct {
	h        *Handler
	pool     *pgxpool.Pool
	owner    store.User
	stranger store.User
	repoID   uuid.UUID
}

func newBoardOrderFixture(ctx context.Context, t *testing.T, iids ...int64) boardOrderFixture {
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

	f := boardOrderFixture{
		pool:     pool,
		owner:    store.User{ID: uuid.New(), Email: fmt.Sprintf("bo-owner-%s@e2e", uuid.NewString()[:8])},
		stranger: store.User{ID: uuid.New(), Email: fmt.Sprintf("bo-other-%s@e2e", uuid.NewString()[:8])},
		repoID:   uuid.New(),
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
	connID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.owner.ID, f.owner.Email)
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.stranger.ID, f.stranger.Email)
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.example', 'bot', 1, $3)`, connID, f.owner.ID, sealed)
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/bo', 'https://forge.example/g/bo', 'main', true)`, f.repoID, connID)
	for _, iid := range iids {
		mustExecT(ctx, t, pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, $2, 't', 'opened', '["PRD"]'::jsonb, 'https://x', true, now(), now())`,
			f.repoID, iid)
	}
	return f
}

// call issues PUT /repos/{id}/board/order as `user` and returns the recorder.
func (f boardOrderFixture) call(t *testing.T, user store.User, repoID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "/repos/x/board/order", bytes.NewBufferString(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", repoID.String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	f.h.SetBoardOrder(w, r)
	return w
}

func (f boardOrderFixture) positions(ctx context.Context, t *testing.T) map[int64]pgtype.Int8 {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT forge_issue_iid, board_position FROM issues WHERE repo_id = $1`, f.repoID)
	if err != nil {
		t.Fatalf("read positions: %v", err)
	}
	defer rows.Close()
	out := map[int64]pgtype.Int8{}
	for rows.Next() {
		var iid int64
		var pos pgtype.Int8
		if err := rows.Scan(&iid, &pos); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[iid] = pos
	}
	return out
}

// boardCardIIDs reads the order out of the RESPONSE body, which is what the client
// adopts — not out of the table. Manual mode is the identity function over this list,
// so this is the only assertion that actually covers what a user sees.
func boardCardIIDs(t *testing.T, w *httptest.ResponseRecorder) []int64 {
	t.Helper()
	var resp struct {
		Board struct {
			Cards []struct {
				IID int64 `json:"iid"`
			} `json:"cards"`
		} `json:"board"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode board: %v (body %s)", err, w.Body.String())
	}
	out := make([]int64, len(resp.Board.Cards))
	for i, c := range resp.Board.Cards {
		out[i] = c.IID
	}
	return out
}

// Acceptance criteria 2 and 3: the submitted order is what the board returns, positions
// are 1000/2000/3000, and an iid the repo does not hold is a per-iid no-op rather than a
// 404 that fails the whole freeze.
func TestSetBoardOrderAppliesSubmittedOrderLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardOrderFixture(ctx, t, 10, 20, 30)

	// Submitted deliberately out of iid order — with a fixture where the two coincide,
	// an implementation that ignored the body entirely would still pass.
	w := f.call(t, f.owner, f.repoID, `{"iids":[30,10,999,20]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got, want := boardCardIIDs(t, w), []int64{30, 10, 20}; !equalIIDs(got, want) {
		t.Errorf("board cards = %v, want %v — the response must already carry the new order", got, want)
	}

	pos := f.positions(ctx, t)
	for iid, want := range map[int64]int64{30: 1000, 10: 2000, 20: 4000} {
		if !pos[iid].Valid || pos[iid].Int64 != want {
			t.Errorf("issue %d board_position = %v, want %d", iid, pos[iid], want)
		}
	}
}

// Acceptance criterion 1: the endpoint is owner-scoped through repoForRequest, and a
// rejected request writes nothing. The second half matters as much as the status: a 404
// that had already run the freeze would be a cross-tenant write with a tidy error code.
func TestSetBoardOrderRejectsNonOwnerLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardOrderFixture(ctx, t, 10, 20)

	if w := f.call(t, f.owner, f.repoID, `{"iids":[20,10]}`); w.Code != http.StatusOK {
		t.Fatalf("owner seed call = %d, want 200", w.Code)
	}
	before := f.positions(ctx, t)

	w := f.call(t, f.stranger, f.repoID, `{"iids":[10,20]}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("stranger status = %d, want 404", w.Code)
	}
	after := f.positions(ctx, t)
	for iid, p := range before {
		if after[iid].Valid != p.Valid || after[iid].Int64 != p.Int64 {
			t.Errorf("issue %d moved from %v to %v under a non-owner request", iid, p, after[iid])
		}
	}
}

// Acceptance criterion 4, and the sharpest failure mode in the endpoint: an empty list
// must change NOTHING. Without the guard, ClearBoardOrderExcept's `<> ALL('{}')` matches
// every row and one empty submit silently erases a whole board's manual order.
func TestSetBoardOrderEmptyListIsANoOpLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardOrderFixture(ctx, t, 10, 20)

	if w := f.call(t, f.owner, f.repoID, `{"iids":[20,10]}`); w.Code != http.StatusOK {
		t.Fatalf("seed call = %d, want 200", w.Code)
	}
	if w := f.call(t, f.owner, f.repoID, `{"iids":[]}`); w.Code != http.StatusOK {
		t.Fatalf("empty-list status = %d, want 200", w.Code)
	}

	pos := f.positions(ctx, t)
	if !pos[20].Valid || pos[20].Int64 != 1000 || !pos[10].Valid || pos[10].Int64 != 2000 {
		t.Errorf("an empty submit changed the order: 20=%v 10=%v, want 1000/2000 — the len==0 guard is missing", pos[20], pos[10])
	}
}

// Acceptance criterion 5: an open card omitted from a later submit loses its position
// and therefore renders LAST in its column (ListIssuesByRepo's NULLS LAST). Asserted
// through the response body, since that ordering is the user-visible effect.
func TestSetBoardOrderOmittedCardFallsToTheEndLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardOrderFixture(ctx, t, 10, 20, 30)

	if w := f.call(t, f.owner, f.repoID, `{"iids":[30,20,10]}`); w.Code != http.StatusOK {
		t.Fatalf("seed call = %d, want 200", w.Code)
	}
	w := f.call(t, f.owner, f.repoID, `{"iids":[30,10]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	if got, want := boardCardIIDs(t, w), []int64{30, 10, 20}; !equalIIDs(got, want) {
		t.Errorf("board cards = %v, want %v — the omitted card must fall to the end, not keep its old rank", got, want)
	}
	if p := f.positions(ctx, t)[20]; p.Valid {
		t.Errorf("issue 20 board_position = %d, want NULL after being omitted", p.Int64)
	}
}

// A duplicate iid is a client bug and must not 400 somebody's drag. Left in, it would
// consume an ordinal and shift every card after it, so the dedupe is behaviour, not
// hygiene: the assertion is on the resulting POSITIONS, which a 200 alone would not
// catch.
func TestSetBoardOrderDedupesLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardOrderFixture(ctx, t, 10, 20)

	w := f.call(t, f.owner, f.repoID, `{"iids":[20,20,10]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a duplicate must not be an error)", w.Code)
	}
	pos := f.positions(ctx, t)
	if !pos[20].Valid || pos[20].Int64 != 1000 {
		t.Errorf("issue 20 board_position = %v, want 1000", pos[20])
	}
	// 2000, not 3000: the duplicate must not have consumed an ordinal.
	if !pos[10].Valid || pos[10].Int64 != 2000 {
		t.Errorf("issue 10 board_position = %v, want 2000 — the duplicate consumed an ordinal", pos[10])
	}
}

// The cap. 5000 is far above any real board; the test exists because an array-accepting
// endpoint must not be handable an unbounded one.
func TestSetBoardOrderRejectsOversizedListLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardOrderFixture(ctx, t, 10)

	iids := make([]int64, maxBoardOrderIids+1)
	for i := range iids {
		iids[i] = int64(i + 1)
	}
	body, err := json.Marshal(map[string]any{"iids": iids})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if w := f.call(t, f.owner, f.repoID, string(body)); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for %d iids", w.Code, len(iids))
	}
}

func equalIIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
