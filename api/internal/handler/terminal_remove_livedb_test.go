package handler

// Issue #4007 slice A, M1: the terminal-preservation delete guards on repo removal
// (DELETE /api/repos/{id}) and forge-connection removal (DELETE /api/forge/connections/{id}). A
// pointer-bearing preservation (publishing, live, retiring) keeps live_run_id (ON DELETE RESTRICT)
// while its refs/uzi-terminal ref may exist on the forge, so both routes refuse with a 409 naming
// the ref; a bound row has no pointer and does not block; a row recorded between the handler's
// count and its delete maps to the same 409, never a 500; the held 409 is unchanged. Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

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

type terminalConflictBody struct {
	Error         string    `json:"error"`
	TerminalRefs  []string  `json:"terminal_refs"`
	TerminalCount int64     `json:"terminal_count"`
	HeldRefs      *[]string `json:"held_refs"`
	Salvage       *int64    `json:"salvage_count"`
}

func terminalRef(run uuid.UUID) string { return "refs/uzi-terminal/" + run.String() }

func assertTerminal409(t *testing.T, rec *httptest.ResponseRecorder, what string, run uuid.UUID) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("%s = %d, want 409\nbody: %s", what, rec.Code, rec.Body.String())
	}
	var body terminalConflictBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode: %v\n%s", what, err, rec.Body.String())
	}
	ref := terminalRef(run)
	if body.TerminalCount != 1 || len(body.TerminalRefs) != 1 || body.TerminalRefs[0] != ref ||
		!strings.Contains(body.Error, ref) || body.HeldRefs != nil || body.Salvage != nil {
		t.Fatalf("%s: body = %+v, want terminal_count 1 naming %s and no held or salvage fields", what, body, ref)
	}
}

// seedTerminal records a terminal preservation of the run in the given state. A state with a
// pointer is reached by the legal moves; the row carries no contribution, which the guards under
// test do not read.
func seedTerminal(t *testing.T, f *salvageRemoveFixture, repo, run uuid.UUID, state string) {
	t.Helper()
	cliMustExec(t, f.pool, `INSERT INTO run_terminal_preservations (run_id,user_id,repo_id,ref,lifetime_at_bind)
		VALUES($1,$2,$3,'refs/uzi-terminal/'||$1::uuid::text,'no_pr')`, run, f.owner, repo)
	if state == "bound" {
		return
	}
	cliMustExec(t, f.pool, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE run_id=$1`, run)
	if state == "publishing" {
		return
	}
	terminalGoLive(t, f, `run_id=$1 AND state='publishing'`, run)
	if state == "live" {
		return
	}
	cliMustExec(t, f.pool, `UPDATE run_terminal_preservations SET state='retiring', retire_tip=repeat('a',40),
		retire_reason='issue_closed', retire_observed_at=now(), retire_decided_at=now() WHERE run_id=$1`, run)
}

// terminalGoLive moves the matching publishing rows to live. The CHECK requires a live row to carry
// its tip, and the guard only lets the tip move to the tip of a published contribution; these
// fixtures record no contribution (the guards under test do not read one), so the guard trigger is
// off for this one statement. The CHECKs stay on.
func terminalGoLive(t *testing.T, f *salvageRemoveFixture, where string, arg any) {
	t.Helper()
	cliMustExec(t, f.pool, `ALTER TABLE run_terminal_preservations DISABLE TRIGGER run_terminal_preservation_guard`)
	defer cliMustExec(t, f.pool, `ALTER TABLE run_terminal_preservations ENABLE TRIGGER run_terminal_preservation_guard`)
	cliMustExec(t, f.pool, `UPDATE run_terminal_preservations SET state='live', tip=repeat('a',40) WHERE `+where, arg)
}

// settleTerminal ends the row's life the way a completed retirement does, so the removal can
// proceed and the fixture's cleanup can delete the owner.
func settleTerminal(t *testing.T, f *salvageRemoveFixture, run uuid.UUID) {
	t.Helper()
	cliMustExec(t, f.pool, `UPDATE run_terminal_preservations SET state='deleted', live_run_id=NULL, deleted_at=now() WHERE run_id=$1 AND state='retiring'`, run)
}

func newTerminalRemoveFixture(t *testing.T) *salvageRemoveFixture {
	t.Helper()
	f := newHeldRemoveFixture(t)
	// Registered after the held fixture's cleanup, so it runs first: a live pointer would RESTRICT
	// the users -> runs cascade that cleanup performs. The walk follows the transition matrix.
	t.Cleanup(func() {
		for _, stmt := range []string{
			// A publishing row never recorded a tip, so it ends as not published (no live row without one).
			`UPDATE run_terminal_preservations SET state='not_published', live_run_id=NULL WHERE user_id=$1 AND state='publishing'`,
			`UPDATE run_terminal_preservations SET state='retiring', retire_tip=repeat('a',40), retire_reason='issue_closed',
				retire_observed_at=now(), retire_decided_at=now() WHERE user_id=$1 AND state='live'`,
			`UPDATE run_terminal_preservations SET state='abandoned', live_run_id=NULL WHERE user_id=$1 AND state='retiring'`,
			`DELETE FROM run_terminal_preservations WHERE user_id=$1`,
		} {
			cliMustExec(t, f.pool, stmt, f.owner)
		}
	})
	return f
}

func rowSurvives(t *testing.T, f *salvageRemoveFixture, table, col string, id uuid.UUID, what string) {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the %s row must survive %s (n=%d err=%v)", table, what, n, err)
	}
}

// Every pointer-bearing state makes BOTH removal routes 409 naming the terminal ref, and the
// connection, repo, run and row all survive. Retiring to deleted lifts both guards.
func TestDeleteWithLiveTerminalPreservationIs409LiveDB(t *testing.T) {
	f := newTerminalRemoveFixture(t)
	for i, state := range []string{"publishing", "live", "retiring"} {
		t.Run(state, func(t *testing.T) {
			conn, repo, run := f.connRepoRun(int64(4007_00 + i))
			seedTerminal(t, f, repo, run, state)

			assertTerminal409(t, cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""), "DELETE repo", run)
			assertTerminal409(t, cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, ""), "DELETE connection", run)
			rowSurvives(t, f, "forge_connections", "id", conn, "a terminal 409")
			rowSurvives(t, f, "repos", "id", repo, "a terminal 409")
			rowSurvives(t, f, "runs", "id", run, "a terminal 409")
			rowSurvives(t, f, "run_terminal_preservations", "run_id", run, "a terminal 409")

			if state != "retiring" {
				terminalGoLive(t, f, `run_id=$1 AND state='publishing'`, run)
				cliMustExec(t, f.pool, `UPDATE run_terminal_preservations SET state='retiring', retire_tip=repeat('a',40), retire_reason='issue_closed',
					retire_observed_at=now(), retire_decided_at=now() WHERE run_id=$1 AND state='live'`, run)
			}
			settleTerminal(t, f, run)
			if rec := cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE repo after settle = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
			}
			if rec := cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE connection after settle = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A bound preservation has no pointer and no ref, so it blocks neither route.
func TestDeleteWithBoundTerminalPreservationIsAllowedLiveDB(t *testing.T) {
	f := newTerminalRemoveFixture(t)
	for i, route := range []string{"repo", "connection"} {
		t.Run(route, func(t *testing.T) {
			conn, repo, run := f.connRepoRun(int64(4007_10 + i))
			seedTerminal(t, f, repo, run, "bound")
			path := "/api/repos/" + repo.String()
			if route == "connection" {
				path = "/api/forge/connections/" + conn.String()
			}
			if rec := cookieReq(t, f.router, http.MethodDelete, path, f.jwt, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("DELETE %s with a bound preservation = %d, want 204\nbody: %s", route, rec.Code, rec.Body.String())
			}
		})
	}
}

// A foreign owner's connection with a pointer-bearing row 404s and counts nothing of another
// user's refs.
func TestDeleteForeignConnectionWithTerminalIs404LiveDB(t *testing.T) {
	f := newTerminalRemoveFixture(t)
	conn, repo, run := f.connRepoRun(4007_20)
	seedTerminal(t, f, repo, run, "live")
	stranger := cliSeedUser(t, f.pool, false)
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, stranger) })
	rec := cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), cliMintJWT(t, f.pool, stranger), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stranger DELETE connection = %d, want 404\nbody: %s", rec.Code, rec.Body.String())
	}
}

// The held 409 is unchanged by the terminal guard: a repo with a held row answers with the held
// body and no terminal fields, and a repo with both lifts the held block first, then the terminal.
func TestHeldConflictUnchangedByTerminalGuardLiveDB(t *testing.T) {
	f := newTerminalRemoveFixture(t)
	conn, repo, run := f.connRepoRun(4007_30)
	heldSeedOnRun(t, f.pool, f.owner, repo, run, true)

	for what, path := range map[string]string{
		"DELETE repo":       "/api/repos/" + repo.String(),
		"DELETE connection": "/api/forge/connections/" + conn.String(),
	} {
		rec := cookieReq(t, f.router, http.MethodDelete, path, f.jwt, "")
		assertHeld409(t, rec, what, run)
		if strings.Contains(rec.Body.String(), "terminal") {
			t.Fatalf("%s: the held 409 must not mention terminal preservations: %s", what, rec.Body.String())
		}
	}

	seedTerminal(t, f, repo, run, "live")
	assertHeld409(t, cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""), "DELETE repo with both", run)
	settleHeld(t, f, run)
	assertTerminal409(t, cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""), "DELETE repo after held settled", run)
	assertTerminal409(t, cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, ""), "DELETE connection after held settled", run)
}

// The insert-after-count interleaving: a transaction records the preservation after the handler
// counted (uncommitted, so the count sees nothing) and commits while the handler's cascade is
// blocked on the run row the pointer's FK check locked. The cascade then hits the RESTRICT (23503
// on run_terminal_preservations_live_run_id_fkey), which must map to the terminal 409, not a 500.
func TestDeleteRacingTerminalInsertIs409LiveDB(t *testing.T) {
	f := newTerminalRemoveFixture(t)
	for i, route := range []string{"repo", "connection"} {
		t.Run(route, func(t *testing.T) {
			conn, repo, run := f.connRepoRun(int64(4007_40 + i))
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
			var pid int32
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			q := store.New(tx)
			p, err := q.InsertTerminalPreservation(ctx, store.InsertTerminalPreservationParams{
				RunID: run, UserID: f.owner, RepoID: repo, Ref: terminalRef(run), LifetimeAtBind: "no_pr",
			})
			if err != nil {
				t.Fatalf("racing insert: %v", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE id=$1`, p.ID); err != nil {
				t.Fatalf("racing publish: %v", err)
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
			assertTerminal409(t, rec, "racing DELETE "+route, run)
			var n int
			if err := f.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE id=$1`, table), anchor).Scan(&n); err != nil || n != 1 {
				t.Fatalf("the %s must survive the raced 409 (n=%d err=%v)", route, n, err)
			}
		})
	}
}

// Only a 23503 on run_terminal_preservations' live pointer maps to the terminal 409, and no other
// restriction maps to it.
func TestIsTerminalRestrict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"terminal fk", &pgconn.PgError{Code: "23503", ConstraintName: "run_terminal_preservations_live_run_id_fkey"}, true},
		{"wrapped terminal fk", fmt.Errorf("delete: %w", &pgconn.PgError{Code: "23503", ConstraintName: "run_terminal_preservations_live_run_id_fkey"}), true},
		{"held fk", &pgconn.PgError{Code: "23503", ConstraintName: heldRestrictConstraint}, false},
		{"salvage fk", &pgconn.PgError{Code: "23503", ConstraintName: salvageRestrictConstraint}, false},
		{"prerequisite fk", &pgconn.PgError{Code: "23503", ConstraintName: prerequisiteRestrictConstraint}, false},
		{"custody fk", &pgconn.PgError{Code: "23503", ConstraintName: "recovery_custody_holds_live_run_id_fkey"}, false},
		{"other code same constraint", &pgconn.PgError{Code: "23514", ConstraintName: terminalRestrictConstraint}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isTerminalRestrict(tc.err); got != tc.want {
			t.Errorf("%s: isTerminalRestrict = %v, want %v", tc.name, got, tc.want)
		}
	}
	terminal := &pgconn.PgError{Code: "23503", ConstraintName: terminalRestrictConstraint}
	if isHeldRestrict(terminal) || isSalvageRestrict(terminal) || isPrerequisiteRestrict(terminal) {
		t.Error("a terminal restriction must not map to the held, salvage or prerequisite 409")
	}
}

func TestWriteTerminalConflict(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	rec := httptest.NewRecorder()
	writeTerminalConflict(rec, "repo", 3, []terminalConflictRow{
		{RunID: a, State: "live", Ref: terminalRef(a)},
		{RunID: b, State: "retiring", Ref: terminalRef(b)},
	})
	var body terminalConflictBody
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
	}
	if body.TerminalCount != 3 || len(body.TerminalRefs) != 2 || !strings.Contains(body.Error, "and 1 more") ||
		!strings.Contains(body.Error, terminalRef(b)+" (retiring)") {
		t.Fatalf("body = %+v", body)
	}
	// A raced guard with nothing named still refuses, with an empty (non-null) list and a floor of 1.
	rec = httptest.NewRecorder()
	writeTerminalConflict(rec, "connection", 0, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"terminal_refs":[]`) || !strings.Contains(rec.Body.String(), `"terminal_count":1`) {
		t.Fatalf("empty conflict = %d %s", rec.Code, rec.Body.String())
	}
}
