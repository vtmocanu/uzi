package handler

// Issue #2545: the held-publication delete guards on repo removal (DELETE /api/repos/{id}) and
// forge-connection removal (DELETE /api/forge/connections/{id}). A live held publication keeps
// live_run_id (ON DELETE RESTRICT) while its refs/uzi-held ref may exist on the forge, and the
// retention delete needs the connection's credential, so both routes refuse with a 409 naming the
// held refs; a row recorded between the handler's count and its delete maps to the same 409, never
// a 500. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type heldConflictBody struct {
	Error     string   `json:"error"`
	HeldRefs  []string `json:"held_refs"`
	HeldCount int64    `json:"held_count"`
	Salvage   *int64   `json:"salvage_count"`
}

func assertHeld409(t *testing.T, rec *httptest.ResponseRecorder, what string, run uuid.UUID) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("%s = %d, want 409\nbody: %s", what, rec.Code, rec.Body.String())
	}
	var body heldConflictBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode: %v\n%s", what, err, rec.Body.String())
	}
	ref := "refs/uzi-held/" + run.String() + "/1"
	if body.HeldCount != 1 || len(body.HeldRefs) != 1 || body.HeldRefs[0] != ref || !strings.Contains(body.Error, ref) || body.Salvage != nil {
		t.Fatalf("%s: body = %+v, want held_count 1 naming %s and no salvage fields", what, body, ref)
	}
}

// settleHeld ends the row's life the way the cleanup does, so the removal can proceed and the
// fixture's cleanup can delete the owner.
func settleHeld(t *testing.T, f *salvageRemoveFixture, run uuid.UUID) {
	t.Helper()
	cliMustExec(t, f.pool, `UPDATE run_held_publications SET state='deleted', live_run_id=NULL, deleted_at=now() WHERE run_id=$1`, run)
}

func newHeldRemoveFixture(t *testing.T) *salvageRemoveFixture {
	t.Helper()
	f := newSalvageRemoveFixture(t)
	// Registered after the salvage fixture's cleanup, so it runs first: a live pointer would
	// RESTRICT the users -> runs cascade that cleanup performs.
	t.Cleanup(func() {
		for _, stmt := range []string{
			`UPDATE run_held_publications SET state='invoked', create_invoked_at=now() WHERE user_id=$1 AND state='prepared'`,
			`UPDATE run_held_publications SET state='created' WHERE user_id=$1 AND state='invoked'`,
			`UPDATE run_held_publications SET state='abandoned', live_run_id=NULL WHERE user_id=$1 AND live_run_id IS NOT NULL`,
		} {
			cliMustExec(t, f.pool, stmt, f.owner)
		}
	})
	return f
}

// Acknowledged and delete_unknown rows make BOTH removal routes 409 naming the held ref, and the
// connection, repo, run and row all survive. Settling the row lifts both guards.
func TestDeleteWithLiveHeldPublicationIs409LiveDB(t *testing.T) {
	f := newHeldRemoveFixture(t)
	for i, state := range []string{"acknowledged", "delete_unknown"} {
		t.Run(state, func(t *testing.T) {
			conn, repo, run := f.connRepoRun(int64(2545_00 + i))
			heldSeedOnRun(t, f.pool, f.owner, repo, run, true)
			if state != "acknowledged" {
				cliMustExec(t, f.pool, `UPDATE run_held_publications SET state=$2 WHERE run_id=$1`, run, state)
			}

			assertHeld409(t, cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""), "DELETE repo", run)
			assertHeld409(t, cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, ""), "DELETE connection", run)
			for _, c := range []struct {
				table string
				id    uuid.UUID
			}{{"forge_connections", conn}, {"repos", repo}, {"runs", run}, {"run_held_publications", run}} {
				col := "id"
				if c.table == "run_held_publications" {
					col = "run_id"
				}
				var n int
				if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+c.table+` WHERE `+col+` = $1`, c.id).Scan(&n); err != nil || n != 1 {
					t.Fatalf("the %s row must survive a held 409 (n=%d err=%v)", c.table, n, err)
				}
			}

			settleHeld(t, f, run)
			if rec := cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE repo after settle = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
			}
			if rec := cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE connection after settle = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A foreign owner's connection with a live held row 404s and counts nothing of another user's refs.
func TestDeleteForeignConnectionWithHeldIs404LiveDB(t *testing.T) {
	f := newHeldRemoveFixture(t)
	conn, repo, run := f.connRepoRun(2545_10)
	heldSeedOnRun(t, f.pool, f.owner, repo, run, true)
	stranger := cliSeedUser(t, f.pool, false)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, stranger) })
	rec := cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), cliMintJWT(t, f.pool, stranger), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stranger DELETE connection = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
}

// The insert-after-count interleaving: a transaction records the held row after the handler counted
// (uncommitted, so the count sees nothing) and commits while the handler's cascade is blocked on the
// run row the insert's FK check locked. The cascade then hits the RESTRICT (23503 on
// run_held_publications_live_run_id_fkey), which must map to the held 409, not a 500.
func TestDeleteRacingHeldInsertIs409LiveDB(t *testing.T) {
	f := newHeldRemoveFixture(t)
	for i, route := range []string{"repo", "connection"} {
		t.Run(route, func(t *testing.T) {
			conn, repo, run := f.connRepoRun(int64(2545_20 + i))
			ctx := context.Background()
			// An open hold of the failed run, released so the custody guard passes; the publication
			// is inserted by the racing transaction below.
			worker := custodySeedWorker(t, f.pool, f.owner)
			hold := uuid.New()
			cliMustExec(t, f.pool, `UPDATE runs SET worker_id=$2, fail_origin='agent_failure', claim_generation=1 WHERE id=$1`, run, worker)
			cliMustExec(t, f.pool, `INSERT INTO recovery_custody_holds
				(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
				VALUES ($1,$2,$3,$4,1,'open',$5,'held-ident',$5,$4,true)`, hold, f.owner, repo, run, worker)
			cliMustExec(t, f.pool, `ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard`)
			cliMustExec(t, f.pool, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
				release_evidence='publication',released_at=now() WHERE id=$1`, hold)
			cliMustExec(t, f.pool, `ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard`)

			path, anchor, table := "/api/repos/"+repo.String(), repo, "repos"
			if route == "connection" {
				path, anchor, table = "/api/forge/connections/"+conn.String(), conn, "forge_connections"
			}
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var pid int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			if _, err := store.New(tx).UpsertHeldPublication(ctx, store.UpsertHeldPublicationParams{
				RunID: run, Generation: 1, HoldID: hold, UserID: f.owner, RepoID: repo, WorkerID: worker,
				Ref: "refs/uzi-held/" + run.String() + "/1", Tip: heldHTTPTip, CoverageDigest: heldHTTPCov,
			}); err != nil {
				t.Fatalf("racing insert: %v", err)
			}

			req := salvageCookieRequest(t, http.MethodDelete, path, f.jwt)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				f.router.ServeHTTP(rec, req)
				done <- rec
			}()
			deadline := time.Now().Add(20 * time.Second)
			for {
				var blocked int
				if err := f.pool.QueryRow(ctx,
					`SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked > 0 {
					break
				}
				select {
				case rec := <-done:
					t.Fatalf("the handler finished before blocking on the insert: %d %s", rec.Code, rec.Body.String())
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the handler's delete never blocked on the racing transaction")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the handler never returned after the insert committed")
			}
			assertHeld409(t, rec, "racing DELETE "+route, run)
			var n int
			if err := f.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE id=$1`, table), anchor).Scan(&n); err != nil || n != 1 {
				t.Fatalf("the %s must survive the raced 409 (n=%d err=%v)", route, n, err)
			}
		})
	}
}

// Only a 23503 on run_held_publications' live pointer maps to the held 409.
func TestIsHeldRestrict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"held fk", &pgconn.PgError{Code: "23503", ConstraintName: "run_held_publications_live_run_id_fkey"}, true},
		{"wrapped held fk", fmt.Errorf("delete: %w", &pgconn.PgError{Code: "23503", ConstraintName: "run_held_publications_live_run_id_fkey"}), true},
		{"salvage fk", &pgconn.PgError{Code: "23503", ConstraintName: "run_salvage_live_run_id_fkey"}, false},
		{"custody fk", &pgconn.PgError{Code: "23503", ConstraintName: "recovery_custody_holds_live_run_id_fkey"}, false},
		{"other code same constraint", &pgconn.PgError{Code: "23514", ConstraintName: "run_held_publications_live_run_id_fkey"}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isHeldRestrict(tc.err); got != tc.want {
			t.Errorf("%s: isHeldRestrict = %v, want %v", tc.name, got, tc.want)
		}
	}
	if isSalvageRestrict(&pgconn.PgError{Code: "23503", ConstraintName: heldRestrictConstraint}) {
		t.Error("a held restriction must not map to the salvage 409")
	}
}

func TestWriteHeldConflict(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rec := httptest.NewRecorder()
	writeHeldConflict(rec, "repo", 3, []heldConflictRow{
		{RunID: a, State: "acknowledged", Ref: "refs/uzi-held/" + a.String() + "/1"},
		{RunID: b, State: "delete_unknown", Ref: "refs/uzi-held/" + b.String() + "/2"},
	})
	var body heldConflictBody
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if body.HeldCount != 3 || len(body.HeldRefs) != 2 || !strings.Contains(body.Error, "and 1 more") ||
		!strings.Contains(body.Error, "(delete_unknown)") {
		t.Fatalf("body = %+v", body)
	}
	// The expire advice is scoped to acknowledged refs; other states settle against the forge first.
	if !strings.Contains(body.Error, "acknowledged refs clear at their retention expiry") ||
		!strings.Contains(body.Error, "any other state") || !strings.Contains(body.Error, "settle against the forge first") || !strings.Contains(body.Error, "then acknowledged follows the retention rule") ||
		strings.Contains(body.Error, "the block lifts when each ref is deleted") {
		t.Fatalf("error text does not scope expire advice to acknowledged refs: %q", body.Error)
	}
	// A raced guard with nothing named still refuses, with an empty (non-null) list and a floor of 1.
	rec = httptest.NewRecorder()
	writeHeldConflict(rec, "connection", 0, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"held_refs":[]`) || !strings.Contains(rec.Body.String(), `"held_count":1`) {
		t.Fatalf("empty conflict = %d %s", rec.Code, rec.Body.String())
	}
}
