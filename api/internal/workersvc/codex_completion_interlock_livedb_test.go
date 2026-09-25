package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func codexCompletionCaps() []string {
	return []string{capability.CompletionInterlockV1, capability.CodexHarnessV1, capability.CodexCompletionInterlockV1}
}

func TestCodexCompletionClaimLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	old := e.seedWorker(t, []string{capability.CompletionInterlockV1, capability.CodexHarnessV1})
	capable := e.seedWorker(t, codexCompletionCaps())
	runID := e.seedCodexQueuedRun(t)
	e.exec(t, `UPDATE runs SET completion_contract_version = 1 WHERE id = $1`, runID)

	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(old, []string{capability.CompletionInterlockV1, capability.CodexHarnessV1}, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old Codex worker claimed interlocked run: %v", err)
	}
	got, err := e.q.ClaimRun(e.ctx, e.claimParams(capable, codexCompletionCaps(), false))
	if err != nil || got.ID != runID {
		t.Fatalf("capable claim: run=%v err=%v", got.ID, err)
	}
}

func TestLegacyCodexCompletionClaimLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	old := e.seedWorker(t, []string{capability.CodexHarnessV1})
	runID := e.seedCodexQueuedRun(t)
	got, err := e.q.ClaimRun(e.ctx, e.claimParams(old, []string{capability.CodexHarnessV1}, false))
	if err != nil || got.ID != runID {
		t.Fatalf("legacy Codex claim: run=%v err=%v", got.ID, err)
	}
}

func TestCodexCompletionPeerMirrorLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	me := e.seedSpreadWorker(t, codexCompletionCaps(), 2)
	e.seedActiveRunOwnedBy(t, me)
	e.seedSpreadWorker(t, []string{capability.CompletionInterlockV1, capability.CodexHarnessV1}, 2)
	runID := e.seedCodexQueuedRun(t)
	e.exec(t, `UPDATE runs SET completion_contract_version = 1 WHERE id = $1`, runID)
	got, err := e.q.ClaimRun(e.ctx, e.claimParams(me, codexCompletionCaps(), false))
	if err != nil || got.ID != runID {
		t.Fatalf("incapable idle peer must not defer claim: run=%v err=%v", got.ID, err)
	}
}

func TestCodexCompletionCapablePeerDefersLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	me := e.seedSpreadWorker(t, codexCompletionCaps(), 2)
	e.seedActiveRunOwnedBy(t, me)
	e.seedSpreadWorker(t, codexCompletionCaps(), 2)
	runID := e.seedCodexQueuedRun(t)
	e.exec(t, `UPDATE runs SET completion_contract_version = 1 WHERE id = $1`, runID)
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(me, codexCompletionCaps(), false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("busy claimant did not defer to capable idle peer: %v", err)
	}
	if got := e.runStatus(t, runID); got != "queued" {
		t.Fatalf("deferred run status = %q", got)
	}
}

func TestCodexCompletionReleasedWorkerHealthLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	workerID := e.seedWorker(t, codexCompletionCaps())
	e.exec(t, `UPDATE workers SET last_heartbeat_at = now(), snapshot_register_nonce = 'old-process' WHERE id = $1`, workerID)
	runID := e.seedCodexQueuedRun(t)
	e.exec(t, `UPDATE runs SET completion_contract_version = 1, released_worker_id = $2, released_worker_nonce = 'old-process' WHERE id = $1`, runID, workerID)
	row := e.codexIndicatingQueuedRow(runID)
	row.CompletionContractVersion = pgtype.Int4{Int32: 1, Valid: true}
	row.ReleasedWorkerID = pgtype.UUID{Bytes: workerID, Valid: true}
	reason := svc.queuedReason(e.ctx, time.Now(), row)
	if reason != "waiting for another worker, or restart worker w-"+workerID.String()[:8]+" (its previous process was released at the time limit)" {
		t.Fatalf("released worker reason = %q", reason)
	}
}

func TestCodexCompletionClaimableCountLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	e.seedHeartbeatWorker(t, []string{capability.CompletionInterlockV1, capability.CodexHarnessV1})
	runID := e.seedCodexQueuedRun(t)
	e.exec(t, `UPDATE runs SET completion_contract_version = 1 WHERE id = $1`, runID)

	if got := e.claimableForRun(t, runID); got != 0 {
		t.Fatalf("interlocked Codex run with only an old worker: claimable count = %d, want 0", got)
	}
	e.seedHeartbeatWorker(t, codexCompletionCaps())
	if got := e.claimableForRun(t, runID); got != 1 {
		t.Fatalf("interlocked Codex run after adding a capable worker: claimable count = %d, want 1", got)
	}
}

func TestCodexCompletionCustomRootProtocolIntersectionLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	e.seedWorker(t, codexCompletionCaps())
	e.seedWorker(t, []string{capability.CompletionInterlockV1, capability.CodexHarnessV1, capability.CodexCustomModelV1})

	count := func() int64 {
		t.Helper()
		n, err := e.q.CountOnlineWorkersSatisfyingCodexCompletion(e.ctx, store.CountOnlineWorkersSatisfyingCodexCompletionParams{
			UserID: e.userID, CustomRoot: true,
		})
		if err != nil {
			t.Fatalf("CountOnlineWorkersSatisfyingCodexCompletion: %v", err)
		}
		return n
	}
	if got := count(); got != 0 {
		t.Fatalf("split custom-root fleet: protocol intersection count = %d, want 0", got)
	}
	e.seedWorker(t, append(codexCompletionCaps(), capability.CodexCustomModelV1))
	if got := count(); got != 1 {
		t.Fatalf("four-capability custom-root worker: protocol intersection count = %d, want 1", got)
	}
}

func TestCodexCompletionHealthIntersectionLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	e.seedWorker(t, []string{capability.CompletionInterlockV1, capability.CodexHarnessV1})
	e.seedWorker(t, []string{capability.CodexCompletionInterlockV1})
	runID := e.seedCodexQueuedRun(t)
	row := e.codexIndicatingQueuedRow(runID)
	row.CompletionContractVersion = pgtype.Int4{Int32: 1, Valid: true}
	if got := svc.queuedReason(e.ctx, time.Now(), row); got != reasonNoCodexCompletionCapableWorker {
		t.Fatalf("split fleet reason = %q", got)
	}
	e.seedWorker(t, codexCompletionCaps())
	if got := svc.queuedReason(e.ctx, time.Now(), row); got != reasonWaitingWorker {
		t.Fatalf("capable fleet reason = %q", got)
	}
	row.CodexCustomRoot = true
	if got := svc.queuedReason(e.ctx, time.Now(), row); got != reasonNoCustomCodexCapableWorker && got != reasonNoCodexCompletionCapableWorker {
		t.Fatalf("custom root lacking model protocol reason = %q", got)
	}
	e.seedWorker(t, append(codexCompletionCaps(), capability.CodexCustomModelV1))
	if got := svc.queuedReason(e.ctx, time.Now(), row); got != reasonWaitingWorker {
		t.Fatalf("custom root capable reason = %q", got)
	}
	n, err := e.q.CountOnlineWorkersSatisfyingCodexCompletion(e.ctx, store.CountOnlineWorkersSatisfyingCodexCompletionParams{UserID: e.userID, CustomRoot: true})
	if err != nil || n == 0 {
		t.Fatalf("custom-root protocol intersection: count=%d err=%v", n, err)
	}
}
