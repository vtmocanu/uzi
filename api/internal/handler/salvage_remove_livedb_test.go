package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1867 M2: the salvage delete guards on repo removal (DELETE /api/repos/{id}) and
// forge-connection removal (DELETE /api/forge/connections/{id}). A failed run's salvage
// row holds live_run_id (ON DELETE RESTRICT) while a checkpoint or salvage ref may still
// exist on the forge; both routes cascade onto the run, so both must refuse with a 409
// naming the refs, and a salvage row recorded between the handler's count and its delete
// must map to the same 409, never a 500.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

const salvageTestTip = "89abcdef0123456789abcdef0123456789abcdef"

type salvageRemoveFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	router http.Handler
	owner  uuid.UUID
	jwt    string
	runs   []uuid.UUID
}

func newSalvageRemoveFixture(t *testing.T) *salvageRemoveFixture {
	t.Helper()
	_, router, pool := cliLiveDB(t)
	f := &salvageRemoveFixture{t: t, pool: pool, router: router, owner: cliSeedUser(t, pool, false)}
	f.jwt = cliMintJWT(t, pool, f.owner)
	// Salvage rows are removed BEFORE the owner: a live pointer would RESTRICT the
	// users -> runs cascade and break the cleanup.
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM run_salvage WHERE run_id = ANY($1) OR user_id = $2`, f.runs, f.owner); err != nil {
			t.Errorf("cleanup run_salvage: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, f.owner); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})
	return f
}

// connRepoRun seeds a connection, a DISABLED repo and a FAILED run on it (so the enabled
// and active-run guards pass and any 409 is the salvage guard).
func (f *salvageRemoveFixture) connRepoRun(projectID int64) (conn, repo, run uuid.UUID) {
	f.t.Helper()
	conn = uuid.New()
	cliMustExec(f.t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, conn, f.owner, "https://"+conn.String()+".e2e", []byte{0x1})
	repo = rmSeedRepo(f.t, f.pool, conn, projectID, false)
	run = rmSeedRun(f.t, f.pool, f.owner, repo, "failed")
	f.runs = append(f.runs, run)
	return conn, repo, run
}

// salvage inserts a run_salvage row directly in state, with the live pointer set for the
// live states and salvage_created_at (with its expires_at) set when created.
func (f *salvageRemoveFixture) salvage(run, repo uuid.UUID, state string, created bool) {
	f.t.Helper()
	var live any
	if state == "pending" || state == "promoted" {
		live = run
	}
	var createdAt, expiresAt any
	if created {
		now := time.Now()
		createdAt, expiresAt = now, now.Add(time.Hour)
	}
	cliMustExec(f.t, f.pool,
		`INSERT INTO run_salvage (run_id, user_id, repo_id, forge_type, branch, tip, live_run_id, state, salvage_created_at, expires_at)
		 VALUES ($1, $2, $3, 'gitlab', 'agent/issue-7', $4, $5, $6, $7, $8)`,
		run, f.owner, repo, salvageTestTip, live, state, createdAt, expiresAt)
}

func (f *salvageRemoveFixture) exists(table string, id uuid.UUID) bool {
	f.t.Helper()
	col := "id"
	if table == "run_salvage" {
		col = "run_id"
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, id).Scan(&n); err != nil {
		f.t.Fatalf("count %s: %v", table, err)
	}
	return n > 0
}

func (f *salvageRemoveFixture) settle(run uuid.UUID) {
	f.t.Helper()
	cliMustExec(f.t, f.pool, `UPDATE run_salvage SET state = 'expired', live_run_id = NULL WHERE run_id = $1`, run)
}

type salvageConflictBody struct {
	Error        string   `json:"error"`
	SalvageRefs  []string `json:"salvage_refs"`
	PendingRuns  []string `json:"salvage_pending_runs"`
	SalvageCount int64    `json:"salvage_count"`
}

// assertSalvage409 checks a 409 carrying the salvage body for one run naming exactly
// wantRefs (its created salvage ref) or, with none, the run whose salvage copy is still
// being made. The branch-scoped checkpoint ref (#1810's) is never named.
func assertSalvage409(t *testing.T, rec *httptest.ResponseRecorder, what string, run uuid.UUID, wantRefs ...string) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("%s = %d, want 409\nbody: %s", what, rec.Code, rec.Body.String())
	}
	var body salvageConflictBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode 409 body: %v\n%s", what, err, rec.Body.String())
	}
	wantPending := []string{}
	if len(wantRefs) == 0 {
		wantPending = []string{run.String()}
		if !strings.Contains(body.Error, "a salvage copy is being made for run "+run.String()) {
			t.Fatalf("%s: error text %q must say a salvage copy is being made for run %s", what, body.Error, run)
		}
	}
	if body.SalvageCount != 1 || strings.Join(body.SalvageRefs, "\n") != strings.Join(wantRefs, "\n") ||
		strings.Join(body.PendingRuns, "\n") != strings.Join(wantPending, "\n") {
		t.Fatalf("%s: salvage body = %+v, want count 1 naming refs %q, pending runs %q", what, body, wantRefs, wantPending)
	}
	for _, ref := range wantRefs {
		if !strings.Contains(body.Error, ref) {
			t.Fatalf("%s: error text %q must name %q", what, body.Error, ref)
		}
	}
	if !strings.Contains(body.Error, "checkpointed commits") || strings.Contains(body.Error, "more run") {
		t.Fatalf("%s: error text %q must name the checkpointed commits and no further runs", what, body.Error)
	}
	if strings.Contains(rec.Body.String(), "uzi-checkpoints") {
		t.Fatalf("%s: the 409 must never name a branch checkpoint ref: %s", what, rec.Body.String())
	}
}

// Pending, promoted and created-pending salvage rows make BOTH removal routes 409 (naming
// the salvage ref once created, else the run), and the repo, connection, run and salvage
// row all survive. Settling the row lifts both guards.
func TestDeleteWithLiveSalvageIs409LiveDB(t *testing.T) {
	f := newSalvageRemoveFixture(t)
	cases := []struct {
		name    string
		state   string
		created bool
		refs    func(run uuid.UUID) []string
	}{
		// Pending, nothing created yet: the run is named, never its branch checkpoint ref.
		{"pending", "pending", false, func(uuid.UUID) []string { return nil }},
		// A created salvage ref on a still-pending row names only that salvage ref.
		{"created-pending", "pending", true, func(run uuid.UUID) []string { return []string{"refs/uzi-salvage/" + run.String()} }},
		{"promoted", "promoted", true, func(run uuid.UUID) []string { return []string{"refs/uzi-salvage/" + run.String()} }},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, repo, run := f.connRepoRun(int64(1867_00 + i))
			f.salvage(run, repo, tc.state, tc.created)

			rec := cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, "")
			assertSalvage409(t, rec, "DELETE repo", run, tc.refs(run)...)
			rec = cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, "")
			assertSalvage409(t, rec, "DELETE connection", run, tc.refs(run)...)

			for _, c := range []struct {
				table string
				id    uuid.UUID
			}{{"forge_connections", conn}, {"repos", repo}, {"runs", run}, {"run_salvage", run}} {
				if !f.exists(c.table, c.id) {
					t.Fatalf("the %s row must survive a salvage 409", c.table)
				}
			}

			// Settled (the ref was CAS-deleted): both guards lift. Repo first, then connection.
			f.settle(run)
			if rec := cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE repo after settle = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
			}
			if rec := cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE connection after settle = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
			}
			if !f.exists("run_salvage", run) {
				t.Fatalf("the settled salvage row must survive the cascade as provenance")
			}
		})
	}
}

// Settled rows (expired, disabled, unavailable, failed, skipped_secret) hold no live pointer and
// block neither route; the rows survive the cascade as provenance.
func TestDeleteWithSettledSalvageSucceedsLiveDB(t *testing.T) {
	f := newSalvageRemoveFixture(t)
	// expired and disabled are seeded with a created (since removed) salvage ref.
	for i, state := range []string{"expired", "disabled", "unavailable", "failed", "skipped_secret"} {
		created := state == "expired" || state == "disabled"
		t.Run(state+"/repo", func(t *testing.T) {
			_, repo, run := f.connRepoRun(int64(1867_10 + i))
			f.salvage(run, repo, state, created)
			if rec := cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE repo with a %s salvage row = %d, want 204\nbody: %s", state, rec.Code, rec.Body.String())
			}
			if f.exists("repos", repo) || f.exists("runs", run) || !f.exists("run_salvage", run) {
				t.Fatalf("repo and run must be gone and the %s salvage row kept", state)
			}
		})
		t.Run(state+"/connection", func(t *testing.T) {
			conn, repo, run := f.connRepoRun(int64(1867_20 + i))
			f.salvage(run, repo, state, created)
			if rec := cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE connection with a %s salvage row = %d, want 204\nbody: %s", state, rec.Code, rec.Body.String())
			}
			if f.exists("forge_connections", conn) || f.exists("runs", run) || !f.exists("run_salvage", run) {
				t.Fatalf("connection and run must be gone and the %s salvage row kept", state)
			}
		})
	}
}

// A foreign owner's connection with a live salvage row 404s (the owner-scoped count names
// nothing of another user's refs), and the connection survives.
func TestDeleteForeignConnectionWithSalvageIs404LiveDB(t *testing.T) {
	f := newSalvageRemoveFixture(t)
	conn, repo, run := f.connRepoRun(1867_30)
	f.salvage(run, repo, "pending", false)
	stranger := cliSeedUser(t, f.pool, false)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, stranger) })
	jwt := cliMintJWT(t, f.pool, stranger)
	if rec := cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), jwt, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger DELETE connection = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
	if !f.exists("forge_connections", conn) {
		t.Fatalf("a foreign connection must survive")
	}
}

// The insert-after-count interleaving: a sweep transaction records the salvage row after
// the handler counted (its insert is uncommitted, so the count sees nothing) and commits
// while the handler's cascade is blocked on the run row the insert's FK check locked. The
// cascade then hits the RESTRICT (23503 on run_salvage_live_run_id_fkey), which must map
// to the salvage 409, not a 500, and the committed row survives with the repo/connection.
func TestDeleteRacingSalvageInsertIs409LiveDB(t *testing.T) {
	f := newSalvageRemoveFixture(t)
	for i, route := range []string{"repo", "connection"} {
		t.Run(route, func(t *testing.T) {
			conn, repo, run := f.connRepoRun(int64(1867_40 + i))
			ctx := context.Background()
			path, anchor, table := "/api/repos/"+repo.String(), repo, "repos"
			if route == "connection" {
				path, anchor, table = "/api/forge/connections/"+conn.String(), conn, "forge_connections"
			}

			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var sweepPID int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&sweepPID); err != nil {
				t.Fatalf("backend pid: %v", err)
			}
			n, err := store.New(tx).InsertRunSalvage(ctx, store.InsertRunSalvageParams{
				RunID: run, UserID: f.owner, RepoID: repo, ForgeType: "gitlab", Branch: "agent/issue-7",
				Tip: salvageTestTip, LiveRunID: pgtype.UUID{Bytes: run, Valid: true}, State: "pending",
			})
			if err != nil || n != 1 {
				t.Fatalf("sweep InsertRunSalvage = (%d, %v), want (1, nil)", n, err)
			}

			req := salvageCookieRequest(t, http.MethodDelete, path, f.jwt)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				f.router.ServeHTTP(rec, req)
				done <- rec
			}()

			// Wait until the handler's delete is blocked behind the sweep transaction:
			// its count already ran and saw nothing.
			deadline := time.Now().Add(20 * time.Second)
			for {
				var blocked int
				if err := f.pool.QueryRow(ctx,
					`SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, sweepPID).Scan(&blocked); err != nil {
					t.Fatalf("blocked probe: %v", err)
				}
				if blocked > 0 {
					break
				}
				select {
				case rec := <-done:
					t.Fatalf("the handler finished before blocking on the sweep insert: %d %s", rec.Code, rec.Body.String())
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("the handler's delete never blocked on the sweep transaction")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit sweep insert: %v", err)
			}

			var rec *httptest.ResponseRecorder
			select {
			case rec = <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("the handler never returned after the sweep committed")
			}
			assertSalvage409(t, rec, "racing DELETE "+route, run)
			if !f.exists(table, anchor) || !f.exists("runs", run) || !f.exists("run_salvage", run) {
				t.Fatalf("the %s, run and committed salvage row must all survive the raced 409", route)
			}
		})
	}
}

// Delete first: once the repo (and its run) is gone, the sweep's insert for that run fails
// with 23503 on the live pointer and leaves no row.
func TestSalvageInsertAfterRepoDeleteFailsLiveDB(t *testing.T) {
	f := newSalvageRemoveFixture(t)
	_, repo, run := f.connRepoRun(1867_50)
	if rec := cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE repo = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
	q := store.New(f.pool)
	_, err := q.InsertRunSalvage(context.Background(), store.InsertRunSalvageParams{
		RunID: run, UserID: f.owner, RepoID: repo, ForgeType: "gitlab", Branch: "agent/issue-7",
		Tip: salvageTestTip, LiveRunID: pgtype.UUID{Bytes: run, Valid: true}, State: "pending",
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != salvageRestrictConstraint {
		t.Fatalf("InsertRunSalvage after the delete = %v, want 23503 on %s", err, salvageRestrictConstraint)
	}
	if _, err := q.GetRunSalvage(context.Background(), run); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the refused insert must leave no row, GetRunSalvage err = %v", err)
	}
}

// PRD #1867 M4: GET /api/runs/{id} overlays the salvage fields on a failed run with a
// salvage row (salvage_ref only when promoted), leaves them null without a row, and never
// overlays a run that is not failed. The lookup-error case is TestApplyRunSalvage's.
func TestGetRunSalvageOverlayLiveDB(t *testing.T) {
	f := newSalvageRemoveFixture(t)
	type dto struct {
		Status       string     `json:"status"`
		LandingState string     `json:"landing_state"`
		State        *string    `json:"salvage_state"`
		Ref          *string    `json:"salvage_ref"`
		Tip          *string    `json:"salvage_tip"`
		ExpiresAt    *time.Time `json:"salvage_expires_at"`
		LastError    *string    `json:"salvage_last_error"`
	}
	get := func(run uuid.UUID) (dto, string) {
		t.Helper()
		rec := cookieReq(t, f.router, http.MethodGet, "/api/runs/"+run.String(), f.jwt, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET run = %d\nbody: %s", rec.Code, rec.Body.String())
		}
		var env struct {
			Run dto `json:"run"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode run: %v", err)
		}
		return env.Run, rec.Body.String()
	}

	_, repo, promoted := f.connRepoRun(1867_60)
	f.salvage(promoted, repo, "promoted", true)
	d, raw := get(promoted)
	if d.State == nil || *d.State != "promoted" || d.Ref == nil || *d.Ref != "refs/uzi-salvage/"+promoted.String() ||
		d.Tip == nil || *d.Tip != salvageTestTip || d.ExpiresAt == nil || d.LastError != nil || d.LandingState != "none" {
		t.Fatalf("promoted overlay = %s", raw)
	}

	_, repo, pending := f.connRepoRun(1867_61)
	f.salvage(pending, repo, "pending", false)
	cliMustExec(t, f.pool, `UPDATE run_salvage SET last_error = 'forge unreachable', attempts = 1 WHERE run_id = $1`, pending)
	d, raw = get(pending)
	if d.State == nil || *d.State != "pending" || d.Ref != nil || d.Tip == nil || d.ExpiresAt != nil ||
		d.LastError == nil || *d.LastError != "forge unreachable" {
		t.Fatalf("pending overlay = %s", raw)
	}
	if strings.Contains(raw, "uzi-checkpoints") {
		t.Fatalf("the run DTO must never surface the branch checkpoint ref as salvage's: %s", raw)
	}

	_, _, bare := f.connRepoRun(1867_62)
	d, raw = get(bare)
	if d.State != nil || d.Ref != nil || d.Tip != nil || d.ExpiresAt != nil || d.LastError != nil ||
		!strings.Contains(raw, `"salvage_state":null`) {
		t.Fatalf("no-row overlay must leave the salvage fields null: %s", raw)
	}

	// A run that is not failed is never overlaid, even with a row.
	_, repo, done := f.connRepoRun(1867_63)
	f.salvage(done, repo, "promoted", true)
	cliMustExec(t, f.pool, `UPDATE runs SET status = 'completed' WHERE id = $1`, done)
	d, raw = get(done)
	if d.Status != "completed" || d.State != nil || d.Ref != nil {
		t.Fatalf("a completed run must carry no salvage fields: %s", raw)
	}
}

// salvageCookieRequest builds a cookie+CSRF request (cookieReq's shape) on the test
// goroutine, so the racing test can serve it from another goroutine without t.Fatal there.
func salvageCookieRequest(t *testing.T, method, path, jwt string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
	return req
}
