package workersvc

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1226 M1 LiveDB coverage of the queued-reason completion-capability rung: an INTERLOCKED
// queued run whose owner has NO online worker implementing the completion protocol
// (completion_interlock_v1) surfaces reasonNoCompletionCapableWorker, and one WITH a protocol
// worker does not. This drives the real Service.queuedReason over a throwaway Postgres (skipped
// unless UZI_TEST_DATABASE_URL), pinning both the new CountOnlineWorkersSatisfyingProtocol SQL and
// the rung wiring. Reuses the interlockLiveDB harness (setup + seed helpers).

// interlockedQueuedRow builds the ListActiveRunsForHealth row queuedReason consumes for an
// INTERLOCKED queued run: no required_capabilities (so the ordinary caps rung is skipped) and no
// repo_id (so the docker-allowlist rung is skipped), isolating the completion-protocol rung as the
// deciding one. completion_contract_version is set, which is what makes the run interlocked.
func (e interlockLiveDB) interlockedQueuedRow(runID [16]byte) store.ListActiveRunsForHealthRow {
	return store.ListActiveRunsForHealthRow{
		ID:                        runID,
		UserID:                    e.userID,
		Status:                    "queued",
		Health:                    healthOK,
		CompletionContractVersion: pgtype.Int4{Int32: 1, Valid: true},
	}
}

func TestQueuedReasonNoCompletionCapableWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	// One ONLINE worker that does NOT implement the completion protocol (protocol_capabilities '{}').
	e.seedWorker(t, nil)
	v1 := int32(1)
	runID := e.seedQueuedRun(t, &v1, nil) // interlocked, no required_capabilities

	got := svc.queuedReason(e.ctx, time.Now(), e.interlockedQueuedRow(runID))
	if got != reasonNoCompletionCapableWorker {
		t.Fatalf("queuedReason = %q, want %q — an interlocked queued run with no protocol-capable "+
			"worker online must surface the completion-capability block", got, reasonNoCompletionCapableWorker)
	}
}

func TestQueuedReasonCompletionCapableWorkerFallsThroughLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	// One ONLINE worker that DOES implement the completion protocol.
	e.seedWorker(t, []string{"completion_interlock_v1"})
	v1 := int32(1)
	runID := e.seedQueuedRun(t, &v1, nil)

	got := svc.queuedReason(e.ctx, time.Now(), e.interlockedQueuedRow(runID))
	if got == reasonNoCompletionCapableWorker {
		t.Fatalf("queuedReason = %q, but a protocol-capable worker is online — the completion-capability "+
			"rung must fall through to the generic reasons", got)
	}
}

// TestQueuedReasonLegacyRunNeverCompletionCapableReasonLiveDB pins the inert direction: a LEGACY
// (non-interlocked) queued run never reaches the completion-protocol rung, so it cannot get
// reasonNoCompletionCapableWorker even with no protocol-capable worker online.
func TestQueuedReasonLegacyRunNeverCompletionCapableReasonLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	e.seedWorker(t, nil) // no protocol worker
	runID := e.seedQueuedRun(t, nil, nil)

	row := e.interlockedQueuedRow(runID)
	row.CompletionContractVersion = pgtype.Int4{} // legacy: NULL contract version
	if got := svc.queuedReason(e.ctx, time.Now(), row); got == reasonNoCompletionCapableWorker {
		t.Fatalf("queuedReason = %q, but a legacy run is not interlocked — the completion-protocol rung "+
			"must not fire for it", got)
	}
}
