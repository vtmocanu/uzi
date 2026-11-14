package handler

// Issue #4007 slice A, M3: after the route's tx1 (the preservation publishing with its live pointer,
// the contribution invoked), repo and forge-connection removal answer the terminal 409 even though
// nothing was pushed yet; once the contribution is decided and the preservation released, removal
// proceeds. The state is built with the route's own writer queries in one transaction, the way tx1
// writes it. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRemovalAfterTerminalTx1Is409LiveDB(t *testing.T) {
	f := newTerminalRemoveFixture(t)
	ctx := context.Background()
	// Registered after the fixture's cleanup, so it runs first: the decided contribution RESTRICTs
	// its preservation, which that cleanup deletes.
	t.Cleanup(func() { cliMustExec(t, f.pool, `DELETE FROM run_terminal_contributions WHERE user_id=$1`, f.owner) })
	conn, repo, run := f.connRepoRun(4007_40)
	q := store.New(f.pool)

	pres, err := q.InsertTerminalPreservation(ctx, store.InsertTerminalPreservationParams{
		RunID: run, UserID: f.owner, RepoID: repo, Ref: terminalRef(run), LifetimeAtBind: "no_pr",
	})
	if err != nil {
		t.Fatal(err)
	}
	con, err := q.InsertTerminalContribution(ctx, store.InsertTerminalContributionParams{
		PreservationID: pres.ID, RunID: run, Generation: 1, WorkerID: uuid.New(), UserID: f.owner,
		Authority: "current_flight", Tip: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CoverageDigest: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Manifest: []byte(`{}`),
		Disposition: "preserved",
	})
	if err != nil {
		t.Fatal(err)
	}

	// tx1, as the route writes it: preservation publishing first, then the contribution marker.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	qt := store.New(tx)
	if _, err = qt.MarkTerminalPreservationPublishing(ctx, store.MarkTerminalPreservationPublishingParams{ID: pres.ID, ExpectedState: "bound"}); err != nil {
		t.Fatal(err)
	}
	if _, err = qt.MarkTerminalContributionInvoked(ctx, con.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	assertTerminal409(t, cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""), "DELETE repo after tx1", run)
	assertTerminal409(t, cookieReq(t, f.router, http.MethodDelete, "/api/forge/connections/"+conn.String(), f.jwt, ""), "DELETE connection after tx1", run)
	rowSurvives(t, f, "repos", "id", repo, "a terminal 409")
	rowSurvives(t, f, "run_terminal_preservations", "run_id", run, "a terminal 409")

	// The push was refused: the contribution is decided and the preservation (no tip) goes back to
	// not_published, clearing the pointer.
	if _, err = q.RecordTerminalContributionRefused(ctx, store.RecordTerminalContributionRefusedParams{
		ID: con.ID, ExpectedState: "invoked", RefusalReason: pgtype.Text{String: "create_refused", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = q.SettleTerminalPreservationRefused(ctx, store.SettleTerminalPreservationRefusedParams{
		ID: pres.ID, NextCheckAt: pgtype.Timestamptz{Time: pres.NextCheckAt.Time, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if rec := cookieReq(t, f.router, http.MethodDelete, "/api/repos/"+repo.String(), f.jwt, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE repo after the pointer was released = %d, want 204\nbody: %s", rec.Code, rec.Body.String())
	}
}
