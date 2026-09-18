package workersvc

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1332 M5A (D3) LiveDB coverage of the fail-closed Codex-harness claim placement gate. A
// CODEX-INDICATING run (harness='codex', or a surviving M1 binding sentinel) is claimable /
// peer-deferrable / counted-available ONLY by a worker advertising the 'codex_harness_v1' protocol
// capability, so an old worker that would ignore the unknown Codex JSON and silently run Claude can
// never receive it. These mirror the completion_interlock_v1 LiveDB tests exactly (claimant admission,
// fleet-spread peer mirror, fleet-count queued reason) and reuse the interlockLiveDB harness. This is
// a credential-routing SECURITY boundary, so it runs against a real throwaway Postgres — skipped
// unless UZI_TEST_DATABASE_URL points at one (run via ./e2e/run-store-it.sh). A package that prints
// `ok` with PASS=0 is INVALID, not green.

// seedCodexQueuedRun inserts a queued issue run with harness='codex' — the primary Codex-indicating
// shape (the binding-coherence CHECK, migration 00226, allows harness='codex' with NULL sentinels).
// worker_id NULL so affinity never pins it; required_capabilities '{}' so fn_worker_can_claim is
// trivially satisfiable and the D3 Codex clause is the ONLY thing that can block.
func (e interlockLiveDB) seedCodexQueuedRun(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	iid := *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id, harness)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'queued', NULL, 'codex')`,
		id, e.userID, e.repoID, iid)
	return id
}

// codexIndicatingQueuedRow builds the ListActiveRunsForHealth row queuedReason consumes for a
// CODEX-INDICATING queued run: harness='codex', no required_capabilities (so the ordinary caps rung
// is skipped) and no repo_id (so the docker-allowlist rung is skipped) and no completion contract (so
// the completion rung is skipped), isolating the Codex-capability rung as the deciding one.
func (e interlockLiveDB) codexIndicatingQueuedRow(runID [16]byte) store.ListActiveRunsForHealthRow {
	return store.ListActiveRunsForHealthRow{
		ID:      runID,
		UserID:  e.userID,
		Status:  "queued",
		Health:  healthOK,
		Harness: harnessCodex,
	}
}

// TestClaimCodexHardClauseBlocksIncapableWorkerLiveDB is the core D3 assertion, mirroring
// TestClaimInterlockHardClauseBlocksIncapableWorkerLiveDB: a CODEX-INDICATING run is NOT claimable by
// a worker whose protocol_capabilities lacks 'codex_harness_v1' — and NEITHER the @capability_aware
// kill-switch (false) NOR the owner ClearRunRequiredCapabilities override can bypass it. A CAPABLE
// worker then claims it.
//
// CALIBRATION (claimant clause): delete the claimant Codex clause in runtime.sql (the
// `AND (NOT (r.harness = 'codex' OR ...) OR 'codex_harness_v1' = ANY(@worker_protocol_caps))` added
// after the completion clause) and regenerate — step (1) then lets the incapable worker claim and this
// test fails.
func TestClaimCodexHardClauseBlocksIncapableWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)

	incapable := e.seedWorker(t, nil)                               // protocol_capabilities '{}'
	capable := e.seedWorker(t, []string{capability.CodexHarnessV1}) // advertises codex_harness_v1
	runID := e.seedCodexQueuedRun(t)                                // harness='codex', required_capabilities '{}'

	// (1) capability_aware=false does NOT bypass: the incapable worker still cannot claim.
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(incapable, []string{}, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("codex-indicating run claimed by an incapable worker with capability_aware=false (err=%v); the D3 clause must block regardless of the kill-switch", err)
	}
	if s := e.runStatus(t, runID); s != "queued" {
		t.Fatalf("run must stay queued after the blocked claim; status = %q", s)
	}

	// (2) the owner override (ClearRunRequiredCapabilities) does NOT bypass either. Park the run at
	// awaiting_approval, set a required cap, clear it via the real owner-clear path, re-queue, and
	// re-attempt. The Codex clause is OUTSIDE required_capabilities, so clearing it changes nothing.
	e.exec(t, `UPDATE runs SET status = 'awaiting_approval', required_capabilities = '{docker}' WHERE id = $1`, runID)
	cleared, err := e.q.ClearRunRequiredCapabilities(e.ctx, store.ClearRunRequiredCapabilitiesParams{ID: runID, UserID: e.userID})
	if err != nil {
		t.Fatalf("ClearRunRequiredCapabilities: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("ClearRunRequiredCapabilities affected %d rows, want 1 (the override must fire)", cleared)
	}
	e.exec(t, `UPDATE runs SET status = 'queued' WHERE id = $1`, runID)
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(incapable, []string{}, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("codex-indicating run claimed by an incapable worker AFTER ClearRunRequiredCapabilities (err=%v); the D3 clause is outside the override and must still block", err)
	}
	if s := e.runStatus(t, runID); s != "queued" {
		t.Fatalf("run must stay queued after the override+blocked claim; status = %q", s)
	}

	// (3) a CAPABLE worker claims it — the clause admits a worker advertising codex_harness_v1.
	run, err := e.q.ClaimRun(e.ctx, e.claimParams(capable, []string{capability.CodexHarnessV1}, false))
	if err != nil {
		t.Fatalf("capable worker must claim the codex-indicating run: %v", err)
	}
	if run.ID != runID {
		t.Fatalf("capable worker claimed %v, want %v", run.ID, runID)
	}
	if run.Status != "claimed" {
		t.Fatalf("claimed run status = %q, want claimed", run.Status)
	}
}

// TestClaimOrdinaryClaudeRunUnaffectedByCodexGateLiveDB proves the byte-compatible guarantee: an
// ordinary Claude run (harness='claude', no codex fields) remains claimable by an empty-protocol-caps
// (old) worker — the D3 clause exempts a non-Codex-indicating row, so old workers keep taking Claude
// work exactly as before. This is the too-broad-gate guard for the claimant clause above.
func TestClaimOrdinaryClaudeRunUnaffectedByCodexGateLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	incapable := e.seedWorker(t, nil)     // no protocol capabilities (an old worker)
	runID := e.seedQueuedRun(t, nil, nil) // ordinary Claude run: harness defaults to 'claude'

	run, err := e.q.ClaimRun(e.ctx, e.claimParams(incapable, []string{}, false))
	if err != nil {
		t.Fatalf("ordinary Claude run must be claimable by an old (no-protocol-cap) worker: %v", err)
	}
	if run.ID != runID {
		t.Fatalf("claimed %v, want %v", run.ID, runID)
	}
	if run.Harness != harnessClaude {
		t.Fatalf("ordinary run harness = %q, want claude", run.Harness)
	}
}

// TestClaimCodexPeerSpreadMirrorLiveDB covers the D3 peer fleet-spread MIRROR clause. Fleet-aware
// spread (PRD #216) defers a queued run to a strictly-better idle peer instead of a busy claimant
// taking it — but for a CODEX-INDICATING run that peer must ALSO advertise codex_harness_v1, or the
// run would be deferred to a peer that can never claim it (the D3 claimant clause blocks the peer),
// making the run permanently unclaimable by being preferred. The two sub-cases differ ONLY in the idle
// peer's protocol_capabilities, isolating the mirror clause.
//
// CALIBRATION (peer clause): delete the peer Codex mirror clause in runtime.sql (the mirrored
// `AND (NOT (r.harness = 'codex' OR ...) OR 'codex_harness_v1' = ANY(p.protocol_capabilities))` inside
// the NOT EXISTS peer block) and regenerate — the incapable-peer sub-case then defers to the incapable
// peer (ErrNoRows) instead of the busy claimant claiming, and that sub-case fails.
func TestClaimCodexPeerSpreadMirrorLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)

	// A busy CAPABLE claimant: cap=2 with 1 active run, so it is NOT minimum-loaded and the spread rule
	// CAN defer its claim to a strictly-better (idle) peer. It advertises codex_harness_v1, so the
	// claimant's OWN D3 clause never blocks it — isolating the PEER clause.
	newBusyClaimant := func() uuid.UUID {
		me := e.seedSpreadWorker(t, []string{capability.CodexHarnessV1}, 2)
		e.seedActiveRunOwnedBy(t, me)
		return me
	}

	t.Run("incapable idle peer is NOT a spread target; busy claimant claims", func(t *testing.T) {
		me := newBusyClaimant()
		e.seedSpreadWorker(t, nil, 2) // idle, cap=2, but advertises no protocol capability
		runID := e.seedCodexQueuedRun(t)

		run, err := e.q.ClaimRun(e.ctx, e.claimParams(me, []string{capability.CodexHarnessV1}, false))
		if err != nil {
			t.Fatalf("busy claimant must claim the codex-indicating run — the incapable peer is not a valid deferral target (err=%v); the mirror clause must exclude it", err)
		}
		if run.ID != runID {
			t.Fatalf("claimed %v, want %v", run.ID, runID)
		}
		if run.Status != "claimed" {
			t.Fatalf("claimed run status = %q, want claimed", run.Status)
		}
	})

	t.Run("capable idle peer IS a spread target; busy claimant defers", func(t *testing.T) {
		me := newBusyClaimant()
		e.seedSpreadWorker(t, []string{capability.CodexHarnessV1}, 2) // idle, cap=2, advertises codex_harness_v1
		runID := e.seedCodexQueuedRun(t)

		if _, err := e.q.ClaimRun(e.ctx, e.claimParams(me, []string{capability.CodexHarnessV1}, false)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("busy claimant must DEFER the codex-indicating run to the capable idle peer (err=%v); the mirror clause admits a capable peer as a spread target", err)
		}
		if s := e.runStatus(t, runID); s != "queued" {
			t.Fatalf("deferred run must stay queued; status = %q", s)
		}
	})
}

// TestQueuedReasonNoCodexCapableWorkerLiveDB drives the real Service.queuedReason: a CODEX-INDICATING
// queued run whose owner has NO online worker advertising codex_harness_v1 surfaces the D3-fixed prose
// reasonNoCodexCapableWorker ("no Codex-capable worker is online"), one WITH a capable worker falls
// through, and an ordinary Claude run never reaches this rung. This pins the new
// CountOnlineWorkersSatisfyingCodexHarness SQL and the health-rung wiring together.
//
// CALIBRATION (fleet-count / queued-reason rung): delete the Codex rung in health.go (the
// `if r.Harness == harnessCodex || ... { CountOnlineWorkersSatisfyingCodexHarness ... }` block) — the
// first sub-case then falls through to a generic wait and this test fails.
func TestQueuedReasonNoCodexCapableWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	t.Run("no codex-capable worker online -> reasonNoCodexCapableWorker", func(t *testing.T) {
		e.seedWorker(t, nil) // one ONLINE worker with no protocol capabilities
		runID := e.seedCodexQueuedRun(t)
		got := svc.queuedReason(e.ctx, time.Now(), e.codexIndicatingQueuedRow(runID))
		if got != reasonNoCodexCapableWorker {
			t.Fatalf("queuedReason = %q, want %q — a codex-indicating queued run with no codex-capable worker online must surface the Codex-capability block", got, reasonNoCodexCapableWorker)
		}
		// Pin the exact D3 user-visible prose so a copy-edit here is a deliberate, reviewed change.
		if reasonNoCodexCapableWorker != "no Codex-capable worker is online" {
			t.Fatalf("reasonNoCodexCapableWorker = %q, want the D3-fixed prose", reasonNoCodexCapableWorker)
		}
	})

	t.Run("codex-capable worker online -> falls through", func(t *testing.T) {
		e.seedWorker(t, []string{capability.CodexHarnessV1}) // an ONLINE codex-capable worker
		runID := e.seedCodexQueuedRun(t)
		got := svc.queuedReason(e.ctx, time.Now(), e.codexIndicatingQueuedRow(runID))
		if got != reasonWaitingWorker {
			t.Fatalf("queuedReason = %q, want %q", got, reasonWaitingWorker)
		}
	})

	t.Run("ordinary Claude run never gets the Codex reason", func(t *testing.T) {
		e.seedWorker(t, nil) // no codex-capable worker
		runID := e.seedQueuedRun(t, nil, nil)
		row := e.codexIndicatingQueuedRow(runID)
		row.Harness = harnessClaude // an ordinary Claude run is not codex-indicating
		if got := svc.queuedReason(e.ctx, time.Now(), row); got != reasonWaitingWorker {
			t.Fatalf("queuedReason = %q, want %q", got, reasonWaitingWorker)
		}
	})
}

// TestClaimCodexVocabularyRemovalCalibrationLiveDB registers a worker through the REAL Service.Register
// path (which runs capability.FilterProtocol over the self-reported protocol capabilities) and proves
// that a worker registered advertising codex_harness_v1 then claims a codex-indicating run.
//
// CALIBRATION (protocol vocabulary): remove CodexHarnessV1 from capability.protocolVocabulary — because
// FilterProtocol silently DROPS a string that is not in the vocabulary, Register then stores no
// codex_harness_v1 for the worker, the stored-set assertion fails, and (were it not for that) the claim
// would fail with ErrNoRows. That is exactly the D3-required calibrated failure ("removing
// codex_harness_v1 must make the capable-worker claim test fail").
func TestClaimCodexVocabularyRemovalCalibrationLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)

	// A bare worker row Register can UPDATE (RegisterWorker is an UPDATE-by-id).
	workerID := e.seedWorker(t, nil)
	if _, _, err := svc.Register(e.ctx, store.Worker{ID: workerID, UserID: e.userID}, "v-test", "base", nil, nil, []string{capability.CodexHarnessV1}, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Read back the STORED protocol_capabilities (what FilterProtocol persisted). This assertion is the
	// calibration tripwire: with the capability removed from the vocabulary, the stored set is empty.
	stored := e.workerProtocolCaps(t, workerID)
	if !slices.Contains(stored, capability.CodexHarnessV1) {
		t.Fatalf("registered worker stored protocol_capabilities = %v, want to contain %q (FilterProtocol dropped it — codex_harness_v1 missing from the protocol vocabulary?)", stored, capability.CodexHarnessV1)
	}

	// The worker claims the codex-indicating run using its STORED caps, exactly as the production claim
	// path threads wkr.ProtocolCapabilities into @worker_protocol_caps.
	runID := e.seedCodexQueuedRun(t)
	run, err := e.q.ClaimRun(e.ctx, e.claimParams(workerID, stored, false))
	if err != nil {
		t.Fatalf("a worker registered with codex_harness_v1 must claim the codex-indicating run: %v", err)
	}
	if run.ID != runID {
		t.Fatalf("claimed %v, want %v", run.ID, runID)
	}
}

// workerProtocolCaps reads a worker's stored protocol_capabilities column.
func (e interlockLiveDB) workerProtocolCaps(t *testing.T, workerID uuid.UUID) []string {
	t.Helper()
	var caps []string
	if err := e.pool.QueryRow(e.ctx, `SELECT protocol_capabilities FROM workers WHERE id = $1`, workerID).Scan(&caps); err != nil {
		t.Fatalf("read worker protocol_capabilities: %v", err)
	}
	return caps
}
