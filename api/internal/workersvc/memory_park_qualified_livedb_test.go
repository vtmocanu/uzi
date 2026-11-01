package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3639: the qualified (settled, receipt stored) memory park through the real entry points
// only. The sibling file memory_park_settlement_livedb_test.go pins the SQL-level matrix; these
// tests walk a bound ephemeral worker through Service.Claim, the cooperative park, the receipt, the
// owner Resume and the successor claim, and then the owner/worker transitions that must not lift,
// re-charge or revive anything.

// claimableFor is CountOnlineWorkersClaimableForRun for runID, the count the queued-health resolver
// uses to decide whether a worker could claim the run.
func (f parkFixture) claimableFor(t *testing.T, runID uuid.UUID) int64 {
	t.Helper()
	now := time.Now()
	n, err := f.e.q.CountOnlineWorkersClaimableForRun(f.e.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID: runID, CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
		AffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, StaleRequeueCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		HeartbeatCutoff: pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true}, CrossCheckEvaluatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true}, CapabilityAware: true, EphemeralLease: pgtype.Interval{Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return n.Claimable
}

// claimAgrees claims for the worker and requires the claimable count read just before to predict
// exactly that outcome (1 claimable worker iff the claim yields the run).
func (f parkFixture) claimAgrees(t *testing.T, stage string) *ClaimPayload {
	t.Helper()
	claimable := f.claimableFor(t, f.b.RunID)
	p := f.claim(t, f.w.ID)
	if (claimable > 0) != (p != nil) {
		t.Fatalf("%s: claimable=%d but claim=%+v", stage, claimable, p)
	}
	if p != nil && p.RunID != f.b.RunID.String() {
		t.Fatalf("%s: claimed foreign run %s", stage, p.RunID)
	}
	return p
}

func (f parkFixture) countWhere(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.e.pool.QueryRow(f.e.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMemoryParkBoundEphemeralWorkerLivenessLiveDB(t *testing.T) {
	f := newParkFixture(t)
	// The worker is the run's own bound ephemeral worker, and the run has never been claimed: the
	// first claim is the production claim that establishes generation 2.
	f.e.exec(`UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', ephemeral=true, ephemeral_run_id=$2 WHERE id=$1`, f.w.ID, f.b.RunID)
	f.e.exec(`UPDATE runs SET status='queued', worker_id=NULL, claim_generation=1, claimed_at=NULL, started_at=NULL, status_since=now() WHERE id=$1`, f.b.RunID)
	// Foreign work the bound worker must never take, older than the bound run so it would win a
	// plain oldest-first pick.
	foreign := seedOutageRun(t, f.e, f.w.UserID, f.repo, f.w.ID, "queued", "issue", 1, 0)
	f.e.exec(`UPDATE runs SET worker_id=NULL, started_at=NULL, created_at=now() - interval '1 day' WHERE id=$1`, foreign)
	ephemerals := func() int {
		return f.countWhere(t, "SELECT count(*) FROM workers WHERE user_id=$1 AND ephemeral", f.w.UserID)
	}
	if ephemerals() != 1 || f.claimableFor(t, foreign) != 0 {
		t.Fatalf("setup: ephemeral workers=%d foreign claimable=%d", ephemerals(), f.claimableFor(t, foreign))
	}

	p := f.claimAgrees(t, "initial claim")
	if p == nil || p.ClaimGeneration != 2 || p.MemoryEpisode != 0 || p.MemoryParkReference != nil {
		t.Fatalf("initial claim payload: %+v", p)
	}
	if f.run(t).WorkerID != pgtypeUUID(f.w.ID) || statusOfRun(t, f, foreign) != "queued" {
		t.Fatalf("initial claim: run=%+v foreign=%s", f.run(t), statusOfRun(t, f, foreign))
	}
	f.reserve(t, f.b)
	held := f.hold(t)
	rel := f.release(t)
	if rel.WorkerID != f.w.ID || rel.RegisterNonce != f.b.RegisterNonce || rel.ClaimGeneration != 2 {
		t.Fatalf("park release row: %+v", rel)
	}
	holds := f.countWhere(t, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", f.b.RunID)

	if p := f.claimAgrees(t, "parked"); p != nil {
		t.Fatalf("parked run claimed: %+v", p)
	}
	raw := f.receipt(t, nil, f.b.InterventionID)
	f.mustSettle(t, raw)
	if p := f.claimAgrees(t, "settled but still parked"); p != nil {
		t.Fatalf("settled but parked run claimed: %+v", p)
	}
	f.mustResume(t)
	resumed := f.run(t)
	if resumed.Status != "queued" || resumed.MemoryEpisode != 1 || resumed.ReleasedWorkerID.Valid || counters(resumed) != counters(held) {
		t.Fatalf("lifted Resume row: %+v", resumed)
	}
	if f.claimableFor(t, f.b.RunID) != 1 || f.claimableFor(t, foreign) != 0 {
		t.Fatalf("after Resume: claimable run=%d foreign=%d", f.claimableFor(t, f.b.RunID), f.claimableFor(t, foreign))
	}

	p = f.claimAgrees(t, "successor claim")
	rel = f.release(t)
	if p == nil || p.ClaimGeneration != 3 || p.MemoryEpisode != 1 || p.MemoryParkReference == nil || *p.MemoryParkReference != wantRef(rel) {
		t.Fatalf("successor payload: %+v want ref %+v", p, wantRef(rel))
	}
	after := f.run(t)
	if after.MemoryEpisode != 1 || after.Status != "claimed" || after.WorkerID != pgtypeUUID(f.w.ID) || after.ReleasedWorkerID.Valid ||
		after.WorkerRecoveryEpisode != held.WorkerRecoveryEpisode || after.RequeueCount != held.RequeueCount ||
		after.BudgetWallSeconds != held.BudgetWallSeconds || after.BudgetExtensionSeconds != held.BudgetExtensionSeconds ||
		after.IterationCount != held.IterationCount || after.MemoryInterventionCount != 0 {
		t.Fatalf("successor claim changed accounting: %+v", after)
	}
	if got := f.countWhere(t, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", f.b.RunID); got != holds {
		t.Fatalf("custody holds %d -> %d", holds, got)
	}
	if ephemerals() != 1 || f.countWhere(t, "SELECT count(*) FROM workers WHERE ephemeral AND ephemeral_run_id=$1", f.b.RunID) != 1 {
		t.Fatalf("a second bound worker appeared: ephemeral workers=%d", ephemerals())
	}
	if statusOfRun(t, f, foreign) != "queued" || f.run2(t, foreign).WorkerID.Valid {
		t.Fatal("foreign work was claimed")
	}
	// Nothing else is left for the bound worker: the run is claimed and the foreign run is not its.
	// (The claimable count is an advisory worker-side count that ignores run status, so only the
	// claim itself is asserted here.)
	if p := f.claim(t, f.w.ID); p != nil {
		t.Fatalf("bound worker claimed again: %+v", p)
	}
}

func pgtypeUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func statusOfRun(t *testing.T, f parkFixture, id uuid.UUID) string { return f.run2(t, id).Status }

func (f parkFixture) run2(t *testing.T, id uuid.UUID) store.Run {
	t.Helper()
	r, err := f.e.q.GetRunByID(f.e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The owner and worker transitions that exist for a parked run, run against a SETTLED park. None of
// them is the memory Resume, so none may lift the exclusion, renew the episode or free the worker.
func TestMemoryParkSettledOwnerTransitionsDoNotLiftLiveDB(t *testing.T) {
	f := newParkFixture(t)
	f.hold(t)
	f.mustSettle(t, f.receipt(t, nil))
	before := f.run(t)
	same := func(name string) {
		t.Helper()
		got := f.run(t)
		if got.Status != "recovery_wait" || got.RecoveryWaitCause != before.RecoveryWaitCause || got.MemoryEpisode != before.MemoryEpisode ||
			got.ReleasedWorkerID != before.ReleasedWorkerID || got.ReleasedWorkerNonce != before.ReleasedWorkerNonce ||
			got.ClaimReleasedAt != before.ClaimReleasedAt || got.ClaimGeneration != before.ClaimGeneration ||
			counters(got) != counters(before) || f.release(t).LiftedAt.Valid {
			t.Fatalf("%s lifted or changed the settled park: %+v", name, got)
		}
	}
	svc := snapshotSvc(f.e, testParams())
	for _, tc := range []struct {
		name string
		do   func() error
	}{
		{"credential switch", func() error {
			_, err := svc.SetRunCredential(f.e.ctx, f.w.UserID, f.b.RunID, CredentialOverrideModeAuto, nil)
			if !errors.Is(err, ErrCredentialSwitchRaced) {
				t.Fatalf("credential switch = %v", err)
			}
			return nil
		}},
		{"early promote", func() error {
			n, err := f.e.q.PromoteRecoveryWaitRunNow(f.e.ctx, store.PromoteRecoveryWaitRunNowParams{ID: f.b.RunID, UserID: f.w.UserID})
			if n != 0 {
				t.Fatalf("early promote rows=%d", n)
			}
			return err
		}},
		{"paused resume", func() error {
			_, err := f.e.q.ResumePausedRun(f.e.ctx, store.ResumePausedRunParams{ID: f.b.RunID, UserID: f.w.UserID})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("paused resume = %v", err)
			}
			return nil
		}},
		{"wall extend resume", func() error {
			_, err := f.e.q.ExtendAndResumeWallPark(f.e.ctx, store.ExtendAndResumeWallParkParams{ID: f.b.RunID, UserID: f.w.UserID, Secs: 60, Cap: 3600,
				BaseWallUpperSeconds: 86400, FinalizeAllowanceSeconds: WallParkFinalizeSeconds, MaxWallSeconds: MaxRunWallSeconds})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("wall extend = %v", err)
			}
			return nil
		}},
		{"wall stop", func() error {
			_, err := f.e.q.StopWallPark(f.e.ctx, store.StopWallParkParams{ID: f.b.RunID, UserID: f.w.UserID, ScopeCeiling: 1})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("wall stop = %v", err)
			}
			return nil
		}},
		{"timer promote", func() error {
			rows, err := f.e.q.PromoteRecoveryWaitRuns(f.e.ctx, pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true})
			for _, r := range rows {
				if r.ID == f.b.RunID {
					t.Fatal("timer promoted a memory park")
				}
			}
			return err
		}},
		{"pause", func() error {
			_, err := f.e.q.CreatePauseInput(f.e.ctx, store.CreatePauseInputParams{ID: f.b.RunID, Mode: pgtype.Text{String: "now", Valid: true}})
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("pause = %v", err)
			}
			return nil
		}},
	} {
		if err := tc.do(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		same(tc.name)
		if p := f.claim(t, f.w.ID); p != nil {
			t.Fatalf("%s: settled parked run claimed: %+v", tc.name, p)
		}
		same(tc.name + " + claim")
	}
	// Positive control: the memory Resume is the one transition that does.
	f.mustResume(t)
	if !f.release(t).LiftedAt.Valid {
		t.Fatal("memory Resume did not lift the settled park")
	}
}

// A credential switch on the LIFTED, queued run (the owner changing the token between Resume and the
// successor claim) writes the override but leaves the release lifted: the same incarnation still
// claims and still names the same receipt.
func TestMemoryParkCredentialSwitchAfterLiftLiveDB(t *testing.T) {
	f := newParkFixture(t)
	f.hold(t)
	f.mustSettle(t, f.receipt(t, nil))
	f.mustResume(t)
	svc := snapshotSvc(f.e, testParams())
	var secret uuid.UUID
	if err := f.e.pool.QueryRow(f.e.ctx, "SELECT id FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", f.w.UserID).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetRunCredential(f.e.ctx, f.w.UserID, f.b.RunID, CredentialOverrideModePinned, &secret); err != nil {
		t.Fatalf("credential switch on the lifted queued run: %v", err)
	}
	rel := f.release(t)
	if !rel.LiftedAt.Valid || rel.SupersededAt.Valid || f.run(t).Status != "queued" {
		t.Fatalf("switch disturbed the lift: rel=%+v run=%+v", rel, f.run(t))
	}
	p := f.claim(t, f.w.ID)
	if p == nil || p.MemoryParkReference == nil || *p.MemoryParkReference != wantRef(rel) {
		t.Fatalf("claim after switch: %+v", p)
	}
}

// A qualified park of a cross-check CHECKER: the receipt is stored, but the owner Resume still
// answers to the parent/checker fences. An ineligible parent keeps the park (no row, nothing lifted,
// the run still parked); the eligible parent lifts it.
func TestMemoryParkCheckerFencesQualifiedLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string // run against the lead (parent) run, or cross_checks by lead_run_id
		ok   bool
	}{
		{"eligible parent", "", true},
		{"checker deadline passed", "UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE lead_run_id=$1", false},
		{"parent released", "UPDATE runs SET claim_released_at=now() WHERE id=$1", false},
		{"parent claim moved on", "UPDATE runs SET claim_generation=3 WHERE id=$1", false},
		{"parent no longer active", "UPDATE runs SET status='awaiting_approval' WHERE id=$1", false},
		{"attempt decided", "UPDATE cross_checks SET verdict='approve' WHERE lead_run_id=$1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mf, lead := memoryCheckerFixture(t)
			f := parkFixture{mf}
			f.e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.w.ID, parkCaps)
			enableClaimAssembly(t, f.e, f.w.UserID)
			f.w = wkrRow(t, f.e, f.w.ID)
			f.reserve(t, f.b)
			f.hold(t)
			f.mustSettle(t, f.receipt(t, nil, f.b.InterventionID))
			if tc.sql != "" {
				f.e.exec(tc.sql, lead)
			}
			err := f.resume(f.e.ctx)
			rel := f.release(t)
			got := f.run(t)
			if tc.ok {
				if err != nil || !rel.LiftedAt.Valid || got.Status != "queued" || got.MemoryEpisode != 1 || got.ReleasedWorkerID.Valid {
					t.Fatalf("eligible checker Resume: err=%v rel=%+v run=%+v", err, rel, got)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) || rel.LiftedAt.Valid || (got.Status != "recovery_wait" && got.Status != "cancelled") || got.MemoryEpisode != 0 ||
				!got.ReleasedWorkerID.Valid || rel.Settlement == nil {
				// cancelled: the parent-exit trigger already ended the checker, which also
				// leaves the stored receipt unlifted.
				t.Fatalf("ineligible checker Resume: err=%v rel.lifted=%v run=%+v", err, rel.LiftedAt.Valid, got)
			}
		})
	}
}

// After the same worker and nonce claim at G+1, everything the old generation can still send is
// refused and moves nothing: its state reports, its memory reservations (including mixed
// generation/episode pairs) and a replay of its receipt.
func TestMemoryParkStaleGenerationAfterSuccessorClaimLiveDB(t *testing.T) {
	f := newParkFixture(t)
	f.reserve(t, f.b)
	f.hold(t)
	raw := f.receipt(t, nil, f.b.InterventionID)
	f.mustSettle(t, raw)
	f.mustResume(t)
	p := f.claim(t, f.w.ID)
	if p == nil || p.ClaimGeneration != 3 || p.MemoryEpisode != 1 {
		t.Fatalf("successor claim: %+v", p)
	}
	before := f.run(t)
	relBefore := f.release(t)

	cause := recoveryCauseWorkerMemoryPressure
	oldGen, oldEpisode := f.b.ClaimGeneration, f.b.MemoryEpisode
	for name, req := range map[string]StateRequest{
		"old generation park":      {State: "recovery_wait", RecoveryCause: &cause, ClaimGeneration: &oldGen, RegisterNonce: f.b.RegisterNonce, MemoryEpisode: &oldEpisode},
		"old generation running":   {State: "running", ClaimGeneration: &oldGen, RegisterNonce: f.b.RegisterNonce},
		"old generation completed": {State: "completed", ClaimGeneration: &oldGen, RegisterNonce: f.b.RegisterNonce},
	} {
		run, applied, err := f.s.SetState(f.e.ctx, f.w, f.b.RunID, req)
		if applied || err == nil {
			t.Fatalf("%s: applied=%v err=%v run=%+v, want a refusal", name, applied, err, run)
		}
		if got := f.run(t); got.Status != before.Status || got.ClaimGeneration != before.ClaimGeneration || got.MemoryEpisode != before.MemoryEpisode ||
			got.ClaimReleasedAt.Valid || got.MemoryInterventionCount != before.MemoryInterventionCount || got.WorkerID != before.WorkerID {
			t.Fatalf("%s moved the successor: %+v", name, got)
		}
	}
	for name, b := range map[string]MemoryBinding{
		"old pair":                   {RunID: f.b.RunID, WorkerID: f.w.ID, RegisterNonce: f.b.RegisterNonce, ClaimGeneration: 2, MemoryEpisode: 0, InterventionID: uuid.New()},
		"old generation new episode": {RunID: f.b.RunID, WorkerID: f.w.ID, RegisterNonce: f.b.RegisterNonce, ClaimGeneration: 2, MemoryEpisode: 1, InterventionID: uuid.New()},
		"new generation old episode": {RunID: f.b.RunID, WorkerID: f.w.ID, RegisterNonce: f.b.RegisterNonce, ClaimGeneration: 3, MemoryEpisode: 0, InterventionID: uuid.New()},
	} {
		_, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{MemoryBinding: b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
		if !errors.Is(err, ErrMemoryStale) {
			t.Fatalf("%s: reserve = %v, want ErrMemoryStale", name, err)
		}
		if got := f.run(t); got.MemoryInterventionCount != before.MemoryInterventionCount {
			t.Fatalf("%s charged the successor: count %d", name, got.MemoryInterventionCount)
		}
	}
	// An exact replay of the old generation's own reservation charges nothing and no longer
	// authorizes an intervention.
	replay, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, MemoryReservationRequest{MemoryBinding: f.b, Policy: MemoryPolicy{Version: 1, MaxInterventions: 3}})
	if err != nil || replay.Authorizing || f.run(t).MemoryInterventionCount != before.MemoryInterventionCount {
		t.Fatalf("old reservation replay = %+v, %v", replay, err)
	}
	if _, err := f.settle(raw); !errors.Is(err, ErrMemoryStale) {
		t.Fatalf("old receipt replay = %v, want ErrMemoryStale", err)
	}
	if got := f.release(t); got.LiftedAt != relBefore.LiftedAt || got.SupersededAt != relBefore.SupersededAt || got.SettledAt != relBefore.SettledAt {
		t.Fatalf("stale traffic changed the release row: %+v -> %+v", relBefore, got)
	}
	// Positive control: the current pair still reserves.
	cur := MemoryBinding{RunID: f.b.RunID, WorkerID: f.w.ID, RegisterNonce: f.b.RegisterNonce, ClaimGeneration: 3, MemoryEpisode: 1, InterventionID: uuid.New()}
	f.reserve(t, cur)
	if got := f.run(t); got.MemoryInterventionCount != 1 {
		t.Fatalf("current reservation count = %d, want 1", got.MemoryInterventionCount)
	}
}
