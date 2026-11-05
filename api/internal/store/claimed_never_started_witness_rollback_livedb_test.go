package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// migration00404DownSQL returns the raw `-- +goose Down` block of 00404 as one script. The shared
// statement splitter cuts on every semicolon, which would shred the DO block's body, so this test
// runs the exact shipped Down text in a single simple-protocol Exec.
func migration00404DownSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("migrations", "00404_claimed_never_started_witness.sql"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "-- +goose Down")
	if i < 0 {
		t.Fatal("00404 has no Down block")
	}
	return s[i+len("-- +goose Down"):]
}

// TestClaimedNeverStartedWitnessRollbackLiveDB proves 00404's Down never relabels a run: a live
// recovery_wait/retained_continuation_unwitnessed park makes the Down fail with the row untouched,
// and after a real owner cancel (CancelRunServerSide) the Down succeeds, clears the stale cause,
// leaves the run cancelled and its custody hold untouched, restores the pre-00404 CHECK and
// fn_custody_attention, and drops the witness columns. Every Down runs in a rolled-back tx.
func TestClaimedNeverStartedWitnessRollbackLiveDB(t *testing.T) {
	f := newAdmissionFixture(t, 2)
	run := f.runs[0]
	// A second row carrying observation keys but NOT presence: the Down must strip it too, or the
	// narrowed CHECK rejects it.
	partial := f.runs[1]
	f.exec(`UPDATE runs SET worker_recovery_evidence = jsonb_build_object('checkpoint_tip', NULL, 'available_capture', false,
 'publication_uncertain', false, 'capture_uncertain', false, 'custody_uncertain', false, 'unknown', false,
 'recorded_at', '2026-11-04T10:00:00Z', 'worker_snapshot_epoch', 3, 'deadline', '2026-11-04T11:00:00Z') WHERE id=$1`, partial)
	f.exec("UPDATE runs SET status='recovery_wait', recovery_wait_cause='retained_continuation_unwitnessed', recovery_retry_not_before=now()+interval '1 hour', claim_generation=2 WHERE id=$1", run)
	down := migration00404DownSQL(t)

	rowState := func(q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}) string {
		t.Helper()
		var got string
		if err := q.QueryRow(f.ctx, `SELECT concat_ws('|', r.status, coalesce(r.recovery_wait_cause,'<null>'), coalesce(r.recovery_retry_not_before::text,'<null>'),
 r.worker_id, r.claim_generation, coalesce(r.finished_at::text,'<null>'), coalesce(r.fail_origin,'<null>'),
 h.id, h.state, h.generation, h.run_id)
 FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id WHERE r.id=$1`, run).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// (i) live park: the Down refuses and the row is unchanged.
	func() {
		tx, err := f.pool.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(f.ctx) }()
		// Settle other tests' leftover parks in this rolled-back tx so only this run can trigger the refusal.
		if _, err := tx.Exec(f.ctx, "UPDATE runs SET status='cancelled', finished_at=now() WHERE status='recovery_wait' AND recovery_wait_cause='retained_continuation_unwitnessed' AND id<>$1", run); err != nil {
			t.Fatal(err)
		}
		before := rowState(tx)
		if _, err := tx.Exec(f.ctx, "SAVEPOINT refuse"); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(f.ctx, down)
		if err == nil {
			t.Fatal("Down succeeded while a run is still parked at retained_continuation_unwitnessed")
		}
		if !strings.Contains(err.Error(), "Resume or Cancel") {
			t.Fatalf("Down failed for the wrong reason: %v", err)
		}
		if _, err := tx.Exec(f.ctx, "ROLLBACK TO SAVEPOINT refuse"); err != nil {
			t.Fatal(err)
		}
		if after := rowState(tx); after != before {
			t.Fatalf("refused Down changed the row: before=%q after=%q", before, after)
		}
	}()

	// (ii) owner cancel through the real query, which leaves the cause set on the terminal row.
	n, err := f.q.CancelRunServerSide(f.ctx, store.CancelRunServerSideParams{ID: run, UserID: f.owner, StopReason: pgtype.Text{}})
	if err != nil || n != 1 {
		t.Fatalf("CancelRunServerSide rows=%d err=%v", n, err)
	}
	var status, cause string
	if err := f.pool.QueryRow(f.ctx, "SELECT status, coalesce(recovery_wait_cause,'<null>') FROM runs WHERE id=$1", run).Scan(&status, &cause); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || cause != "retained_continuation_unwitnessed" {
		t.Fatalf("precondition: owner cancel left status=%s cause=%s", status, cause)
	}
	// Seed evidence keys on the terminal row so the Down's key strip is exercised too.
	f.exec(`UPDATE runs SET worker_recovery_evidence = jsonb_build_object('checkpoint_tip', NULL, 'available_capture', false,
 'publication_uncertain', false, 'capture_uncertain', false, 'custody_uncertain', false, 'unknown', false,
 'recorded_at', '2026-11-04T10:00:00Z', 'presence', 'p1', 'worker_snapshot_epoch', 3) WHERE id=$1`, run)

	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	before := rowState(tx)
	attention := func(cause string) string {
		t.Helper()
		var got string
		if err := tx.QueryRow(f.ctx, "SELECT fn_custody_attention('open',true,false,'available','recovery_wait',$1)", cause).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := attention("retained_continuation_unwitnessed"); got != "source_only" {
		t.Fatalf("Up attention = %s, want source_only", got)
	}
	// The database is shared with other tests' leftover parked runs, which would (rightly) make the
	// Down refuse; settle them inside this rolled-back tx so only this test's run is under test.
	if _, err := tx.Exec(f.ctx, "UPDATE runs SET status='cancelled', finished_at=now() WHERE status='recovery_wait' AND recovery_wait_cause='retained_continuation_unwitnessed' AND id<>$1", run); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, down); err != nil {
		t.Fatalf("Down failed after the owner cancel: %v", err)
	}
	wantAfter := strings.Replace(before, "cancelled|retained_continuation_unwitnessed|", "cancelled|<null>|", 1)
	if wantAfter == before {
		t.Fatalf("unexpected pre-Down row state %q", before)
	}
	if after := rowState(tx); after != wantAfter {
		t.Fatalf("Down must only clear the cause: before=%q after=%q want=%q", before, after, wantAfter)
	}
	var exhausted int
	if err := tx.QueryRow(f.ctx, "SELECT count(*) FROM runs WHERE recovery_wait_cause='retained_recovery_exhausted'").Scan(&exhausted); err != nil || exhausted != 0 {
		t.Fatalf("rows relabelled retained_recovery_exhausted = %d (%v), want 0", exhausted, err)
	}
	var evKeys bool
	for name, id := range map[string]any{"presence row": run, "row without presence": partial} {
		if err := tx.QueryRow(f.ctx, "SELECT worker_recovery_evidence ?| ARRAY['presence','registration_evidence_epoch','worker_snapshot_epoch','deadline'] FROM runs WHERE id=$1", id).Scan(&evKeys); err != nil || evKeys {
			t.Fatalf("%s: observation keys survived the Down: %v (%v)", name, evKeys, err)
		}
	}
	if got := attention("retained_continuation_unwitnessed"); got != "archive_ready" {
		t.Fatalf("Down attention for the new cause = %s, want archive_ready (function restored)", got)
	}
	if _, err := tx.Exec(f.ctx, "SAVEPOINT narrow"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, "UPDATE runs SET recovery_wait_cause='retained_continuation_unwitnessed' WHERE id=$1", run); err == nil {
		t.Fatal("Down CHECK still admits the new cause")
	}
	if _, err := tx.Exec(f.ctx, "ROLLBACK TO SAVEPOINT narrow"); err != nil {
		t.Fatal(err)
	}
	var cols int
	if err := tx.QueryRow(f.ctx, "SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='runs' AND column_name IN ('registration_evidence_generation','registration_evidence_nonce','registration_evidence_epoch','snapshot_departed_generation','snapshot_departed_nonce','snapshot_departed_epoch')").Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("witness columns after Down = %d (%v), want 0", cols, err)
	}
}
