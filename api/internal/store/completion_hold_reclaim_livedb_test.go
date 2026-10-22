package store_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCompletionHoldHistoricalAttemptAfterReclaimLiveDB keeps a real attempt from
// an earlier flight through resume/reclaim, then checks both generation fences
// in SetRunCompletionHold: the run update and its wall-input settlement CTE.
func TestCompletionHoldHistoricalAttemptAfterReclaimLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	protocolCaps := []string{"completion_interlock_v1"}
	w := fx.worker("completion-reclaim", wpWorker{nonce: "completion-reclaim", protocolCaps: protocolCaps})
	wid := pgtype.UUID{Bytes: w, Valid: true}
	const oldGeneration int64 = 7
	oldGen := pgtype.Int8{Int64: oldGeneration, Valid: true}
	id := fx.run(wpRun{
		worker: &w, claimGen: oldGeneration, completionContract: p32(1),
		budgetWall: p32(60), startedAgo: 2 * time.Hour,
	})
	if r := fx.mustRun(id); r.CompletionAttempts != 0 {
		t.Fatalf("initial completion_attempts = %d, want 0", r.CompletionAttempts)
	}
	count, err := fx.q.RecordCompletionAttempt(fx.ctx, store.RecordCompletionAttemptParams{
		RunID: id, WorkerID: wid, ClaimGeneration: oldGen,
		Unmet: []byte(`["M2"]`), Head: pgtype.Text{String: "historical-head", Valid: true},
	})
	if err != nil || count != 1 {
		t.Fatalf("RecordCompletionAttempt at N = (%d, %v), want (1, nil)", count, err)
	}
	// Snapshot the actual log rows, including identity and timestamps, so a reset
	// followed by a replacement attempt cannot masquerade as preserved history.
	attemptLog := func() []byte {
		t.Helper()
		var log []byte
		if err := fx.pool.QueryRow(fx.ctx, `SELECT COALESCE(
			jsonb_agg(to_jsonb(a) ORDER BY a.id), '[]'::jsonb)
			FROM run_completion_attempts a WHERE a.run_id=$1`, id).Scan(&log); err != nil {
			t.Fatal(err)
		}
		return log
	}
	history := attemptLog()
	if bytes.Equal(history, []byte("[]")) {
		t.Fatal("RecordCompletionAttempt did not persist an attempt row")
	}
	original := fx.mustRun(id)
	assertHistory := func(r store.Run) {
		t.Helper()
		if r.CompletionAttempts != 1 {
			t.Fatalf("completion_attempts = %d, want historical count 1", r.CompletionAttempts)
		}
		if !bytes.Equal(r.LatestCompletionAttempt, original.LatestCompletionAttempt) {
			t.Fatal("latest_completion_attempt changed without a new attempt")
		}
		if !bytes.Equal(attemptLog(), history) {
			t.Fatal("historical completion attempt rows changed")
		}
	}

	if r, err := fx.q.SetRunCompletionHold(fx.ctx, store.SetRunCompletionHoldParams{
		ID: id, WorkerID: wid, ClaimGeneration: oldGen,
	}); err != nil || r.Status != "paused" || r.HoldReason.String != "completion_blocked" {
		t.Fatalf("initial completion hold = (%s, %s, %v), want paused/completion_blocked",
			r.Status, r.HoldReason.String, err)
	}
	resumed, err := fx.q.ResumePausedRun(fx.ctx, store.ResumePausedRunParams{
		ID: id, UserID: fx.userID, AllowCompletionBlockedHold: true, GlobalTimeoutSeconds: 60,
	})
	if err != nil || resumed.Status != "queued" {
		t.Fatalf("ResumePausedRun = (%s, %v), want queued", resumed.Status, err)
	}
	assertHistory(fx.mustRun(id))
	reclaimed, err := fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		UserID: fx.userID, WorkerID: wid, Lane: "run",
		AffinityCutoff: wpAgo(30 * time.Minute), SpreadCutoff: wpAgo(9 * time.Second),
		HeartbeatCutoff: wpAgo(45 * time.Second), WorkerProtocolCaps: protocolCaps,
	})
	if err != nil {
		t.Fatalf("ClaimRun after resume: %v", err)
	}
	if reclaimed.ID != id || reclaimed.ClaimGeneration != oldGeneration+1 || reclaimed.WorkerID != wid {
		t.Fatal("reclaim must select the same run and worker at generation N+1")
	}
	assertHistory(reclaimed)
	if n, err := fx.q.SetRunRunning(fx.ctx, store.SetRunRunningParams{
		ID: id, WorkerID: wid,
	}); err != nil || n != 1 {
		t.Fatalf("SetRunRunning after reclaim = (%d, %v), want (1, nil)", n, err)
	}

	// Arm a new wall request on the reclaimed flight, without recording another
	// attempt. The stale call must be refused solely for its old generation.
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET pause_mode='wall',
		pause_requested_at=now(), pause_after_count=3 WHERE id=$1`, id)
	var inputID int64
	if err := fx.pool.QueryRow(fx.ctx, `INSERT INTO run_user_inputs (run_id, kind, body)
		VALUES ($1, 'pause', 'wall') RETURNING id`, id).Scan(&inputID); err != nil {
		t.Fatal(err)
	}
	readWall := func() [2]pgtype.Timestamptz {
		t.Helper()
		var stamps [2]pgtype.Timestamptz
		if err := fx.pool.QueryRow(fx.ctx, `SELECT consumed_at, applied_at
			FROM run_user_inputs WHERE id=$1`, inputID).Scan(&stamps[0], &stamps[1]); err != nil {
			t.Fatal(err)
		}
		return stamps
	}
	before := fx.mustRun(id)
	assertHistory(before)
	if before.Status != "running" || before.WorkerID != wid ||
		before.ClaimGeneration != oldGeneration+1 || before.ClaimReleasedAt.Valid ||
		!before.CompletionContractVersion.Valid || before.HoldReason.Valid ||
		!before.PauseMode.Valid || before.PauseMode.String != "wall" ||
		!before.PauseRequestedAt.Valid || !before.PauseAfterCount.Valid {
		t.Fatal("reclaimed run is not otherwise eligible for a completion hold with pending wall")
	}
	var pastDeadline bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT started_at + make_interval(secs =>
		budget_wall_seconds + budget_extension_seconds + budget_finalize_seconds
		+ budget_paused_seconds) < now() FROM runs WHERE id=$1`, id).Scan(&pastDeadline); err != nil {
		t.Fatal(err)
	}
	if !pastDeadline {
		t.Fatal("reclaimed run must still be past its wall deadline")
	}
	wallBefore := readWall()
	if wallBefore[0].Valid || wallBefore[1].Valid {
		t.Fatal("new wall input must be unconsumed and unapplied")
	}
	if _, err := fx.q.SetRunCompletionHold(fx.ctx, store.SetRunCompletionHoldParams{
		ID: id, WorkerID: wid, ClaimGeneration: oldGen,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale N completion hold = %v, want pgx.ErrNoRows", err)
	}
	if !reflect.DeepEqual(fx.mustRun(id), before) {
		t.Fatal("stale N completion hold changed the reclaimed run")
	}
	if got := readWall(); got != wallBefore {
		t.Fatal("stale N completion hold changed wall consumed_at or applied_at")
	}
	assertHistory(fx.mustRun(id))

	held, err := fx.q.SetRunCompletionHold(fx.ctx, store.SetRunCompletionHoldParams{
		ID: id, WorkerID: wid,
		ClaimGeneration: pgtype.Int8{Int64: oldGeneration + 1, Valid: true},
	})
	if err != nil {
		t.Fatalf("current N+1 completion hold: %v", err)
	}
	if held.Status != "paused" || !held.HoldReason.Valid || held.HoldReason.String != "completion_blocked" {
		t.Fatalf("current hold = %s/%s, want paused/completion_blocked", held.Status, held.HoldReason.String)
	}
	if held.PauseMode.Valid || held.PauseRequestedAt.Valid || held.PauseAfterCount.Valid {
		t.Fatal("current completion hold did not clear all wall pause columns")
	}
	assertHistory(held)
	assertHistory(fx.mustRun(id))
	if stamps := readWall(); !stamps[0].Valid || !stamps[1].Valid {
		t.Fatal("current completion hold must stamp wall consumed_at and applied_at")
	}
}
