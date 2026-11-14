package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4206: the hibernation cache prune attempt over a real database. Skipped without
// UZI_TEST_DATABASE_URL (run via ./e2e/run-store-it.sh).

// dockerWorker seeds an idle hosted Docker worker that advertises hibernation_cache_prune_v1.
func (f *hibFx) dockerWorker() (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	id, pod := f.worker()
	f.env.exec(`UPDATE workers SET docker_enabled = true, protocol_capabilities = $2, snapshot_register_nonce = $3 WHERE id = $1`,
		id, []string{capability.HibernationCachePruneV1}, "nonce-"+uuid.NewString())
	return id, pod
}

func (f *hibFx) cleanupOp(id uuid.UUID) string {
	return uuid.UUID(f.row(id).CleanupOpID.Bytes).String()
}

// rep is a report bound to the worker's current attempt.
func (f *hibFx) rep(id uuid.UUID, kind string) HibernationCachePruneReport {
	w := f.row(id)
	return HibernationCachePruneReport{OpID: uuid.UUID(w.CleanupOpID.Bytes).String(), RegisterNonce: w.CleanupRegisterNonce.String, Kind: kind}
}

func (f *hibFx) readyRep(id uuid.UUID) HibernationCachePruneReport {
	r := f.rep(id, CleanupReportReady)
	zero, at := 0, time.Now()
	r.LocalClaims, r.LocalExecutions, r.CustodyClear, r.CustodyCheckedAt, r.GateExclusive = &zero, &zero, true, &at, true
	return r
}

func okSteps() []HibernationCachePruneStep {
	return []HibernationCachePruneStep{
		{Command: CleanupCommandImagePruneDangling, State: CleanupStepExitedOK},
		{Command: CleanupCommandBuilderPrune, State: CleanupStepExitedOK},
	}
}

func (f *hibFx) resultRep(id uuid.UUID, exclusive bool, steps []HibernationCachePruneStep) HibernationCachePruneReport {
	r := f.rep(id, CleanupReportResult)
	r.Steps, r.GateExclusiveThroughout = steps, exclusive
	return r
}

func (f *hibFx) mustReport(id uuid.UUID, r HibernationCachePruneReport) store.Worker {
	f.t.Helper()
	w, err := f.svc.ReportHibernationCachePrune(f.env.ctx, id, r)
	if err != nil {
		f.t.Fatalf("report %s: %v", r.Kind, err)
	}
	return w
}

func (f *hibFx) wantPhase(id uuid.UUID, phase string) store.Worker {
	f.t.Helper()
	w := f.row(id)
	if w.CleanupPhase != phase {
		f.t.Fatalf("cleanup phase = %q, want %q", w.CleanupPhase, phase)
	}
	return w
}

func (f *hibFx) wantOutcome(id uuid.UUID, outcome, reason string) {
	f.t.Helper()
	w := f.row(id)
	if w.HibernationCleanup.String != outcome || w.HibernationCleanupReason.String != reason {
		f.t.Fatalf("outcome = %q/%q, want %q/%q", w.HibernationCleanup.String, w.HibernationCleanupReason.String, outcome, reason)
	}
}

// started drives a docker worker to a started attempt and returns its id and pod.
func (f *hibFx) started() (uuid.UUID, uuid.UUID) {
	f.t.Helper()
	id, pod := f.dockerWorker()
	f.hibernate(id)
	f.wantPhase(id, CleanupPhaseRequested)
	f.mustReport(id, f.readyRep(id))
	f.wantPhase(id, CleanupPhaseStarted)
	return id, pod
}

func (f *hibFx) pass() {
	f.t.Helper()
	if _, err := f.svc.HibernationSettlePass(f.env.ctx); err != nil {
		f.t.Fatalf("settle pass: %v", err)
	}
}

func (f *hibFx) wantCleanupConflict(id uuid.UUID, r HibernationCachePruneReport) {
	f.t.Helper()
	before := f.row(id)
	if _, err := f.svc.ReportHibernationCachePrune(f.env.ctx, id, r); !errors.Is(err, ErrHibernationCleanupConflict) {
		f.t.Fatalf("report %s = %v, want a conflict", r.Kind, err)
	}
	after := f.row(id)
	if before.CleanupPhase != after.CleanupPhase || before.HibernationState != after.HibernationState ||
		before.HibernationCleanup != after.HibernationCleanup || before.CleanupReconcileSeq != after.CleanupReconcileSeq ||
		before.CleanupSettledAt != after.CleanupSettledAt || before.HibernationOpID != after.HibernationOpID {
		f.t.Fatalf("a rejected report changed the row:\n before %+v\n after  %+v", before, after)
	}
}

func (f *hibFx) wantTransitionConflict(id uuid.UUID, t HibernationTransition) {
	f.t.Helper()
	if _, err := f.apply(id, t); !errors.Is(err, ErrHibernationConflict) {
		f.t.Fatalf("%s = %v, want ErrHibernationConflict", t.Transition, err)
	}
}

func (f *hibFx) cleanupStopped(id, pod uuid.UUID) HibernationTransition {
	return HibernationTransition{OpID: f.op(id), Transition: TransitionCleanupStopped, DeploymentUID: f.deploy,
		PodUID: pod.String(), CleanupOpID: f.cleanupOp(id)}
}

// Admission: Docker with the capability gets an attempt, Docker without records unsupported, a
// plain worker gets nothing.
func TestHibernationCleanupAdmissionLiveDB(t *testing.T) {
	f := newHibFx(t)
	t.Run("docker with the capability gets a requested attempt bound to the registration", func(t *testing.T) {
		id, _ := f.dockerWorker()
		nonce := f.row(id).SnapshotRegisterNonce.String
		f.hibernate(id)
		w := f.wantPhase(id, CleanupPhaseRequested)
		if !w.CleanupOpID.Valid || w.CleanupRegisterNonce.String != nonce || !w.CleanupRequestedAt.Valid || w.HibernationCleanup.Valid {
			t.Fatalf("requested attempt = %+v", w)
		}
		if p := HibernationCachePruneFromWorker(w); p == nil || p.Phase != CleanupPhaseRequested || p.RegisterNonce != nonce || p.CancelRequested {
			t.Fatalf("projection = %+v", p)
		}
	})
	t.Run("docker without the capability is skipped as unsupported with no attempt", func(t *testing.T) {
		id, _ := f.worker()
		f.env.exec(`UPDATE workers SET docker_enabled = true WHERE id = $1`, id)
		f.hibernate(id)
		f.wantPhase(id, CleanupPhaseNone)
		f.wantOutcome(id, HibernationCleanupSkipped, CleanupReasonUnsupported)
		if f.row(id).CleanupOpID.Valid || HibernationCachePruneFromWorker(f.row(id)) != nil {
			t.Fatal("an unsupported worker was issued an attempt")
		}
		f.mustApply(id, TransitionScalingDown, uuid.New()) // hibernation proceeds as before
	})
	t.Run("a plain worker has no attempt and no outcome", func(t *testing.T) {
		id, _ := f.worker()
		f.hibernate(id)
		w := f.wantPhase(id, CleanupPhaseNone)
		if w.HibernationCleanup.Valid || w.CleanupOpID.Valid {
			t.Fatalf("plain worker row = %+v", w)
		}
	})
}

// The prepare phase waits for a confirmed settlement, then proceeds.
func TestHibernationCleanupPrepareWaitsThenProceedsLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, pod := f.started()
	// Scale-down and every other fence-reopening step is refused while unsettled.
	f.wantTransitionConflict(id, f.tr(id, TransitionScalingDown, pod))
	f.wantTransitionConflict(id, f.tr(id, TransitionScaledDown, uuid.Nil))
	f.wantTransitionConflict(id, f.tr(id, TransitionRefused, uuid.Nil))
	f.want(id, "preparing")
	if _, err := f.svc.HibernateWorker(f.env.ctx, f.actor, id, ""); refusalCode(err) != RefusalAlreadyActive {
		t.Fatalf("overlapping hibernate = %v", err)
	}
	f.mustReport(id, f.resultRep(id, true, okSteps()))
	f.wantPhase(id, CleanupPhaseSettled)
	f.wantOutcome(id, HibernationCleanupPruned, "")
	f.want(id, "preparing")
	f.mustApply(id, TransitionScalingDown, pod)
	f.mustApply(id, TransitionScaledDown, uuid.Nil)
	f.want(id, "hibernated")
}

func TestHibernationCleanupRefusalsLiveDB(t *testing.T) {
	f := newHibFx(t)
	for _, reason := range []string{CleanupReasonDaemonUnreachable, CleanupReasonCancelled} {
		t.Run("non-safety "+reason+" hibernates", func(t *testing.T) {
			id, pod := f.dockerWorker()
			f.hibernate(id)
			r := f.rep(id, CleanupReportRefused)
			r.Reason = reason
			f.mustReport(id, r)
			f.wantPhase(id, CleanupPhaseSettled)
			f.wantOutcome(id, HibernationCleanupSkipped, reason)
			f.want(id, "preparing")
			f.mustApply(id, TransitionScalingDown, pod)
			f.mustApply(id, TransitionScaledDown, uuid.Nil)
			f.want(id, "hibernated")
		})
	}
	for _, reason := range []string{CleanupReasonCustodyStale, CleanupReasonActiveWork, CleanupReasonClaimInFlight, CleanupReasonGateNotExclusive} {
		t.Run("safety "+reason+" withdraws", func(t *testing.T) {
			id, _ := f.dockerWorker()
			f.hibernate(id)
			r := f.rep(id, CleanupReportRefused)
			r.Reason = reason
			f.mustReport(id, r)
			f.wantPhase(id, CleanupPhaseSettled)
			f.wantOutcome(id, HibernationCleanupSkipped, reason)
			f.want(id, "waking")
		})
	}
	t.Run("an unknown refusal reason is rejected", func(t *testing.T) {
		id, _ := f.dockerWorker()
		f.hibernate(id)
		r := f.rep(id, CleanupReportRefused)
		r.Reason = "bored"
		f.wantCleanupConflict(id, r)
	})
}

func TestHibernationCleanupStartFenceLiveDB(t *testing.T) {
	f := newHibFx(t)
	mutate := map[string]func(r *HibernationCachePruneReport){
		"claims running":    func(r *HibernationCachePruneReport) { one := 1; r.LocalClaims = &one },
		"executions":        func(r *HibernationCachePruneReport) { one := 1; r.LocalExecutions = &one },
		"missing claims":    func(r *HibernationCachePruneReport) { r.LocalClaims = nil },
		"custody not clear": func(r *HibernationCachePruneReport) { r.CustodyClear = false },
		"gate not exclusive": func(r *HibernationCachePruneReport) {
			r.GateExclusive = false
		},
		"custody evidence stale": func(r *HibernationCachePruneReport) {
			old := time.Now().Add(-2 * time.Minute)
			r.CustodyCheckedAt = &old
		},
		"custody evidence from the future": func(r *HibernationCachePruneReport) {
			future := time.Now().Add(time.Minute)
			r.CustodyCheckedAt = &future
		},
		"custody evidence before the request": func(r *HibernationCachePruneReport) {
			early := time.Now().Add(-time.Hour)
			r.CustodyCheckedAt = &early
		},
		"missing custody evidence": func(r *HibernationCachePruneReport) { r.CustodyCheckedAt = nil },
	}
	for name, m := range mutate {
		t.Run(name+" does not start", func(t *testing.T) {
			id, _ := f.dockerWorker()
			f.hibernate(id)
			r := f.readyRep(id)
			m(&r)
			f.wantCleanupConflict(id, r)
			f.wantPhase(id, CleanupPhaseRequested)
		})
	}
	t.Run("server checks failing at ready settle skipped and withdraw", func(t *testing.T) {
		for name, tc := range map[string]struct {
			prep   func(id uuid.UUID)
			reason string
		}{
			"active work": {func(id uuid.UUID) {
				run := f.queuedRun()
				f.env.exec(`UPDATE runs SET worker_id = $2, status = 'running' WHERE id = $1`, run, id)
			}, CleanupReasonActiveWork},
			"pending wake demand": {func(id uuid.UUID) {
				run := f.queuedRun()
				f.env.exec(`INSERT INTO hibernation_wakes (run_id, worker_id, first_woken_at) VALUES ($1, $2, now())`, run, id)
			}, CleanupReasonPendingWakeDemand},
			"maintenance": {func(id uuid.UUID) {
				f.env.exec(`UPDATE workers SET maintenance_fenced = true WHERE id = $1`, id)
			}, CleanupReasonMaintenanceActive},
		} {
			id, _ := f.dockerWorker()
			f.hibernate(id)
			tc.prep(id)
			f.mustReport(id, f.readyRep(id))
			f.wantPhase(id, CleanupPhaseSettled)
			f.wantOutcome(id, HibernationCleanupSkipped, tc.reason)
			f.want(id, "waking")
			_ = name
		}
	})
	t.Run("cancellation requested before ready blocks the start", func(t *testing.T) {
		id, _ := f.dockerWorker()
		f.hibernate(id)
		r := f.readyRep(id)
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		f.wantPhase(id, CleanupPhaseSettled) // a requested attempt settles at once
		f.wantOutcome(id, HibernationCleanupSkipped, CleanupReasonCancelled)
		f.want(id, "waking")
		// A ready for a settled attempt is only a replay, never a start.
		f.mustReport(id, r)
		f.wantPhase(id, CleanupPhaseSettled)
	})
	t.Run("started is recorded with the registered pod", func(t *testing.T) {
		id, pod := f.started()
		w := f.row(id)
		if !w.CleanupStartedAt.Valid || w.CleanupPodUid.Bytes != pod {
			t.Fatalf("started row = %+v", w)
		}
		// A replayed ready is a no-op.
		f.mustReport(id, f.readyRep(id))
		f.wantPhase(id, CleanupPhaseStarted)
	})
}

func TestHibernationCleanupResultSettlementLiveDB(t *testing.T) {
	f := newHibFx(t)
	t.Run("gate not exclusive throughout withdraws", func(t *testing.T) {
		id, _ := f.started()
		f.mustReport(id, f.resultRep(id, false, okSteps()))
		f.wantPhase(id, CleanupPhaseSettled)
		f.wantOutcome(id, HibernationCleanupPruned, "")
		f.want(id, "waking")
	})
	t.Run("all steps not started settles skipped and hibernation proceeds", func(t *testing.T) {
		id, pod := f.started()
		f.mustReport(id, f.resultRep(id, true, []HibernationCachePruneStep{
			{Command: CleanupCommandImagePruneDangling, State: CleanupStepNotStarted},
			{Command: CleanupCommandBuilderPrune, State: CleanupStepNotStarted},
		}))
		f.wantOutcome(id, HibernationCleanupSkipped, CleanupReasonCancelled)
		f.want(id, "preparing")
		f.mustApply(id, TransitionScalingDown, pod)
	})
	t.Run("new custody or work evidence at result time re-runs admission and withdraws", func(t *testing.T) {
		id, _ := f.started()
		run := f.queuedRun()
		f.env.exec(`UPDATE runs SET worker_id = $2, status = 'running' WHERE id = $1`, run, id)
		f.mustReport(id, f.resultRep(id, true, okSteps()))
		f.wantPhase(id, CleanupPhaseSettled)
		f.wantOutcome(id, HibernationCleanupPruned, "")
		f.want(id, "waking")
	})
	t.Run("a step reported unknown keeps the fence", func(t *testing.T) {
		id, _ := f.started()
		f.mustReport(id, f.resultRep(id, true, []HibernationCachePruneStep{
			{Command: CleanupCommandImagePruneDangling, State: CleanupStepExitedOK},
			{Command: CleanupCommandBuilderPrune, State: CleanupStepUnknown},
		}))
		w := f.wantPhase(id, CleanupPhaseUnknown)
		if !w.CleanupUnknownAt.Valid || w.HibernationCleanup.Valid {
			t.Fatalf("unknown row = %+v", w)
		}
		f.want(id, "preparing")
		// A late, confirmed result for the same op and nonce is identity-bound evidence.
		f.mustReport(id, f.resultRep(id, true, okSteps()))
		f.wantPhase(id, CleanupPhaseSettled)
		f.wantOutcome(id, HibernationCleanupPruned, "")
	})
	t.Run("a malformed result is rejected", func(t *testing.T) {
		id, _ := f.started()
		f.wantCleanupConflict(id, f.resultRep(id, true, okSteps()[:1]))
		f.wantCleanupConflict(id, f.resultRep(id, true, []HibernationCachePruneStep{{Command: "system_prune", State: CleanupStepExitedOK}, {Command: CleanupCommandBuilderPrune, State: CleanupStepExitedOK}}))
		f.wantCleanupConflict(id, f.resultRep(id, true, []HibernationCachePruneStep{{Command: CleanupCommandBuilderPrune, State: "done"}, {Command: CleanupCommandImagePruneDangling, State: CleanupStepExitedOK}}))
		f.wantPhase(id, CleanupPhaseStarted)
	})
	t.Run("a result before the start is rejected", func(t *testing.T) {
		id, _ := f.dockerWorker()
		f.hibernate(id)
		f.wantCleanupConflict(id, f.resultRep(id, true, okSteps()))
	})
	t.Run("a replayed result after settlement is a no-op", func(t *testing.T) {
		id, _ := f.started()
		f.mustReport(id, f.resultRep(id, true, okSteps()))
		before := f.row(id)
		f.mustReport(id, f.resultRep(id, true, okSteps()))
		if f.row(id).CleanupSettledAt != before.CleanupSettledAt {
			t.Fatal("a replay moved settled_at")
		}
	})
}

func TestHibernationCleanupStaleAndMismatchedReportsLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.dockerWorker()
	f.hibernate(id)
	oldOp := f.cleanupOp(id)
	for name, mut := range map[string]func(r *HibernationCachePruneReport){
		"other op":    func(r *HibernationCachePruneReport) { r.OpID = uuid.NewString() },
		"other nonce": func(r *HibernationCachePruneReport) { r.RegisterNonce = "nonce-other" },
		"no nonce":    func(r *HibernationCachePruneReport) { r.RegisterNonce = "" },
		"bad op":      func(r *HibernationCachePruneReport) { r.OpID = "x" },
		"bad kind":    func(r *HibernationCachePruneReport) { r.Kind = "shrug" },
	} {
		r := f.readyRep(id)
		mut(&r)
		f.wantCleanupConflict(id, r)
		_ = name
	}
	f.wantPhase(id, CleanupPhaseRequested)

	// Overlapping attempts: finish and withdraw the first, then run a second hibernation; every
	// report for the first op is rejected and changes nothing.
	r := f.rep(id, CleanupReportRefused)
	r.Reason = CleanupReasonActiveWork
	f.mustReport(id, r)
	f.want(id, "waking")
	next := uuid.New()
	f.mustApply(id, TransitionReady, next)
	f.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, id, next)
	f.heartbeatNow(id)
	f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, id, []string{capability.HibernationCachePruneV1})
	f.register(id, &next)
	f.want(id, "none")
	f.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, id, []string{capability.HibernationCachePruneV1})
	f.hibernate(id)
	if f.cleanupOp(id) == oldOp {
		t.Fatal("the second hibernation reused the first attempt's op id")
	}
	stale := f.readyRep(id)
	stale.OpID = oldOp
	f.wantCleanupConflict(id, stale)
	staleResult := f.resultRep(id, true, okSteps())
	staleResult.OpID = oldOp
	f.wantCleanupConflict(id, staleResult)
	f.wantPhase(id, CleanupPhaseRequested)
}

func TestHibernationCleanupWakeKeepsFenceLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, pod := f.started()
	if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, "operator"); err != nil {
		t.Fatal(err)
	}
	w := f.row(id)
	if w.HibernationState != "waking" || w.CleanupPhase != CleanupPhaseStarted || !w.CleanupCancelRequested {
		t.Fatalf("wake of a started attempt: %q %q cancel=%v", w.HibernationState, w.CleanupPhase, w.CleanupCancelRequested)
	}
	if p := HibernationCachePruneFromWorker(w); p == nil || !p.CancelRequested || p.Phase != CleanupPhaseStarted {
		t.Fatalf("projection = %+v", p)
	}
	// The original pod reports ready (the controller never scaled down): that alone must not
	// settle while the attempt is unsettled, however many passes run.
	f.mustApply(id, TransitionReady, pod)
	f.heartbeatNow(id)
	f.pass()
	f.want(id, "waking")
	// Settlement of the attempt lets the next pass settle the wake.
	f.mustReport(id, f.resultRep(id, false, []HibernationCachePruneStep{
		{Command: CleanupCommandImagePruneDangling, State: CleanupStepExitedOK},
		{Command: CleanupCommandBuilderPrune, State: CleanupStepNotStarted},
	}))
	f.wantPhase(id, CleanupPhaseSettled)
	f.want(id, "waking")
	f.heartbeatNow(id)
	f.pass()
	f.want(id, "none")
}

func TestHibernationCleanupDemandWithdrawsLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.dockerWorker()
	f.hibernate(id)
	run := f.queuedRun()
	if res, err := f.svc.DemandWake(f.env.ctx, run, id); err != nil || res != DemandWakeWoke {
		t.Fatalf("demand on a preparing worker = %v, %v", res, err)
	}
	f.want(id, "waking")
	f.wantPhase(id, CleanupPhaseSettled)
	f.wantOutcome(id, HibernationCleanupSkipped, CleanupReasonCancelled)

	id2, _ := f.started()
	run2 := f.queuedRun()
	if res, err := f.svc.DemandWake(f.env.ctx, run2, id2); err != nil || res != DemandWakeWoke {
		t.Fatalf("demand on a started worker = %v, %v", res, err)
	}
	if w := f.row(id2); w.HibernationState != "waking" || w.CleanupPhase != CleanupPhaseStarted || !w.CleanupCancelRequested {
		t.Fatalf("started attempt under demand: %q %q cancel=%v", w.HibernationState, w.CleanupPhase, w.CleanupCancelRequested)
	}
}

func TestHibernationCleanupUnknownTimeoutAndReconcileLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.started()
	// A fresh started attempt is left alone.
	f.pass()
	f.wantPhase(id, CleanupPhaseStarted)
	// Ten minutes of silence makes it unknown, and the first pass bumps the reconcile sequence.
	f.env.exec(`UPDATE workers SET cleanup_started_at = now() - interval '11 minutes' WHERE id = $1`, id)
	f.pass()
	w := f.wantPhase(id, CleanupPhaseUnknown)
	if w.CleanupReconcileSeq != 1 || !w.CleanupReconciledAt.Valid || !w.CleanupUnknownAt.Valid {
		t.Fatalf("after timeout: seq=%d reconciled=%v unknown=%v", w.CleanupReconcileSeq, w.CleanupReconciledAt.Valid, w.CleanupUnknownAt.Valid)
	}
	// Reconciliation is paced at 5 minutes.
	f.pass()
	f.pass()
	if got := f.row(id).CleanupReconcileSeq; got != 1 {
		t.Fatalf("seq = %d after immediate passes, want 1", got)
	}
	f.env.exec(`UPDATE workers SET cleanup_reconciled_at = now() - interval '4 minutes' WHERE id = $1`, id)
	f.pass()
	if got := f.row(id).CleanupReconcileSeq; got != 1 {
		t.Fatalf("seq = %d at 4 minutes, want 1", got)
	}
	f.env.exec(`UPDATE workers SET cleanup_reconciled_at = now() - interval '5 minutes' WHERE id = $1`, id)
	f.pass()
	if got := f.row(id).CleanupReconcileSeq; got != 2 {
		t.Fatalf("seq = %d at 5 minutes, want 2", got)
	}
	// Still fenced and unsettled, however long it lasts.
	f.want(id, "preparing")
	f.wantPhase(id, CleanupPhaseUnknown)
}

func TestHibernationCleanupDeadlineLiveDB(t *testing.T) {
	f := newHibFx(t)
	t.Run("suspended while started or unknown", func(t *testing.T) {
		id, _ := f.started()
		f.env.exec(`UPDATE workers SET hibernation_requested_at = now() - interval '30 minutes' WHERE id = $1`, id)
		f.pass()
		f.want(id, "preparing")
		f.env.exec(`UPDATE workers SET cleanup_started_at = now() - interval '11 minutes' WHERE id = $1`, id)
		f.pass()
		f.wantPhase(id, CleanupPhaseUnknown)
		f.want(id, "preparing")
	})
	t.Run("a requested attempt past the deadline withdraws and cancels", func(t *testing.T) {
		id, _ := f.dockerWorker()
		f.hibernate(id)
		f.env.exec(`UPDATE workers SET hibernation_requested_at = now() - interval '30 minutes' WHERE id = $1`, id)
		f.pass()
		f.want(id, "waking")
		f.wantPhase(id, CleanupPhaseSettled)
		f.wantOutcome(id, HibernationCleanupSkipped, CleanupReasonCancelled)
	})
	t.Run("measured from the settlement", func(t *testing.T) {
		id, pod := f.started()
		f.env.exec(`UPDATE workers SET hibernation_requested_at = now() - interval '30 minutes' WHERE id = $1`, id)
		f.mustReport(id, f.resultRep(id, true, okSteps()))
		f.pass()
		f.want(id, "preparing") // settled just now: the controller gets a full window
		f.env.exec(`UPDATE workers SET cleanup_settled_at = now() - interval '11 minutes' WHERE id = $1`, id)
		f.pass()
		f.want(id, "waking")
		_ = pod
	})
}

func TestHibernationCleanupRegisterLiveDB(t *testing.T) {
	f := newHibFx(t)
	t.Run("a requested attempt settles skipped worker_restarted and hibernation proceeds", func(t *testing.T) {
		id, pod := f.dockerWorker()
		f.hibernate(id)
		f.register(id, nil)
		f.wantPhase(id, CleanupPhaseSettled)
		f.wantOutcome(id, HibernationCleanupSkipped, CleanupReasonWorkerRestarted)
		f.want(id, "preparing")
		f.mustApply(id, TransitionScalingDown, pod)
	})
	t.Run("a started attempt becomes unknown and keeps a preparing worker fenced", func(t *testing.T) {
		id, pod := f.started()
		f.register(id, nil)
		f.wantPhase(id, CleanupPhaseUnknown)
		f.want(id, "preparing")
		f.wantTransitionConflict(id, f.tr(id, TransitionScalingDown, pod))
	})
	t.Run("a started attempt under a wake stays waking, unknown, through a replacement pod", func(t *testing.T) {
		id, _ := f.started()
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		next := uuid.New()
		f.mustApply(id, TransitionReady, next)
		f.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, id, next)
		f.heartbeatNow(id)
		f.register(id, &next) // 200: the register itself succeeds
		w := f.row(id)
		if w.HibernationState != "waking" || w.CleanupPhase != CleanupPhaseUnknown || !w.CleanupCancelRequested {
			t.Fatalf("after register: %q %q cancel=%v", w.HibernationState, w.CleanupPhase, w.CleanupCancelRequested)
		}
		// A new pod uid and a new nonce are not proof the old command stopped.
		f.pass()
		f.pass()
		f.want(id, "waking")
		f.wantPhase(id, CleanupPhaseUnknown)
	})
	t.Run("a settled attempt is untouched by a later register", func(t *testing.T) {
		id, _ := f.started()
		f.mustReport(id, f.resultRep(id, true, okSteps()))
		f.register(id, nil)
		f.wantPhase(id, CleanupPhaseSettled)
		f.wantOutcome(id, HibernationCleanupPruned, "")
	})
}

func TestHibernationCleanupStoppedEvidenceLiveDB(t *testing.T) {
	f := newHibFx(t)
	toUnknown := func() (uuid.UUID, uuid.UUID) {
		id, pod := f.started()
		f.env.exec(`UPDATE workers SET cleanup_started_at = now() - interval '11 minutes' WHERE id = $1`, id)
		f.pass()
		f.wantPhase(id, CleanupPhaseUnknown)
		return id, pod
	}
	t.Run("a matching report settles failed/interrupted and is replayable", func(t *testing.T) {
		id, pod := toUnknown()
		tr := f.cleanupStopped(id, pod)
		if _, err := f.apply(id, tr); err != nil {
			t.Fatalf("cleanup_stopped: %v", err)
		}
		f.wantPhase(id, CleanupPhaseSettled)
		f.wantOutcome(id, HibernationCleanupFailed, CleanupReasonInterrupted)
		if _, err := f.apply(id, tr); err != nil {
			t.Fatalf("replayed cleanup_stopped: %v", err)
		}
		f.mustApply(id, TransitionScalingDown, pod)
	})
	t.Run("mismatched reports are rejected and change nothing", func(t *testing.T) {
		id, pod := toUnknown()
		before := f.row(id)
		bad := map[string]func(t *HibernationTransition){
			"other cleanup op": func(t *HibernationTransition) { t.CleanupOpID = uuid.NewString() },
			"other pod":        func(t *HibernationTransition) { t.PodUID = uuid.NewString() },
			"other hibernation op": func(t *HibernationTransition) {
				t.OpID = uuid.NewString()
			},
		}
		for name, mut := range bad {
			tr := f.cleanupStopped(id, pod)
			mut(&tr)
			f.wantTransitionConflict(id, tr)
			_ = name
		}
		if _, err := f.apply(id, HibernationTransition{OpID: f.op(id), Transition: TransitionCleanupStopped, DeploymentUID: f.deploy, PodUID: pod.String()}); err == nil {
			t.Fatal("a cleanup_stopped without cleanup_op_id was accepted")
		}
		after := f.row(id)
		if after.CleanupPhase != before.CleanupPhase || after.HibernationState != before.HibernationState {
			t.Fatal("a mismatched cleanup_stopped changed the row")
		}
	})
	t.Run("only an unknown attempt can be stopped", func(t *testing.T) {
		id, pod := f.started()
		f.wantTransitionConflict(id, f.cleanupStopped(id, pod))
		f.wantPhase(id, CleanupPhaseStarted)
	})
	t.Run("a replacement pod, a later prune and pod absence are not evidence", func(t *testing.T) {
		id, pod := toUnknown()
		if _, err := f.svc.WakeWorker(f.env.ctx, &f.actor, id, ""); err != nil {
			t.Fatal(err)
		}
		next := uuid.New()
		f.mustApply(id, TransitionReady, next)
		f.env.exec(`UPDATE workers SET registered_pod_uid = $2 WHERE id = $1`, id, next)
		f.heartbeatNow(id)
		f.register(id, &next)
		// A stopped report naming the replacement pod is not the attempt's pod.
		f.wantTransitionConflict(id, f.cleanupStopped(id, next))
		// A later attempt's confirmed report (a different op) is rejected.
		later := f.resultRep(id, true, okSteps())
		later.OpID = uuid.NewString()
		f.wantCleanupConflict(id, later)
		// Absence reports (scaled_down) are refused outright while unsettled.
		f.wantTransitionConflict(id, f.tr(id, TransitionScaledDown, uuid.Nil))
		f.pass()
		f.want(id, "waking")
		f.wantPhase(id, CleanupPhaseUnknown)
		_ = pod
	})
}

func TestHibernationCleanupDeleteRefusedLiveDB(t *testing.T) {
	f := newHibFx(t)
	for _, phase := range []string{CleanupPhaseRequested, CleanupPhaseStarted, CleanupPhaseUnknown} {
		var id uuid.UUID
		switch phase {
		case CleanupPhaseRequested:
			id, _ = f.dockerWorker()
			f.hibernate(id)
		case CleanupPhaseStarted:
			id, _ = f.started()
		default:
			id, _ = f.started()
			f.env.exec(`UPDATE workers SET cleanup_started_at = now() - interval '11 minutes' WHERE id = $1`, id)
			f.pass()
		}
		f.wantPhase(id, phase)
		err := f.svc.DeleteWorker(f.env.ctx, f.user, id)
		var un *WorkerHibernationCleanupUnsettledError
		if !errors.As(err, &un) || !errors.Is(err, ErrWorkerHibernationCleanupUnsettled) || un.Phase != phase {
			t.Fatalf("delete during %s = %v", phase, err)
		}
		if _, gerr := f.env.q.GetWorkerByID(f.env.ctx, id); gerr != nil {
			t.Fatalf("a refused delete removed the worker: %v", gerr)
		}
	}
	// Settled: the delete goes through.
	id, _ := f.started()
	f.mustReport(id, f.resultRep(id, true, okSteps()))
	if err := f.svc.DeleteWorker(f.env.ctx, f.user, id); err != nil {
		t.Fatalf("delete after settlement: %v", err)
	}
	if err := f.svc.DeleteWorker(f.env.ctx, f.user, id); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("second delete = %v, want not found", err)
	}
}

// The CHECK constraints are the backstop against an image that does not know the attempt.
func TestHibernationCleanupCheckFencesLiveDB(t *testing.T) {
	f := newHibFx(t)
	id, _ := f.started()
	for name, sql := range map[string]string{
		"old image restores none":        `UPDATE workers SET hibernation_state = 'none' WHERE id = $1`,
		"old image reports hibernated":   `UPDATE workers SET hibernation_state = 'hibernated', hibernated_at = now() WHERE id = $1`,
		"old image records scaling_down": `UPDATE workers SET hibernation_scaling_down_at = now() WHERE id = $1`,
		"phase without an op id":         `UPDATE workers SET cleanup_op_id = NULL WHERE id = $1`,
		"started without a pod uid":      `UPDATE workers SET cleanup_pod_uid = NULL WHERE id = $1`,
		"unknown phase value":            `UPDATE workers SET cleanup_phase = 'maybe' WHERE id = $1`,
		"unknown without unknown_at":     `UPDATE workers SET cleanup_phase = 'unknown' WHERE id = $1`,
		"settled without an outcome":     `UPDATE workers SET cleanup_phase = 'settled', cleanup_settled_at = now() WHERE id = $1`,
		"outcome outside the set":        `UPDATE workers SET hibernation_cleanup = 'meh' WHERE id = $1`,
		"negative sequence":              `UPDATE workers SET cleanup_reconcile_seq = -1 WHERE id = $1`,
		"overlong reason":                `UPDATE workers SET hibernation_cleanup_reason = repeat('x', 501) WHERE id = $1`,
	} {
		_, err := f.env.pool.Exec(f.env.ctx, sql, id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: err = %v, want a check violation", name, err)
		}
	}
	// A settled attempt may sit under none, so settled-then-none stays legal.
	f.mustReport(id, f.resultRep(id, true, okSteps()))
	f.env.exec(`UPDATE workers SET hibernation_state = 'none', hibernation_op_id = NULL WHERE id = $1`, id)
}
