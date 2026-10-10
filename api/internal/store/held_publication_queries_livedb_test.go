package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2545 M1: the held_publication.sql queries drive the lifecycle end to end against the
// real schema, so M2 to M4 build on statements that have executed.

func tstz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func TestHeldPublicationQueriesLiveDB(t *testing.T) {
	ctx, _, pool := heldMigratedPool(t, "held_queries_")
	q := store.New(pool)
	failed := "agent_failure"
	newRow := func() (heldFixture, store.RunHeldPublication) {
		f := heldNewFixture(ctx, t, pool, true, "failed", &failed)
		mustExec(ctx, t, pool, `DELETE FROM run_held_publications WHERE id=$1`, f.pub)
		row, err := q.UpsertHeldPublication(ctx, store.UpsertHeldPublicationParams{
			RunID: f.run, Generation: 1, HoldID: f.hold, UserID: f.user, RepoID: f.repo, WorkerID: f.worker,
			Ref: "refs/uzi-held/" + f.run.String() + "/1", Tip: heldTip, CoverageDigest: heldDigest,
		})
		if err != nil {
			t.Fatalf("UpsertHeldPublication: %v", err)
		}
		return f, row
	}

	t.Run("upsert loads the existing identity instead of overwriting it", func(t *testing.T) {
		f, row := newRow()
		if row.State != "prepared" || !row.LiveRunID.Valid || row.CreateInvokedAt.Valid {
			t.Fatalf("new row = %+v", row)
		}
		again, err := q.UpsertHeldPublication(ctx, store.UpsertHeldPublicationParams{
			RunID: f.run, Generation: 1, HoldID: f.hold, UserID: f.user, RepoID: f.repo, WorkerID: f.worker,
			Ref: "refs/uzi-held/" + f.run.String() + "/1", Tip: "9999999999999999999999999999999999999999", CoverageDigest: heldDigest,
		})
		if err != nil || again.ID != row.ID || again.Tip != heldTip {
			t.Fatalf("second upsert = %+v, %v; want the original row (tip %s) back", again, err, heldTip)
		}
	})

	t.Run("exactly one caller wins the invoked marker", func(t *testing.T) {
		_, row := newRow()
		won, err := q.MarkHeldCreateInvoked(ctx, row.ID)
		if err != nil || won.State != "invoked" || !won.CreateInvokedAt.Valid {
			t.Fatalf("first MarkHeldCreateInvoked = %+v, %v", won, err)
		}
		if _, err := q.MarkHeldCreateInvoked(ctx, row.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("second MarkHeldCreateInvoked = %v, want no rows (loser reconciles only)", err)
		}
	})

	t.Run("outcomes", func(t *testing.T) {
		_, row := newRow()
		if _, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: row.ID}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("outcome before the marker = %v, want no rows", err)
		}
		if _, err := q.MarkHeldCreateInvoked(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "acknowledged", ID: row.ID}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("outcome to acknowledged = %v, want no rows", err)
		}
		next := time.Now().Add(time.Hour)
		unknown, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{
			State: "create_unknown", ID: row.ID, NextAttemptAt: tstz(next),
			LastError: pgtype.Text{String: "deadline exceeded", Valid: true},
		})
		if err != nil || unknown.State != "create_unknown" || !unknown.LiveRunID.Valid || !unknown.NextAttemptAt.Valid {
			t.Fatalf("create_unknown = %+v, %v", unknown, err)
		}
		created, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: row.ID})
		if err != nil || created.State != "created" || !created.RefCreatedAt.Valid || created.LastError.Valid || created.NextAttemptAt.Valid {
			t.Fatalf("created = %+v, %v", created, err)
		}

		_, refused := newRow()
		if _, err := q.MarkHeldCreateInvoked(ctx, refused.ID); err != nil {
			t.Fatal(err)
		}
		got, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{
			State: "refused", ID: refused.ID, RefusalReason: pgtype.Text{String: "create_refused", Valid: true},
		})
		if err != nil || got.State != "refused" || got.LiveRunID.Valid || got.RefusalReason.String != "create_refused" {
			t.Fatalf("refused = %+v, %v; want the live pointer cleared", got, err)
		}
	})

	t.Run("release then acknowledge, hold first", func(t *testing.T) {
		f, row := newRow()
		if _, err := q.MarkHeldCreateInvoked(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: row.ID}); err != nil {
			t.Fatal(err)
		}
		expires := time.Now().Add(24 * time.Hour)
		if _, err := q.AcknowledgeHeldPublication(ctx, store.AcknowledgeHeldPublicationParams{ID: row.ID, ExpiresAt: tstz(expires)}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("acknowledge before the hold is released = %v, want no rows", err)
		}
		rel := store.ReleaseHeldPublicationHoldParams{
			ID: f.hold, RunID: f.run, UserID: f.user, Generation: 1, WorkerID: f.worker,
			PublicationID: row.ID, Tip: heldTip, CoverageDigest: heldDigest,
		}
		bad := rel
		bad.CoverageDigest = "4444444444444444444444444444444444444444444444444444444444444444"
		if n, err := q.ReleaseHeldPublicationHold(ctx, bad); err != nil || n != 0 {
			t.Fatalf("release with a foreign digest: rows=%d err=%v", n, err)
		}
		if n, err := q.ReleaseHeldPublicationHold(ctx, rel); err != nil || n != 1 {
			t.Fatalf("release: rows=%d err=%v", n, err)
		}
		if n, err := q.ReleaseHeldPublicationHold(ctx, rel); err != nil || n != 0 {
			t.Fatalf("second release: rows=%d err=%v", n, err)
		}
		ack, err := q.AcknowledgeHeldPublication(ctx, store.AcknowledgeHeldPublicationParams{ID: row.ID, ExpiresAt: tstz(expires)})
		if err != nil || ack.State != "acknowledged" || !ack.AcknowledgedAt.Valid || !ack.ExpiresAt.Valid {
			t.Fatalf("acknowledge = %+v, %v", ack, err)
		}
		hold, err := q.GetHeldPublicationHold(ctx, store.GetHeldPublicationHoldParams{RunID: f.run, UserID: f.user, WorkerID: f.worker, Generation: 1})
		if err != nil || hold.State != "released" || !hold.FinalPublicationID.Valid || hold.FinalPublicationID.Bytes != row.ID {
			t.Fatalf("hold = %+v, %v", hold, err)
		}
		if n, err := q.CountOpenHoldsForRun(ctx, f.run); err != nil || n != 0 {
			t.Fatalf("open holds = %d, %v", n, err)
		}

		// Lanes: the acknowledged row is due only after its expiry, and the orphan lane never
		// sees it.
		due, err := q.ListHeldPublicationsDueExpiry(ctx, store.ListHeldPublicationsDueExpiryParams{Now: tstz(time.Now()), Lim: 5})
		if err != nil || containsHeld(due, row.ID) {
			t.Fatalf("expiry lane before expiry = %v, %v", ids(due), err)
		}
		due, err = q.ListHeldPublicationsDueExpiry(ctx, store.ListHeldPublicationsDueExpiryParams{Now: tstz(expires.Add(time.Minute)), Lim: 5})
		if err != nil || !containsHeld(due, row.ID) {
			t.Fatalf("expiry lane after expiry = %v, %v", ids(due), err)
		}
		orphans, err := q.ListHeldPublicationsDueOrphan(ctx, 50)
		if err != nil || containsHeld(orphans, row.ID) {
			t.Fatalf("orphan lane selected an acknowledged row: %v %v", ids(orphans), err)
		}
		if _, err := q.RecordHeldDeleteOutcome(ctx, store.RecordHeldDeleteOutcomeParams{State: "deleted", ID: row.ID, FromStates: []string{"created"}}); err != nil {
			t.Fatal(err)
		}
		if n := heldState(ctx, t, pool, row.ID); n != "acknowledged" {
			t.Fatalf("delete outcome with the wrong source state moved the row to %s", n)
		}
		if n, err := q.RecordHeldDeleteOutcome(ctx, store.RecordHeldDeleteOutcomeParams{State: "deleted", ID: row.ID, FromStates: []string{"acknowledged", "delete_unknown"}}); err != nil || n != 1 {
			t.Fatalf("delete outcome: rows=%d err=%v", n, err)
		}
		got, err := q.GetHeldPublication(ctx, row.ID)
		if err != nil || got.State != "deleted" || got.LiveRunID.Valid || !got.DeletedAt.Valid {
			t.Fatalf("deleted row = %+v, %v", got, err)
		}
	})

	t.Run("orphan lane waits for the hold to close", func(t *testing.T) {
		f, row := newRow()
		if _, err := q.MarkHeldCreateInvoked(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: row.ID}); err != nil {
			t.Fatal(err)
		}
		orphans, err := q.ListHeldPublicationsDueOrphan(ctx, 50)
		if err != nil || containsHeld(orphans, row.ID) {
			t.Fatalf("orphan lane selected a created row whose hold is open: %v %v", ids(orphans), err)
		}
		// The archive path closes the hold without ever touching the publication.
		mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard`)
		mustExec(ctx, t, pool, `UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
			release_evidence='publication',released_at=now() WHERE id=$1`, f.hold)
		mustExec(ctx, t, pool, `ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard`)
		orphans, err = q.ListHeldPublicationsDueOrphan(ctx, 50)
		if err != nil || !containsHeld(orphans, row.ID) {
			t.Fatalf("orphan lane after the hold closed = %v, %v", ids(orphans), err)
		}
	})

	t.Run("reconcile lane honours the schedule and the invoked grace", func(t *testing.T) {
		_, row := newRow()
		if _, err := q.MarkHeldCreateInvoked(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		list := func(at time.Time, grace float64) []store.RunHeldPublication {
			t.Helper()
			rows, err := q.ListHeldPublicationsDueReconcile(ctx, store.ListHeldPublicationsDueReconcileParams{GraceSeconds: grace, Now: tstz(at), Lim: 50})
			if err != nil {
				t.Fatal(err)
			}
			return rows
		}
		if containsHeld(list(now, 300), row.ID) {
			t.Fatal("a just-invoked create was offered to reconcile inside its grace")
		}
		if !containsHeld(list(now.Add(10*time.Minute), 300), row.ID) {
			t.Fatal("an invoked create past its grace was not offered to reconcile")
		}
		next := now.Add(2 * time.Hour)
		if _, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "create_unknown", ID: row.ID, NextAttemptAt: tstz(next)}); err != nil {
			t.Fatal(err)
		}
		if containsHeld(list(now.Add(time.Hour), 0), row.ID) || !containsHeld(list(next.Add(time.Second), 0), row.ID) {
			t.Fatal("create_unknown is not scheduled by next_attempt_at")
		}
	})

	t.Run("owner expiry is owner-scoped and acknowledged-only", func(t *testing.T) {
		f, row := newRow()
		if _, err := q.MarkHeldCreateInvoked(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: row.ID}); err != nil {
			t.Fatal(err)
		}
		if got, err := q.OwnerExpireHeldPublications(ctx, store.OwnerExpireHeldPublicationsParams{RunID: f.run, UserID: f.user}); err != nil || len(got) != 0 {
			t.Fatalf("expiring a created (unacknowledged) publication = %v, %v", ids(got), err)
		}
		if n, err := q.ReleaseHeldPublicationHold(ctx, store.ReleaseHeldPublicationHoldParams{
			ID: f.hold, RunID: f.run, UserID: f.user, Generation: 1, WorkerID: f.worker, PublicationID: row.ID, Tip: heldTip, CoverageDigest: heldDigest,
		}); err != nil || n != 1 {
			t.Fatalf("release: rows=%d err=%v", n, err)
		}
		if _, err := q.AcknowledgeHeldPublication(ctx, store.AcknowledgeHeldPublicationParams{ID: row.ID, ExpiresAt: tstz(time.Now().Add(48 * time.Hour))}); err != nil {
			t.Fatal(err)
		}
		if got, err := q.OwnerExpireHeldPublications(ctx, store.OwnerExpireHeldPublicationsParams{RunID: f.run, UserID: uuid.New()}); err != nil || len(got) != 0 {
			t.Fatalf("a foreign owner expired %v, %v", ids(got), err)
		}
		got, err := q.OwnerExpireHeldPublications(ctx, store.OwnerExpireHeldPublicationsParams{RunID: f.run, UserID: f.user})
		if err != nil || len(got) != 1 || !got[0].OwnerExpiredAt.Valid || got[0].ExpiresAt.Time.After(time.Now().Add(time.Minute)) {
			t.Fatalf("owner expiry = %+v, %v", got, err)
		}
		due, err := q.ListHeldPublicationsDueExpiry(ctx, store.ListHeldPublicationsDueExpiryParams{Now: tstz(time.Now().Add(time.Second)), Lim: 50})
		if err != nil || !containsHeld(due, row.ID) {
			t.Fatalf("owner-expired row is not due: %v %v", ids(due), err)
		}
	})

	t.Run("live refs name the repo and connection", func(t *testing.T) {
		f, row := newRow()
		var conn uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT connection_id FROM repos WHERE id=$1`, f.repo).Scan(&conn); err != nil {
			t.Fatal(err)
		}
		if n, err := q.CountLiveHeldPublicationsForRepo(ctx, store.CountLiveHeldPublicationsForRepoParams{RepoID: f.repo, UserID: f.user}); err != nil || n != 1 {
			t.Fatalf("repo count = %d, %v", n, err)
		}
		if n, err := q.CountLiveHeldPublicationsForRepo(ctx, store.CountLiveHeldPublicationsForRepoParams{RepoID: f.repo, UserID: uuid.New()}); err != nil || n != 0 {
			t.Fatalf("a foreign owner counted %d, %v", n, err)
		}
		if n, err := q.CountLiveHeldPublicationsForConnection(ctx, store.CountLiveHeldPublicationsForConnectionParams{ConnectionID: conn, UserID: f.user}); err != nil || n != 1 {
			t.Fatalf("connection count = %d, %v", n, err)
		}
		refs, err := q.ListLiveHeldPublicationRefsForRepo(ctx, store.ListLiveHeldPublicationRefsForRepoParams{RepoID: f.repo, UserID: f.user, Lim: 10})
		if err != nil || len(refs) != 1 || refs[0].Ref != row.Ref || refs[0].RunID != f.run {
			t.Fatalf("repo refs = %+v, %v", refs, err)
		}
		refs2, err := q.ListLiveHeldPublicationRefsForConnection(ctx, store.ListLiveHeldPublicationRefsForConnectionParams{ConnectionID: conn, UserID: f.user, Lim: 10})
		if err != nil || len(refs2) != 1 || refs2[0].Ref != row.Ref {
			t.Fatalf("connection refs = %+v, %v", refs2, err)
		}
		list, err := q.ListHeldPublicationsForRun(ctx, store.ListHeldPublicationsForRunParams{RunID: f.run, UserID: f.user})
		if err != nil || len(list) != 1 {
			t.Fatalf("owner list = %v, %v", ids(list), err)
		}
		if list, err := q.ListHeldPublicationsForRun(ctx, store.ListHeldPublicationsForRunParams{RunID: f.run, UserID: uuid.New()}); err != nil || len(list) != 0 {
			t.Fatalf("a foreign owner listed %v, %v", ids(list), err)
		}
		if got, err := q.LockHeldPublicationByRunGeneration(ctx, store.LockHeldPublicationByRunGenerationParams{RunID: f.run, Generation: 1}); err != nil || got.ID != row.ID {
			t.Fatalf("lock = %+v, %v", got, err)
		}
		if got, err := q.GetHeldPublicationByRunGeneration(ctx, store.GetHeldPublicationByRunGenerationParams{RunID: f.run, Generation: 1}); err != nil || got.ID != row.ID {
			t.Fatalf("by run/generation = %+v, %v", got, err)
		}
	})
}

func containsHeld(rows []store.RunHeldPublication, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

func ids(rows []store.RunHeldPublication) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func heldState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT state FROM run_held_publications WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}
