package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Reuse the real registration/claim fixture: inventory must carry the returned
// nonce and generation, while the policy report remains undelivered.
func policyPendingFlight(t *testing.T) outageCodexFlight {
	t.Helper()
	x := newOutageCodexFlight(t, codexAuthModeAPIKey)
	x.f.svc.p.PolicyHoldNegotiated = true
	x.f.svc.p.PolicyHoldSnapshotEnabled = true
	caps := append(x.f.wkr.ProtocolCapabilities, capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1)
	x.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", x.f.workerID, caps)
	x.f.wkr = hbWorker(t, x.env, x.f.workerID)
	x.env.exec(`UPDATE runs SET status='running', started_at=now()-interval '3 hours',
		status_since=now()-interval '3 hours', budget_wall_seconds=86400 WHERE id=$1`, x.f.runID)
	return x
}

func policyPendingInventory(x outageCodexFlight, overflow bool) *ActiveSnapshot {
	active := []ActiveRunEntry{entry(x.f.runID, x.initial.ClaimGeneration, "running", true)}
	if overflow {
		active = []ActiveRunEntry{}
	}
	// terminal_pending is the wire inventory bit for this NONTERMINAL policy
	// outcome too; no policy transition has reached the server yet.
	return &ActiveSnapshot{
		Active: active, PendingOverflow: overflow,
		PendingAll: []PendingOutcomeIdentity{{RunID: x.f.runID.String(), ClaimGeneration: x.initial.ClaimGeneration}},
	}
}

func policyPendingMissing(t *testing.T, x outageCodexFlight) {
	t.Helper()
	if _, err := x.env.q.RequeueRunsMissingFromSnapshot(x.env.ctx, store.RequeueRunsMissingFromSnapshotParams{
		WorkerID: pgconv.UUID(x.f.workerID), MissingCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
		MaxRequeues: 5, Now: pgconv.Time(time.Now()), GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
	}); err != nil {
		t.Fatal(err)
	}
}

func policyPendingStale(t *testing.T, x outageCodexFlight) {
	t.Helper()
	x.env.exec("UPDATE workers SET last_heartbeat_at=now()-interval '3 hours' WHERE id=$1", x.f.workerID)
	if _, err := x.env.q.RequeueRunsOfStaleWorkers(x.env.ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 5, Cutoff: pgconv.Time(time.Now().Add(-time.Hour)),
	}); err != nil {
		t.Fatal(err)
	}
}

func assertPolicyPendingUnreported(t *testing.T, x outageCodexFlight) {
	t.Helper()
	r := mustRun(t, x.env, x.f.runID)
	if r.Status != "running" || r.PolicyRefusalCount != 0 || r.PolicyLastOutcomeID.Valid ||
		r.RequeueCount != 0 || r.ClaimGeneration != x.initial.ClaimGeneration || r.WorkerID != pgconv.UUID(x.f.workerID) {
		t.Fatalf("undelivered policy inventory changed run: status=%s count=%d requeues=%d generation=%d owner=%v",
			r.Status, r.PolicyRefusalCount, r.RequeueCount, r.ClaimGeneration, r.WorkerID)
	}
}

func applyPendingPolicy(t *testing.T, x outageCodexFlight, req PolicyHoldRequest) {
	t.Helper()
	r, disposition, err := x.f.svc.ReportPolicyHold(x.env.ctx, hbWorker(t, x.env, x.f.workerID), x.f.runID, req)
	if err != nil || disposition != PolicyHoldApplied || r.Status != "paused" || r.PolicyRefusalCount != 1 ||
		r.PolicyLastOutcomeID != pgconv.UUID(uuid.MustParse(req.OutcomeID)) ||
		r.PolicyLastTag.String != *req.Tag || r.PolicyLastPhase.String != *req.Phase ||
		r.PolicyLastCorrelationID.String != *req.CorrelationID {
		t.Fatalf("delayed policy report: status=%s count=%d disposition=%s err=%v", r.Status, r.PolicyRefusalCount, disposition, err)
	}
}

func TestPolicyPendingSnapshotProtectsMissingAndStaleLiveDB(t *testing.T) {
	x := policyPendingFlight(t)
	req := policyRequest() // Immutable local evidence exists before the network outage.
	r := mustRun(t, x.env, x.f.runID)
	control := seedOutageRun(t, x.env, x.f.userID, r.RepoID.Bytes, x.f.workerID, "running", "issue", 1, 0)
	x.env.exec("UPDATE runs SET harness='codex' WHERE id=$1", control)

	m4Heartbeat(t, x.env, x.f.svc, x.f.workerID, policyPendingInventory(x, false))
	leased, ok := readActiveRun(t, x.env, x.f.workerID, x.f.runID)
	if !ok || !leased.terminalPending || !leased.untilFuture || leased.gen != x.initial.ClaimGeneration {
		t.Fatalf("policy pending lease=%+v present=%v", leased, ok)
	}
	m4Aggregate(t, x.env, x.f.workerID, 1, false)
	if statusOf(t, x.env, control) != "queued" {
		t.Fatal("missing unprotected control did not requeue")
	}
	policyPendingMissing(t, x)
	policyPendingStale(t, x)
	assertPolicyPendingUnreported(t, x)
	// Only the actual report, after both SQL writers ran, increments policy count.
	applyPendingPolicy(t, x, req)
}

func TestPolicyPendingCapZeroOverflowProtectsMissingAndStaleLiveDB(t *testing.T) {
	x := policyPendingFlight(t)
	x.f.svc.p.WorkerOutboxMaxPending = 0
	req := policyRequest()
	m4Heartbeat(t, x.env, x.f.svc, x.f.workerID, policyPendingInventory(x, true))
	if countActiveRuns(t, x.env, x.f.workerID) != 0 {
		t.Fatal("cap-zero inventory unexpectedly stored an active entry")
	}
	if flag, future := workerOverflow(t, x.env, x.f.workerID); !flag || !future {
		t.Fatalf("overflow closure: flag=%v future=%v", flag, future)
	}
	m4Aggregate(t, x.env, x.f.workerID, 1, true)
	policyPendingMissing(t, x)
	policyPendingStale(t, x)
	assertPolicyPendingUnreported(t, x)
	applyPendingPolicy(t, x, req)
}

func TestPolicyPendingExpiredLeaseAllowsOtherClaimLiveDB(t *testing.T) {
	x := policyPendingFlight(t)
	req := policyRequest()
	m4Heartbeat(t, x.env, x.f.svc, x.f.workerID, policyPendingInventory(x, false))
	policyPendingStale(t, x)
	assertPolicyPendingUnreported(t, x)

	// Expiry is a bounded protection window, not global exactly-once execution.
	// Age both inventory clocks so the fresh-snapshot claim predicate also lapses.
	x.env.exec(`UPDATE worker_active_runs SET terminal_pending_until=now()-interval '1 minute',
		reported_at=now()-interval '3 hours' WHERE worker_id=$1`, x.f.workerID)
	x.env.exec("UPDATE worker_pending_outcome_identities SET reported_until=now()-interval '1 minute' WHERE worker_id=$1", x.f.workerID)
	policyPendingStale(t, x)
	requeued := mustRun(t, x.env, x.f.runID)
	if requeued.Status != "queued" || requeued.RequeueCount != 1 || requeued.PolicyRefusalCount != 0 ||
		requeued.ClaimGeneration != x.initial.ClaimGeneration {
		t.Fatalf("expired policy lease: status=%s requeues=%d policy=%d generation=%d",
			requeued.Status, requeued.RequeueCount, requeued.PolicyRefusalCount, requeued.ClaimGeneration)
	}

	otherID := seedSnapshotWorker(t, x.env, x.f.userID, uuid.NewString())
	x.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", otherID, x.f.wkr.ProtocolCapabilities)
	x.env.exec("UPDATE runs SET status_since=now()-interval '3 hours', updated_at=now()-interval '3 hours' WHERE id=$1", x.f.runID)
	other := hbWorker(t, x.env, otherID)
	claimed, err := x.env.q.ClaimRun(x.env.ctx, claimRunParamsFor(other))
	if err != nil || claimed.ID != x.f.runID || claimed.WorkerID != pgconv.UUID(otherID) ||
		claimed.ClaimGeneration != x.initial.ClaimGeneration+1 {
		t.Fatalf("other worker claim: id=%s generation=%d owner=%v err=%v", claimed.ID, claimed.ClaimGeneration, claimed.WorkerID, err)
	}
	// Knowing current G cannot attest to a refusal produced by its different owner.
	// This is not a historical G1 report (M2 authenticates those through G1 custody).
	// Own-current-G conversion is covered by TestPolicyHoldAnyGenerationWithoutCustodyLiveDB.
	req.SourceGeneration = claimed.ClaimGeneration
	req.ClaimGeneration = claimed.ClaimGeneration
	if _, _, err := x.f.svc.ReportPolicyHold(x.env.ctx, x.f.wkr, x.f.runID, req); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("foreign-current-generation report=%v", err)
	}
	if got := mustRun(t, x.env, x.f.runID); got.PolicyRefusalCount != 0 || got.WorkerID != pgconv.UUID(otherID) {
		t.Fatal("rejected original-worker report changed the new owner's run")
	}
}
