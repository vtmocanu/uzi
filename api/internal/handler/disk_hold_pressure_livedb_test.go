package handler

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Keep this regression compilable on d0e300a5: only existing Service, handler,
// and query interfaces are used. No SQL write impersonates a successful claim.
// The baseline must stop at fourth_counted_report, before owner-resume fences.
func TestDiskHoldPressureCycleOwnerResumeLiveDB(t *testing.T) {
	fx := newEphemeralFixture(t, false)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := fx.pool.Exec(fx.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	pat, err := fx.box.Seal([]byte("fixture-forge-pat"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := fx.box.Seal([]byte("fixture-model-token"))
	if err != nil {
		t.Fatal(err)
	}
	exec("UPDATE forge_connections SET token_ciphertext=$2 WHERE user_id=$1", fx.userID, pat)
	secretID := uuid.New()
	exec("INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,$2,'anthropic_token','disk-cycle',true,$3,'master')", secretID, fx.userID, token)
	svc := workersvc.New(fx.q, fx.box, workersvc.Params{
		RunTimeout: 24 * time.Hour, RunWallCeiling: 24 * time.Hour,
		WorkerHeartbeatStale: time.Minute, RunDiskParkMax: 3,
		CustodyHoldLimit: defaultCustodyLimit(),
	})
	svc.SetVault(vault.NewPostgres(fx.box, fx.pool))
	svc.SetTxBeginner(fx.pool)
	h := &Handler{q: fx.q, pool: fx.pool, wsvc: svc}
	h.cfg.RunTimeout = 24 * time.Hour
	workerID := fx.onlineWorker("disk-cycle", false)
	exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", workerID, []string{capability.RecoveryArchiveV1})
	worker, err := fx.q.GetWorkerByID(fx.ctx, workerID)
	if err != nil {
		t.Fatal(err)
	}
	id := fx.queuedRun([]string{})
	read := func(runID uuid.UUID) store.Run {
		t.Helper()
		run, err := fx.q.GetRunByID(fx.ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	claim := func(runID uuid.UUID) store.Run {
		t.Helper()
		before := read(runID)
		payload, err := svc.Claim(fx.ctx, worker, nil)
		if err != nil || payload == nil || payload.RunID != runID.String() {
			t.Fatalf("real Service.Claim want=%s payload=%+v err=%v", runID, payload, err)
		}
		run := read(runID)
		if run.Status != "claimed" || run.ClaimGeneration != before.ClaimGeneration+1 || run.ClaimReleasedAt.Valid {
			t.Fatalf("claim did not open next flight: status=%s gen=%d previous=%d released=%v", run.Status, run.ClaimGeneration, before.ClaimGeneration, run.ClaimReleasedAt)
		}
		return run
	}
	running := func(runID uuid.UUID, generation int64) store.Run {
		t.Helper()
		run, applied, err := svc.SetState(fx.ctx, worker, runID, workersvc.StateRequest{State: "running", ClaimGeneration: &generation})
		if err != nil || !applied || run.Status != "running" {
			t.Fatalf("fresh running report gen=%d applied=%v status=%s err=%v", generation, applied, run.Status, err)
		}
		return run
	}
	cause := "data_volume_full"
	containsLatest := false
	report := func(generation int64) workersvc.StateRequest {
		return workersvc.StateRequest{State: "recovery_wait", RecoveryCause: &cause, ClaimGeneration: &generation, CheckpointContainsLatest: &containsLatest}
	}
	park := func(generation int64) store.Run {
		t.Helper()
		run, applied, err := svc.SetState(fx.ctx, worker, id, report(generation))
		if err != nil || !applied {
			t.Fatalf("counted disk report gen=%d applied=%v err=%v", generation, applied, err)
		}
		return run
	}
	// Each retry really claims and starts the run. Advancing the timer's input
	// avoids real sleeps, while retaining the production promotion predicate.
	run := claim(id)
	run = running(id, run.ClaimGeneration)
	for n := int32(1); n <= 3; n++ {
		run = park(run.ClaimGeneration)
		if run.Status != "recovery_wait" || run.RecoveryWaitCause.String != cause ||
			!run.RecoveryRetryNotBefore.Valid || run.DiskParkCount != n {
			t.Fatalf("finite_counted_park_%d: status=%s cause=%v retry=%v count=%d", n, run.Status, run.RecoveryWaitCause, run.RecoveryRetryNotBefore, run.DiskParkCount)
		}
		if _, err := fx.q.PromoteRecoveryWaitRuns(fx.ctx, pgtype.Timestamptz{Time: run.RecoveryRetryNotBefore.Time.Add(time.Second), Valid: true}); err != nil {
			t.Fatal(err)
		}
		if read(id).Status != "queued" {
			t.Fatalf("finite_counted_park_%d was not promoted", n)
		}
		run = claim(id)
		run = running(id, run.ClaimGeneration)
	}
	// Nonzero, distinct allowances discriminate an accidental reset at Resume.
	exec(`UPDATE runs SET session_id='disk-retained-session',checkpoint_tip=$2,checkpoint_tip_at=now(),
		checkpoint_contains_latest=false,started_at=now()-interval '5 minutes',budget_paused_seconds=37,
		budget_extension_seconds=123,budget_finalize_seconds=321,
		requeue_count=9,requeue_episode_baseline=5,worker_recovery_episode=2,
		memory_episode=3,retained_park_count=2,forge_park_count=1 WHERE id=$1`,
		id, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	// Snapshot exact IDs, states, provenance and recovery metadata, not just counts.
	textQuery := func(sql string, args ...any) string {
		t.Helper()
		var value string
		if err := fx.pool.QueryRow(fx.ctx, sql, args...).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	holds := func(maxGeneration int64) string {
		return textQuery(`SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY h.id),'[]'::jsonb)::text
			FROM recovery_custody_holds h WHERE run_id=$1 AND generation<=$2`, id, maxGeneration)
	}
	preserved := func() string {
		return textQuery(`SELECT jsonb_object_agg(key,value)::text FROM runs r,
			LATERAL jsonb_each(to_jsonb(r)) e WHERE r.id=$1 AND key=ANY($2::text[])`, id, []string{
			"worker_id", "session_id", "checkpoint_tip", "checkpoint_tip_at", "checkpoint_contains_latest",
			"checkpoint_attested_source", "checkpoint_coordinates", "source_publication_intent",
			"worker_recovery_evidence", "released_worker_id", "released_worker_nonce",
			"budget_wall_seconds", "budget_max_iterations", "budget_extension_seconds", "budget_finalize_seconds",
			"requeue_count", "requeue_episode_baseline", "worker_recovery_episode", "memory_episode",
			"retained_park_count", "forge_park_count", "disk_park_count",
		})
	}
	generation := run.ClaimGeneration
	beforeHolds, beforePreserved := holds(generation), preserved()
	var openGenerations []int64
	rows, err := fx.pool.Query(fx.ctx, "SELECT generation FROM recovery_custody_holds WHERE run_id=$1 AND state='open' ORDER BY generation", id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var g int64
		if err := rows.Scan(&g); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		openGenerations = append(openGenerations, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(openGenerations, []int64{1, 2, 3, 4}) {
		t.Fatalf("real claims must retain exact open generations: %v", openGenerations)
	}
	run = park(generation)
	// Fatal here is intentional: base fails for the defect, never for a later
	// interface or budget-fence expectation.
	if run.Status != "recovery_wait" || run.RecoveryWaitCause.String != cause ||
		run.RecoveryRetryNotBefore.Valid || run.DiskParkCount != 3 ||
		run.FailOrigin.Valid || run.FinishedAt.Valid {
		t.Fatalf("fourth_counted_report: status=%s cause=%v retry=%v count=%d origin=%v finished=%v; want owner-held recovery_wait/data_volume_full, NULL retry, count 3, no failure",
			run.Status, run.RecoveryWaitCause, run.RecoveryRetryNotBefore, run.DiskParkCount, run.FailOrigin, run.FinishedAt)
	}
	if run.ClaimGeneration != generation || holds(generation) != beforeHolds || preserved() != beforePreserved {
		t.Fatal("fourth_counted_report changed claim, exact custody, source metadata or allowances")
	}
	assertHeldUnchanged := func(before store.Run, phase string) {
		t.Helper()
		if after := read(id); !reflect.DeepEqual(before, after) || holds(generation) != beforeHolds {
			t.Fatalf("%s mutated held run or exact custody", phase)
		}
	}
	delayed := func(phase string, stale bool) {
		t.Helper()
		before := read(id)
		for _, state := range []string{"failed", "awaiting_input", "awaiting_followup", "completed", "recovery_wait"} {
			req := workersvc.StateRequest{State: state, ClaimGeneration: &generation}
			if state == "recovery_wait" {
				req = report(generation)
			}
			_, applied, err := svc.SetState(fx.ctx, worker, id, req)
			if applied || (stale && !errors.Is(err, workersvc.ErrStaleClaim)) || (!stale && err != nil) {
				t.Fatalf("%s delayed %s applied=%v err=%v", phase, state, applied, err)
			}
		}
		if !reflect.DeepEqual(before, read(id)) || holds(generation) != beforeHolds {
			t.Fatalf("%s delayed reports mutated run or original custody", phase)
		}
	}
	delayed("held", false)
	// Include a duplicate typed park (lost ACK) in the same no-op matrix above.
	exec("UPDATE runs SET status_since=now()-interval '2 minutes' WHERE id=$1", id)
	held := read(id)
	for opportunity := 0; opportunity < 3; opportunity++ {
		if _, err := fx.q.PromoteRecoveryWaitRuns(fx.ctx, pgtype.Timestamptz{Time: time.Now().Add(365 * 24 * time.Hour), Valid: true}); err != nil {
			t.Fatal(err)
		}
		n, err := fx.q.PromoteRecoveryWaitRunNow(fx.ctx, store.PromoteRecoveryWaitRunNowParams{ID: id, UserID: fx.userID})
		if err != nil || n != 0 {
			t.Fatalf("held early promotion rows=%d err=%v", n, err)
		}
		if err := svc.PromoteVaultLockedRecoveryWaitRuns(fx.ctx, fx.userID); err != nil {
			t.Fatal(err)
		}
		// Change the actual credential value, then exercise the public switch
		// interface. A token change cannot confer disk-hold continuation authority.
		rotated, err := fx.box.Seal([]byte("fixture-model-token-rotated"))
		if err != nil {
			t.Fatal(err)
		}
		exec("UPDATE user_secrets SET ciphertext=$2 WHERE id=$1", secretID, rotated)
		if _, err := svc.SetRunCredential(fx.ctx, fx.userID, id, workersvc.CredentialOverrideModePinned, &secretID); err == nil {
			t.Fatal("held token switch unexpectedly succeeded")
		}
		payload, err := svc.Claim(fx.ctx, worker, nil)
		if err != nil || payload != nil {
			t.Fatalf("held claim opportunity %d payload=%+v err=%v", opportunity, payload, err)
		}
		assertHeldUnchanged(held, "clock/promoter/token/claim")
	}
	// Positive capacity control: this same worker actually claims another run
	// while the disk hold remains inert. No global admission refusal can pass.
	controlID := fx.queuedRun([]string{})
	control := claim(controlID)
	control = running(controlID, control.ClaimGeneration)
	assertHeldUnchanged(held, "actually_claimed_capacity_control")
	if rec := recoveryResumeRequest(h, id, uuid.New()); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign Resume=%d body=%s", rec.Code, rec.Body.String())
	}
	assertHeldUnchanged(held, "foreign Resume")
	// Two bodyless authenticated owner requests contend for exactly one hold.
	start := make(chan struct{})
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			codes <- recoveryResumeRequest(h, id, fx.userID).Code
		}()
	}
	close(start)
	a, b := <-codes, <-codes
	if (a != http.StatusOK || b != http.StatusConflict) && (b != http.StatusOK || a != http.StatusConflict) {
		t.Fatalf("concurrent same-hold Resume=%d/%d want 200/409", a, b)
	}
	queued := read(id)
	credit := int32(queued.StatusSince.Time.Sub(held.StatusSince.Time).Seconds() + 0.5)
	if queued.Status != "queued" || !queued.ClaimReleasedAt.Valid ||
		queued.ClaimGeneration != generation || queued.RecoveryWaitCause.Valid ||
		queued.StartedAt != held.StartedAt || !held.StartedAt.Valid ||
		credit < 120 || queued.BudgetPausedSeconds != held.BudgetPausedSeconds+credit ||
		preserved() != beforePreserved || holds(generation) != beforeHolds ||
		queued.RecoveryWaitCount != held.RecoveryWaitCount {
		t.Fatalf("owner Resume changed allowances/source or missed wall credit: status=%s gen=%d credit=%d paused=%d previous=%d",
			queued.Status, queued.ClaimGeneration, credit, queued.BudgetPausedSeconds, held.BudgetPausedSeconds)
	}
	audits := textQuery("SELECT count(*)::text FROM run_user_inputs WHERE run_id=$1 AND kind='resume'", id)
	if audits != "1" {
		t.Fatalf("same-hold owner Resume audit count=%s want 1", audits)
	}
	if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate Resume=%d", rec.Code)
	}
	delayed("queued interval", true)
	run = claim(id)
	if run.ClaimGeneration != generation+1 {
		t.Fatalf("owner Resume claim gen=%d want %d", run.ClaimGeneration, generation+1)
	}
	delayed("G+1", true)
	run = running(id, run.ClaimGeneration)
	delayed("G+1 running", true)
	if payload, err := svc.Claim(fx.ctx, worker, nil); err != nil || payload != nil {
		t.Fatalf("Resume supplied more than one claim: payload=%+v err=%v", payload, err)
	}
	run = park(run.ClaimGeneration)
	if run.Status != "recovery_wait" || run.RecoveryRetryNotBefore.Valid ||
		run.DiskParkCount != 3 || run.FailOrigin.Valid || run.FinishedAt.Valid ||
		holds(generation) != beforeHolds {
		t.Fatalf("fresh G+1 counted rehold changed cap or custody: status=%s count=%d retry=%v", run.Status, run.DiskParkCount, run.RecoveryRetryNotBefore)
	}
	// Cancellation wins over a counted disk report on an actually running flight.
	if _, err := svc.SubmitInput(fx.ctx, fx.userID, controlID, "cancel", "", nil); err != nil {
		t.Fatal(err)
	}
	cancelledControl, applied, err := svc.SetState(fx.ctx, worker, controlID, report(control.ClaimGeneration))
	if err != nil || !applied || cancelledControl.Status != "cancelled" ||
		cancelledControl.DiskParkCount != 0 || cancelledControl.FailOrigin.Valid {
		t.Fatalf("cancel precedence status=%s applied=%v count=%d err=%v", cancelledControl.Status, applied, cancelledControl.DiskParkCount, err)
	}
	// A genuinely exhausted execution budget cannot be revived by hold-time
	// credit. Clock/allowance setup changes no status or claim ownership.
	exec(`UPDATE runs SET started_at=now()-interval '2 hours',status_since=now(),
		budget_wall_seconds=1,budget_extension_seconds=0,budget_finalize_seconds=0,
		budget_paused_seconds=0 WHERE id=$1`, id)
	spent := read(id)
	if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusConflict {
		t.Fatalf("wall-ineligible Resume=%d body=%s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(spent, read(id)) || holds(generation) != beforeHolds {
		t.Fatal("wall-ineligible Resume mutated run or retained custody")
	}
	// A held run has no poller; owner cancellation must finish server-side.
	result, err := svc.SubmitInput(fx.ctx, fx.userID, id, "cancel", "", nil)
	final := read(id)
	if err != nil || !result.ServerSide || final.Status != "cancelled" || !final.FinishedAt.Valid || final.FailOrigin.Valid || final.DiskParkCount != 3 {
		t.Fatalf("final owner cancel status=%s serverSide=%v err=%v", final.Status, result.ServerSide, err)
	}
	if rec := recoveryResumeRequest(h, id, fx.userID); rec.Code != http.StatusConflict {
		t.Fatalf("cancelled Resume=%d", rec.Code)
	}
	if payload, err := svc.Claim(fx.ctx, worker, nil); err != nil || payload != nil {
		t.Fatalf("cancelled run woke: payload=%+v err=%v", payload, err)
	}
}
