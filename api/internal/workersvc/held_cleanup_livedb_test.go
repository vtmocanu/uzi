package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2545 M4: the held-publication cleanup pass against a REAL Postgres, with the broker
// (listing and CAS delete) replaced by the recording fake forge of held_publish_livedb_test.go.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

type cleanupFix struct {
	*heldFix
	mu      sync.Mutex
	deletes []pushbroker.DeleteOptions
	delErr  error
}

func newCleanupFix(t *testing.T) *cleanupFix {
	t.Helper()
	c := &cleanupFix{heldFix: newHeldFix(t)}
	c.svc.heldDeleteFn = func(_ context.Context, o pushbroker.DeleteOptions) error {
		c.mu.Lock()
		c.deletes = append(c.deletes, o)
		err := c.delErr
		c.mu.Unlock()
		if err != nil {
			return err
		}
		c.fake.mu.Lock()
		defer c.fake.mu.Unlock()
		if c.fake.refs[o.Ref] == o.ExpectedOldTip {
			delete(c.fake.refs, o.Ref)
		}
		return nil
	}
	return c
}

func (c *cleanupFix) deleteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deletes)
}

func (c *cleanupFix) sweep() int64 {
	c.t.Helper()
	n, err := c.svc.SweepHeldPublications(c.e.ctx)
	if err != nil {
		c.t.Fatalf("SweepHeldPublications: %v", err)
	}
	return n
}

// created publishes step A for the fixture run, leaving a created row and the ref on the fake forge.
func (c *cleanupFix) created() store.RunHeldPublication {
	c.t.Helper()
	res, err := c.publish(c.req())
	if err != nil || res.State != "created" {
		c.t.Fatalf("publish = %+v, %v", res, err)
	}
	return c.mustRow()
}

// closeHold closes the fixture hold the way the archive path does, without touching the row.
func (c *cleanupFix) closeHold() {
	c.t.Helper()
	c.rawHold(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
		release_evidence='publication',released_at=now() WHERE id=$1`, c.hold)
}

// acknowledge releases the fixture hold against the row and acknowledges it, as step B does.
func (c *cleanupFix) acknowledge(pub store.RunHeldPublication) {
	c.t.Helper()
	n, err := c.e.q.ReleaseHeldPublicationHold(c.e.ctx, store.ReleaseHeldPublicationHoldParams{
		ID: c.hold, RunID: c.run, UserID: c.e.userID, Generation: 1, WorkerID: c.worker.ID,
		PublicationID: pub.ID, Tip: heldTestTip, CoverageDigest: heldTestCov,
	})
	if err != nil || n != 1 {
		c.t.Fatalf("release hold: rows=%d err=%v", n, err)
	}
	if _, err := c.e.q.AcknowledgeHeldPublication(c.e.ctx, store.AcknowledgeHeldPublicationParams{ID: pub.ID, RetentionSeconds: 7 * 24 * 3600}); err != nil {
		c.t.Fatalf("acknowledge: %v", err)
	}
}

func (c *cleanupFix) setExpiry(at time.Time) {
	c.t.Helper()
	c.e.exec(c.t, `UPDATE run_held_publications SET expires_at=$2 WHERE run_id=$1`, c.run, at)
}

func (c *cleanupFix) expireNow() { c.setExpiry(time.Now().Add(-time.Minute)) }

func (c *cleanupFix) openLaterHold() uuid.UUID {
	c.t.Helper()
	id := uuid.New()
	c.e.exec(c.t, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
		VALUES ($1,$2,$3,$4,2,'open',$5,'held-ident',$5,$4,true)`, id, c.e.userID, c.e.repoID, c.run, c.worker.ID)
	return id
}

func (c *cleanupFix) forgeHas(ref string) (string, bool) {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	tip, ok := c.fake.refs[ref]
	return tip, ok
}

func (c *cleanupFix) wantState(want string) store.RunHeldPublication {
	c.t.Helper()
	r := c.mustRow()
	if r.State != want {
		c.t.Fatalf("state = %s (error %q), want %s", r.State, r.LastError.String, want)
	}
	return r
}

// The expiry lane deletes an acknowledged ref once expires_at passes, with a CAS on the recorded
// tip, and clears the live pointer; an unexpired row is left alone.
func TestHeldCleanupExpiryDeletesLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	pub := c.created()
	c.acknowledge(pub)
	if n := c.sweep(); n != 0 || c.deleteCount() != 0 {
		t.Fatalf("an unexpired acknowledged row was touched: n=%d deletes=%d", n, c.deleteCount())
	}
	c.expireNow()
	if n := c.sweep(); n != 1 {
		t.Fatalf("sweep touched %d rows, want 1", n)
	}
	row := c.wantState("deleted")
	if row.LiveRunID.Valid || !row.DeletedAt.Valid {
		t.Fatalf("deleted row = %+v", row)
	}
	if _, ok := c.forgeHas(c.ref()); ok {
		t.Fatal("the ref is still on the forge")
	}
	if len(c.deletes) != 1 || c.deletes[0].ExpectedOldTip != heldTestTip || c.deletes[0].Ref != c.ref() || c.deletes[0].PAT != c.pat {
		t.Fatalf("delete options = %+v: want a CAS on the recorded tip with the server-derived credential", c.deletes)
	}
}

// Owner expiry only moves expires_at, and the expiry lane then deletes; the cleanup runs with the
// publication switch off too (the switch gates only new step A and B work).
func TestHeldCleanupOwnerExpiryAndSwitchOffLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	c.created()
	c.acknowledge(c.mustRow())
	rows, err := c.e.q.OwnerExpireHeldPublications(c.e.ctx, store.OwnerExpireHeldPublicationsParams{RunID: c.run, UserID: c.e.userID})
	if err != nil || len(rows) != 1 || !rows[0].OwnerExpiredAt.Valid {
		t.Fatalf("owner expire = %v, %v", rows, err)
	}
	c.svc.p.HeldPublication = false
	if n := c.sweep(); n != 1 {
		t.Fatalf("sweep touched %d rows, want 1", n)
	}
	c.wantState("deleted")
}

// Never a delete while ANY hold of the run is open, whatever its generation; the row is deferred
// (not blocking), and deleted once the hold closes. A stale listing (the hold opened after the
// row was listed) is caught by the re-check under the lock.
func TestHeldCleanupNeverDeletesWithOpenHoldLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	pub := c.created()
	c.acknowledge(pub)
	c.expireNow()
	later := c.openLaterHold()

	if n := c.sweep(); n != 0 || c.deleteCount() != 0 {
		t.Fatalf("a ref was deleted while a generation-2 hold is open: n=%d deletes=%d", n, c.deleteCount())
	}
	row := c.wantState("acknowledged")
	if !row.NextAttemptAt.Valid || !row.NextAttemptAt.Time.After(time.Now()) {
		t.Fatalf("a skipped row must be deferred, next_attempt_at = %+v", row.NextAttemptAt)
	}
	// The stale-listing path: call the protocol directly with the row as listed.
	row.NextAttemptAt.Valid = false
	if n, err := c.svc.heldDeleteItem(c.e.ctx, row, heldLaneExpiry); err != nil || n != 0 || c.deleteCount() != 0 {
		t.Fatalf("direct deletion with an open hold: n=%d err=%v deletes=%d", n, err, c.deleteCount())
	}

	c.rawHold(`UPDATE recovery_custody_holds SET state='released',live_worker_id=NULL,live_run_id=NULL,
		release_evidence='publication',released_at=now() WHERE id=$1`, later)
	c.e.exec(t, `UPDATE run_held_publications SET next_attempt_at=NULL WHERE run_id=$1`, c.run)
	if n := c.sweep(); n != 1 {
		t.Fatalf("sweep after the hold closed touched %d rows, want 1", n)
	}
	c.wantState("deleted")
}

// The orphan lane deletes a created, never-acknowledged ref only after the run's hold closed, and
// never touches an acknowledged row.
func TestHeldCleanupOrphanLaneLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	c.created()
	if n := c.sweep(); n != 0 || c.deleteCount() != 0 {
		t.Fatalf("the orphan lane deleted a ref whose hold is open: n=%d", n)
	}
	c.closeHold()
	if n := c.sweep(); n != 1 {
		t.Fatalf("sweep touched %d rows, want 1", n)
	}
	row := c.wantState("deleted")
	if row.AcknowledgedAt.Valid {
		t.Fatal("an orphan row carries an acknowledgement")
	}

	a := newCleanupFix(t)
	pub := a.created()
	a.acknowledge(pub)
	if n := a.sweep(); n != 0 || a.deleteCount() != 0 {
		t.Fatalf("the orphan lane touched an acknowledged row: n=%d", n)
	}
	a.wantState("acknowledged")
	// And directly, with a stale listing that still says created.
	stale := pub
	if n, err := a.svc.heldDeleteItem(a.e.ctx, stale, heldLaneOrphan); err != nil || n != 0 || a.deleteCount() != 0 {
		t.Fatalf("orphan protocol on an acknowledged row: n=%d err=%v deletes=%d", n, err, a.deleteCount())
	}
}

// delete_unknown: an absent ref is deleted and a foreign tip abandoned, both with a hold open; a
// ref still at the tip is retried only once no hold is open.
func TestHeldCleanupDeleteUnknownLiveDB(t *testing.T) {
	setup := func(t *testing.T) *cleanupFix {
		c := newCleanupFix(t)
		c.created()
		c.e.exec(t, `UPDATE run_held_publications SET state='delete_unknown', next_attempt_at=now() - interval '1 minute' WHERE run_id=$1`, c.run)
		return c
	}
	t.Run("absent with a hold open", func(t *testing.T) {
		c := setup(t)
		c.fake.mu.Lock()
		delete(c.fake.refs, c.ref())
		c.fake.mu.Unlock()
		if n := c.sweep(); n != 1 {
			t.Fatalf("touched %d", n)
		}
		c.wantState("deleted")
		c.assertHoldOpen()
	})
	t.Run("foreign tip with a hold open", func(t *testing.T) {
		c := setup(t)
		c.fake.setRef(c.ref(), heldTestTip2)
		if n := c.sweep(); n != 1 || c.deleteCount() != 0 {
			t.Fatalf("touched %d, deletes %d", n, c.deleteCount())
		}
		row := c.wantState("abandoned")
		if row.RefusalReason.String != "foreign_tip" || row.LiveRunID.Valid {
			t.Fatalf("abandoned row = %+v", row)
		}
		if tip, _ := c.forgeHas(c.ref()); tip != heldTestTip2 {
			t.Fatal("a foreign tip was modified")
		}
	})
	t.Run("at the tip waits for the hold", func(t *testing.T) {
		c := setup(t)
		if n := c.sweep(); n != 0 || c.deleteCount() != 0 {
			t.Fatalf("a delete was retried with a hold open: n=%d deletes=%d", n, c.deleteCount())
		}
		row := c.wantState("delete_unknown")
		if !row.NextAttemptAt.Valid {
			t.Fatal("the skipped retry was not deferred")
		}
		c.closeHold()
		c.e.exec(t, `UPDATE run_held_publications SET next_attempt_at=now() - interval '1 minute' WHERE run_id=$1`, c.run)
		if n := c.sweep(); n != 1 {
			t.Fatalf("touched %d", n)
		}
		c.wantState("deleted")
		if c.deleteCount() != 1 {
			t.Fatalf("deletes = %d, want 1", c.deleteCount())
		}
	})
}

// A delete that fails (or whose confirmation fails) is delete_unknown with a backoff and a
// sanitized error that never carries the credential; the next listing settles it.
func TestHeldCleanupDeleteFailureIsUnknownAndRedactedLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	pub := c.created()
	c.acknowledge(pub)
	c.expireNow()
	c.delErr = errors.New("push to https://bot:" + c.pat + "@forge.e2e/g/interlock.git failed\x1b[31m red\nsecond line")
	if n := c.sweep(); n != 1 {
		t.Fatalf("touched %d", n)
	}
	row := c.wantState("delete_unknown")
	if strings.Contains(row.LastError.String, c.pat) || strings.ContainsAny(row.LastError.String, "\x1b\n") || row.LastError.String == "" {
		t.Fatalf("last_error = %q", row.LastError.String)
	}
	if !row.NextAttemptAt.Valid || !row.NextAttemptAt.Time.After(time.Now()) || !row.LiveRunID.Valid {
		t.Fatalf("row = %+v: want a backoff and the live pointer kept", row)
	}
	// The backoff holds the retry back; once due and the forge works, the retry deletes.
	if n := c.sweep(); n != 0 {
		t.Fatalf("a backed-off row was retried early (n=%d)", n)
	}
	c.delErr = nil
	c.e.exec(t, `UPDATE run_held_publications SET next_attempt_at=now() - interval '1 minute' WHERE run_id=$1`, c.run)
	if n := c.sweep(); n != 1 {
		t.Fatalf("touched %d", n)
	}
	c.wantState("deleted")
}

// A create whose outcome is unknown is never terminal on an absent ref and is never re-sent; a
// late-applied create reconciles to created, and the orphan lane then deletes it after the hold
// closes.
func TestHeldCleanupCreateUnknownLateApplyLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	c.fake.sendFn = func(context.Context, pushbroker.HeldPackOptions) (pushbroker.HeldCreateOutcome, error) {
		return pushbroker.HeldCreateUnknown, errors.New("deadline exceeded")
	}
	res, err := c.publish(c.req())
	if err != nil || res.State != "create_unknown" {
		t.Fatalf("publish = %+v, %v", res, err)
	}
	c.e.exec(t, `UPDATE run_held_publications SET next_attempt_at = now() - interval '1 second' WHERE run_id=$1`, c.run)
	_, sendsBefore, _ := c.fake.counts()
	if n := c.sweep(); n != 0 {
		t.Fatalf("an absent ref moved the row (n=%d)", n)
	}
	row := c.wantState("create_unknown")
	if !row.NextAttemptAt.Valid || !row.NextAttemptAt.Time.After(time.Now()) || !row.LiveRunID.Valid {
		t.Fatalf("row = %+v: want unknown, scheduled, pointer kept", row)
	}
	if _, sends, _ := c.fake.counts(); sends != sendsBefore {
		t.Fatal("the cleanup re-sent a create")
	}
	if c.deleteCount() != 0 {
		t.Fatal("the cleanup deleted an unconfirmed ref")
	}

	c.fake.setRef(c.ref(), heldTestTip) // the create applied late
	c.e.exec(t, `UPDATE run_held_publications SET next_attempt_at = now() - interval '1 second' WHERE run_id=$1`, c.run)
	if n := c.sweep(); n != 1 {
		t.Fatalf("reconcile touched %d", n)
	}
	c.wantState("created")
	if c.deleteCount() != 0 {
		t.Fatal("the orphan lane deleted a ref whose hold is still open")
	}
	c.closeHold()
	if n := c.sweep(); n != 1 {
		t.Fatalf("orphan sweep touched %d", n)
	}
	c.wantState("deleted")
}

// A busy run (another transaction holds the row lock) makes the item skip: no forge call, the
// state is unchanged, next_attempt_at is deferred, and the sweep returns promptly.
func TestHeldCleanupBusyRunSkipsLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	pub := c.created()
	c.acknowledge(pub)
	c.expireNow()
	_, _, listsBefore := c.fake.counts()

	tx, err := c.e.pool.Begin(c.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(c.e.ctx) }()
	if _, err := tx.Exec(c.e.ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, c.run); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if n := c.sweep(); n != 0 {
		t.Fatalf("touched %d", n)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the sweep waited %v behind a busy run", time.Since(start))
	}
	row := c.wantState("acknowledged")
	if !row.NextAttemptAt.Valid || !row.NextAttemptAt.Time.After(time.Now()) {
		t.Fatalf("next_attempt_at = %+v, want deferred", row.NextAttemptAt)
	}
	if _, _, lists := c.fake.counts(); lists != listsBefore || c.deleteCount() != 0 {
		t.Fatal("a busy run still reached the forge")
	}
}

// A skipped (busy) row does not block the row behind it in the same pass.
func TestHeldCleanupSkippedRowDoesNotBlockNextLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	pub := c.created()
	c.acknowledge(pub)
	c.expireNow()

	// A second failed run with its own acknowledged, expired publication.
	run2 := c.e.seedLegacyRunningRun(t, c.worker.ID)
	hold2 := uuid.New()
	c.e.exec(t, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
		VALUES ($1,$2,$3,$4,1,'open',$5,'held-ident',$5,$4,true)`, hold2, c.e.userID, c.e.repoID, run2, c.worker.ID)
	c.e.exec(t, `UPDATE runs SET status='failed', fail_origin='agent_failure', claim_generation=1 WHERE id=$1`, run2)
	if res, err := c.svc.PublishHeld(c.e.ctx, c.worker, run2, c.req(), []byte("pack")); err != nil || res.State != "created" {
		t.Fatalf("publish run2 = %+v, %v", res, err)
	}
	row2, err := c.e.q.GetHeldPublicationByRunGeneration(c.e.ctx, store.GetHeldPublicationByRunGenerationParams{RunID: run2, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := c.e.q.ReleaseHeldPublicationHold(c.e.ctx, store.ReleaseHeldPublicationHoldParams{
		ID: hold2, RunID: run2, UserID: c.e.userID, Generation: 1, WorkerID: c.worker.ID,
		PublicationID: row2.ID, Tip: heldTestTip, CoverageDigest: heldTestCov,
	}); err != nil || n != 1 {
		t.Fatalf("release hold2: %d %v", n, err)
	}
	if _, err := c.e.q.AcknowledgeHeldPublication(c.e.ctx, store.AcknowledgeHeldPublicationParams{ID: row2.ID, RetentionSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	c.e.exec(t, `UPDATE run_held_publications SET expires_at = now() - interval '2 minutes' WHERE run_id=$1`, c.run)
	c.e.exec(t, `UPDATE run_held_publications SET expires_at = now() - interval '1 minute' WHERE run_id=$1`, run2)

	tx, err := c.e.pool.Begin(c.e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(c.e.ctx) }()
	if _, err := tx.Exec(c.e.ctx, `SELECT 1 FROM runs WHERE id=$1 FOR UPDATE`, c.run); err != nil { // the older row is busy
		t.Fatal(err)
	}
	if n := c.sweep(); n != 1 {
		t.Fatalf("touched %d, want only run2's row", n)
	}
	c.wantState("acknowledged")
	got, err := c.e.q.GetHeldPublication(c.e.ctx, row2.ID)
	if err != nil || got.State != "deleted" {
		t.Fatalf("run2 row = %+v, %v", got, err)
	}
	// The deferred row no longer heads the list.
	due, err := c.e.q.ListHeldPublicationsDueExpiry(c.e.ctx, store.ListHeldPublicationsDueExpiryParams{Now: salvageTS(time.Now()), Lim: 10})
	if err != nil || containsHeldPub(due, pub.ID) {
		t.Fatalf("the skipped row still heads the expiry lane: %v %v", due, err)
	}
}

// The cleanup's run lock does not block a heartbeat-style FK insert that references the run.
func TestHeldCleanupRunLockDoesNotBlockFKInsertLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	pub := c.created()
	c.acknowledge(pub)
	c.expireNow()
	var inserted error
	c.svc.heldCleanupHook = func(stage string) {
		if stage != "run_locked" {
			return
		}
		ctx, cancel := context.WithTimeout(c.e.ctx, 3*time.Second)
		defer cancel()
		_, inserted = c.e.pool.Exec(ctx, `INSERT INTO worker_active_runs
			(worker_id, run_id, claim_generation, phase, snapshot_epoch, reported_at)
			VALUES ($1,$2,1,'running',1,now())`, c.worker.ID, c.run)
	}
	if n := c.sweep(); n != 1 {
		t.Fatalf("touched %d", n)
	}
	if inserted != nil {
		t.Fatalf("a heartbeat-style FK insert was blocked by the cleanup lock: %v", inserted)
	}
	c.wantState("deleted")
}

// A panic in the forge seam is recovered into that item's deferred, sanitized error and does not
// end the pass.
func TestHeldCleanupPanicIsRecoveredLiveDB(t *testing.T) {
	c := newCleanupFix(t)
	pub := c.created()
	c.acknowledge(pub)
	c.expireNow()
	c.svc.heldDeleteFn = func(context.Context, pushbroker.DeleteOptions) error { panic("go-git nil dereference") }
	_, err := c.svc.SweepHeldPublications(c.e.ctx)
	if err != nil {
		t.Fatalf("a recovered panic must not fail the pass: %v", err)
	}
	// The panic happened after tx started; the row keeps its state, and the next pass sees it.
	row := c.mustRow()
	if row.State != "acknowledged" && row.State != "delete_unknown" {
		t.Fatalf("state = %s", row.State)
	}
	if !row.LastError.Valid || !strings.Contains(row.LastError.String, "panic") {
		t.Fatalf("last_error = %+v, want the recovered panic", row.LastError)
	}
}

func containsHeldPub(rows []store.RunHeldPublication, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}
