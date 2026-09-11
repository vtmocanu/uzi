package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// interlockLiveDB is the shared setup for the PRD #1226 M1 (D1/D2) live-DB tests: a real
// throwaway Postgres with the goose migrations applied, plus a user/connection/repo the
// seeded runs and workers hang off. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres (run via ./e2e/run-store-it.sh). A package that prints `ok` with
// PASS=0 is INVALID, not green.
type interlockLiveDB struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	userID uuid.UUID
	repoID uuid.UUID
	// nextIID hands out a distinct issue_iid per seeded run so two active runs never
	// collide on uq_runs_one_active_per_issue (repo_id, issue_iid). A pointer so the
	// value-receiver seed helpers share one counter.
	nextIID *int64
}

func setupInterlockLiveDB(t *testing.T) interlockLiveDB {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	firstIID := int64(1)
	e := interlockLiveDB{ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), repoID: uuid.New(), nextIID: &firstIID}
	connID := uuid.New()
	e.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		e.userID, fmt.Sprintf("interlock-%s@e2e", e.userID))
	e.exec(t, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	           VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, e.userID, []byte("x"))
	e.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	           VALUES ($1, $2, 1, 'g/interlock', 'https://forge.e2e/g/interlock', 'main', true)`, e.repoID, connID)
	return e
}

func (e interlockLiveDB) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedWorker inserts a worker with the given protocol capabilities (the workers column the
// D2 hard claim clause reads). protocolCaps nil ⇒ the '{}' default (implements no protocol).
func (e interlockLiveDB) seedWorker(t *testing.T, protocolCaps []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if protocolCaps == nil {
		protocolCaps = []string{}
	}
	e.exec(t, `INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities)
	           VALUES ($1, $2, $3, $4, 'online', $5)`,
		id, e.userID, "w-"+id.String()[:8], id[:], protocolCaps)
	return id
}

// seedQueuedRun inserts a queued issue run. contractVersion nil ⇒ a LEGACY run
// (completion_contract_version NULL); non-nil ⇒ an INTERLOCKED run stamped at that version.
func (e interlockLiveDB) seedQueuedRun(t *testing.T, contractVersion *int32, requiredCaps []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if requiredCaps == nil {
		requiredCaps = []string{}
	}
	var cv any
	if contractVersion != nil {
		cv = *contractVersion
	}
	iid := *e.nextIID
	*e.nextIID++
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id, required_capabilities, completion_contract_version)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'queued', NULL, $5, $6)`,
		id, e.userID, e.repoID, iid, requiredCaps, cv)
	return id
}

func (e interlockLiveDB) runStatus(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&s); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	return s
}

// claimParams builds ClaimRunParams for a single-worker claim attempt with the given
// protocol caps and capability_aware flag. Cutoffs are set so neither affinity nor
// fleet-spread interferes: the run is unclaimed (worker_id NULL) so affinity passes, and
// seeded workers carry NULL max_concurrent_runs so no peer ever qualifies as a spread
// target. WorkerCaps empty + capability_aware false makes fn_worker_can_claim trivially
// true, isolating the D2 protocol clause as the ONLY thing that can block.
func (e interlockLiveDB) claimParams(workerID uuid.UUID, protocolCaps []string, capAware bool) store.ClaimRunParams {
	now := time.Now()
	return store.ClaimRunParams{
		WorkerID:              pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:                e.userID,
		HeartbeatCutoff:       pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		AffinityCutoff:        pgtype.Timestamptz{Time: now.Add(-25 * time.Minute), Valid: true},
		SpreadCutoff:          pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		WorkerCaps:            []string{},
		CapabilityAware:       capAware,
		WorkerProtocolCaps:    protocolCaps,
	}
}

// TestClaimInterlockHardClauseBlocksIncapableWorkerLiveDB is the core D2 assertion: a fresh
// INTERLOCKED run is NOT claimable by a worker whose protocol_capabilities lacks
// completion_interlock_v1 — and NEITHER the capability_aware kill-switch (false) NOR the
// owner ClearRunRequiredCapabilities override can bypass it. A CAPABLE worker then claims it.
func TestClaimInterlockHardClauseBlocksIncapableWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)

	incapable := e.seedWorker(t, nil)                               // protocol_capabilities '{}'
	capable := e.seedWorker(t, []string{"completion_interlock_v1"}) // implements the protocol
	v1 := int32(1)
	runID := e.seedQueuedRun(t, &v1, nil) // interlocked, required_capabilities '{}'

	// (1) capability_aware=false does NOT bypass: the incapable worker still cannot claim.
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(incapable, []string{}, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("interlocked run claimed by an incapable worker with capability_aware=false (err=%v); the D2 clause must block regardless of the kill-switch", err)
	}
	if s := e.runStatus(t, runID); s != "queued" {
		t.Fatalf("run must stay queued after the blocked claim; status = %q", s)
	}

	// (2) the owner override (ClearRunRequiredCapabilities) does NOT bypass either. Exercise
	// the real override: park the run at awaiting_approval, clear its required_capabilities,
	// then re-queue and re-attempt. The interlock clause is OUTSIDE required_capabilities, so
	// clearing it changes nothing for the incapable worker.
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
		t.Fatalf("interlocked run claimed by an incapable worker AFTER ClearRunRequiredCapabilities (err=%v); the D2 clause is outside the override and must still block", err)
	}
	if s := e.runStatus(t, runID); s != "queued" {
		t.Fatalf("run must stay queued after the override+blocked claim; status = %q", s)
	}

	// (3) a CAPABLE worker claims it — the clause admits a worker that implements the protocol.
	run, err := e.q.ClaimRun(e.ctx, e.claimParams(capable, []string{"completion_interlock_v1"}, false))
	if err != nil {
		t.Fatalf("capable worker must claim the interlocked run: %v", err)
	}
	if run.ID != runID {
		t.Fatalf("capable worker claimed %v, want %v", run.ID, runID)
	}
	if run.Status != "claimed" {
		t.Fatalf("claimed run status = %q, want claimed", run.Status)
	}
}

// TestClaimLegacyRunClaimableByAnyWorkerLiveDB: a LEGACY run
// (completion_contract_version NULL) remains claimable by ANY worker, including one with no
// protocol capabilities — the D2 clause exempts legacy rows by the NULL discriminator.
func TestClaimLegacyRunClaimableByAnyWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	incapable := e.seedWorker(t, nil)
	runID := e.seedQueuedRun(t, nil, nil) // legacy: completion_contract_version NULL

	run, err := e.q.ClaimRun(e.ctx, e.claimParams(incapable, []string{}, false))
	if err != nil {
		t.Fatalf("legacy run must be claimable by any worker: %v", err)
	}
	if run.ID != runID {
		t.Fatalf("claimed %v, want %v", run.ID, runID)
	}
}

// TestCompletionContractFreezesAtApprovalLiveDB proves the human-path freeze
// (CreateApprovePlanInput) writes the contract + revision atomically and IDEMPOTENTLY for an
// interlocked run, and NEVER for a legacy run.
func TestCompletionContractFreezesAtApprovalLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	candidate := []byte(`[{"id":"m1","title":"First"},{"id":"m2","title":"Second"}]`)

	// Interlocked run at awaiting_approval with a candidate list, contract not yet frozen.
	v1 := int32(1)
	runID := e.seedQueuedRun(t, &v1, nil)
	e.exec(t, `UPDATE runs SET status = 'awaiting_approval', milestones_candidate = $2 WHERE id = $1`, runID, candidate)

	contract, err := buildCompletionContract(candidate)
	if err != nil {
		t.Fatalf("buildCompletionContract: %v", err)
	}
	approveParams := store.CreateApprovePlanInputParams{
		RunID: runID, Body: pgtype.Text{String: "{}", Valid: true},
		CompletionContract:       contract,
		RunMaxIterations:         5,
		MilestoneBudgetCap:       milestoneBudgetCap,
		RunTimeoutSeconds:        7200,
		BudgetWallCeilingSeconds: budgetWallCeilingSeconds,
	}
	if _, err := e.q.CreateApprovePlanInput(e.ctx, approveParams); err != nil {
		t.Fatalf("CreateApprovePlanInput: %v", err)
	}
	gotContract, gotRev := e.readContract(t, runID)
	assertContractEquals(t, gotContract, contract)
	if gotRev == nil || *gotRev != 1 {
		t.Fatalf("contract_revision = %v, want 1", gotRev)
	}

	// Idempotent: a second approve with a DIFFERENT contract must not re-freeze.
	other, _ := buildCompletionContract([]byte(`[{"id":"z9","title":"Different"}]`))
	approveParams.CompletionContract = other
	if _, err := e.q.CreateApprovePlanInput(e.ctx, approveParams); err != nil {
		t.Fatalf("CreateApprovePlanInput (re-approve): %v", err)
	}
	reContract, reRev := e.readContract(t, runID)
	assertContractEquals(t, reContract, contract) // still the ORIGINAL
	if reRev == nil || *reRev != 1 {
		t.Fatalf("contract_revision after re-approve = %v, want unchanged 1", reRev)
	}

	// Legacy run: the freeze must NOT fire even when a contract is passed.
	legacyID := e.seedQueuedRun(t, nil, nil)
	e.exec(t, `UPDATE runs SET status = 'awaiting_approval', milestones_candidate = $2 WHERE id = $1`, legacyID, candidate)
	legacyParams := approveParams
	legacyParams.RunID = legacyID
	legacyParams.CompletionContract = contract
	if _, err := e.q.CreateApprovePlanInput(e.ctx, legacyParams); err != nil {
		t.Fatalf("CreateApprovePlanInput (legacy): %v", err)
	}
	lc, lr := e.readContract(t, legacyID)
	if lc != nil {
		t.Fatalf("legacy run froze a contract (%s); a NULL version must never freeze", lc)
	}
	if lr != nil {
		t.Fatalf("legacy run set contract_revision (%v); a NULL version must never freeze", lr)
	}
}

// TestCompletionContractFreezesAtRunningReportLiveDB proves the autopilot-path freeze
// (SetRunRunning) writes the contract + revision atomically and IDEMPOTENTLY for an
// interlocked run, and NEVER for a legacy run.
func TestCompletionContractFreezesAtRunningReportLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	frozen := []byte(`[{"id":"m1","title":"Alpha"},{"id":"m2","title":"Beta"}]`)
	worker := e.seedWorker(t, []string{"completion_interlock_v1"})

	// Interlocked run at 'claimed' owned by the worker (the autopilot claimed→running path).
	v1 := int32(1)
	runID := e.seedQueuedRun(t, &v1, nil)
	e.exec(t, `UPDATE runs SET status = 'claimed', worker_id = $2 WHERE id = $1`, runID, worker)

	contract, err := buildCompletionContract(frozen)
	if err != nil {
		t.Fatalf("buildCompletionContract: %v", err)
	}
	runningParams := store.SetRunRunningParams{
		IterationCount:           1,
		MilestonesFrozen:         frozen,
		CompletionContract:       contract,
		RunMaxIterations:         5,
		MilestoneBudgetCap:       milestoneBudgetCap,
		RunTimeoutSeconds:        7200,
		BudgetWallCeilingSeconds: budgetWallCeilingSeconds,
		ID:                       runID,
		WorkerID:                 pgtype.UUID{Bytes: worker, Valid: true},
	}
	rows, err := e.q.SetRunRunning(e.ctx, runningParams)
	if err != nil {
		t.Fatalf("SetRunRunning: %v", err)
	}
	if rows != 1 {
		t.Fatalf("SetRunRunning affected %d rows, want 1", rows)
	}
	gotContract, gotRev := e.readContract(t, runID)
	assertContractEquals(t, gotContract, contract)
	if gotRev == nil || *gotRev != 1 {
		t.Fatalf("contract_revision = %v, want 1", gotRev)
	}

	// Idempotent: a second running report carrying a DIFFERENT contract must not re-freeze.
	other, _ := buildCompletionContract([]byte(`[{"id":"z9","title":"Different"}]`))
	runningParams.CompletionContract = other
	if _, err := e.q.SetRunRunning(e.ctx, runningParams); err != nil {
		t.Fatalf("SetRunRunning (heartbeat): %v", err)
	}
	reContract, reRev := e.readContract(t, runID)
	assertContractEquals(t, reContract, contract) // still the ORIGINAL
	if reRev == nil || *reRev != 1 {
		t.Fatalf("contract_revision after heartbeat = %v, want unchanged 1", reRev)
	}

	// Legacy run: the freeze must NOT fire even when a contract is passed.
	legacyWorker := e.seedWorker(t, []string{"completion_interlock_v1"})
	legacyID := e.seedQueuedRun(t, nil, nil)
	e.exec(t, `UPDATE runs SET status = 'claimed', worker_id = $2 WHERE id = $1`, legacyID, legacyWorker)
	legacyParams := runningParams
	legacyParams.ID = legacyID
	legacyParams.WorkerID = pgtype.UUID{Bytes: legacyWorker, Valid: true}
	legacyParams.CompletionContract = contract
	if _, err := e.q.SetRunRunning(e.ctx, legacyParams); err != nil {
		t.Fatalf("SetRunRunning (legacy): %v", err)
	}
	lc, lr := e.readContract(t, legacyID)
	if lc != nil {
		t.Fatalf("legacy run froze a contract (%s); a NULL version must never freeze", lc)
	}
	if lr != nil {
		t.Fatalf("legacy run set contract_revision (%v); a NULL version must never freeze", lr)
	}
}

// TestCompletionContractNotFrozenBeforeMilestonesLiveDB is the premature-freeze regression:
// an autopilot run reports `running` at claim time BEFORE it resolves any milestones. That
// early report must NOT freeze the contract (there is nothing to freeze yet), and a LATER
// report that carries the resolved list must then freeze the REAL contract. Without the
// milestone-source guard on the freeze, the early report would lock an empty contract the
// later report could never correct.
func TestCompletionContractNotFrozenBeforeMilestonesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	worker := e.seedWorker(t, []string{"completion_interlock_v1"})
	v1 := int32(1)
	runID := e.seedQueuedRun(t, &v1, nil)
	e.exec(t, `UPDATE runs SET status = 'claimed', worker_id = $2 WHERE id = $1`, runID, worker)

	base := store.SetRunRunningParams{
		IterationCount:           1,
		RunMaxIterations:         5,
		MilestoneBudgetCap:       milestoneBudgetCap,
		RunTimeoutSeconds:        7200,
		BudgetWallCeilingSeconds: budgetWallCeilingSeconds,
		ID:                       runID,
		WorkerID:                 pgtype.UUID{Bytes: worker, Valid: true},
	}

	// (1) claim-time report: NO milestones, NO contract. The freeze must not fire.
	if _, err := e.q.SetRunRunning(e.ctx, base); err != nil {
		t.Fatalf("SetRunRunning (pre-milestone): %v", err)
	}
	if c, r := e.readContract(t, runID); c != nil || r != nil {
		t.Fatalf("contract froze before milestones resolved (contract=%s revision=%v); the milestone-source guard must defer the freeze", c, r)
	}

	// (2) the report that resolves the milestone list freezes the REAL contract.
	frozen := []byte(`[{"id":"m1","title":"Resolved late"}]`)
	contract, err := buildCompletionContract(frozen)
	if err != nil {
		t.Fatalf("buildCompletionContract: %v", err)
	}
	withMs := base
	withMs.MilestonesFrozen = frozen
	withMs.CompletionContract = contract
	if _, err := e.q.SetRunRunning(e.ctx, withMs); err != nil {
		t.Fatalf("SetRunRunning (milestones): %v", err)
	}
	gotContract, gotRev := e.readContract(t, runID)
	assertContractEquals(t, gotContract, contract)
	if gotRev == nil || *gotRev != 1 {
		t.Fatalf("contract_revision after the milestone report = %v, want 1", gotRev)
	}
}

// readContract returns the run's frozen completion_contract jsonb (nil when NULL) and
// contract_revision (nil when NULL).
func (e interlockLiveDB) readContract(t *testing.T, runID uuid.UUID) ([]byte, *int32) {
	t.Helper()
	var contract []byte
	var rev pgtype.Int4
	if err := e.pool.QueryRow(e.ctx,
		`SELECT completion_contract, contract_revision FROM runs WHERE id = $1`, runID).Scan(&contract, &rev); err != nil {
		t.Fatalf("read contract: %v", err)
	}
	if rev.Valid {
		v := rev.Int32
		return contract, &v
	}
	return contract, nil
}

// assertContractEquals compares two contract jsonb blobs by semantic (unmarshaled) equality,
// so a whitespace/key-order difference from the Postgres round-trip is not a false failure.
func assertContractEquals(t *testing.T, got, want []byte) {
	t.Helper()
	if got == nil {
		t.Fatalf("contract is NULL, want %s", want)
	}
	var g, w completionContract
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got contract %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want contract %s: %v", want, err)
	}
	gj, _ := json.Marshal(g)
	wj, _ := json.Marshal(w)
	if string(gj) != string(wj) {
		t.Fatalf("frozen contract = %s, want %s", gj, wj)
	}
}
