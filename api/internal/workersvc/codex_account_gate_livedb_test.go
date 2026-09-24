package workersvc

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_account_gate_livedb_test.go pins PRD #1590 M2 (D2, amendment A1) behaviourally against a
// real Postgres: ClaimRun's Codex account gate, the park_codex_account_unavailable sweeper pass
// and the health projection. It replaces the former text scan of the gate (the store package
// keeps only the copies-identical test). Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres; run via ./e2e/run-store-it.sh.
//
// The claim-versus-quarantine race that reaches assembly is M1's
// TestClaimObservedCaseReplayParksLiveDB (claim_finish_livedb_test.go): the quarantine lands
// after ClaimRun's gate and the exact-claim transaction parks the run.

// uuidPredecessor returns id - 1 in the 128-bit order Postgres compares uuids in, so a keyset
// page after it starts exactly at id.
func uuidPredecessor(id uuid.UUID) uuid.UUID {
	out := id
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] > 0 {
			out[i]--
			return out
		}
		out[i] = 0xff
	}
	return out
}

// holdCaseFixture is one codexHoldTable row seeded as a queued run of a fresh owner.
type holdCaseFixture struct {
	userID, workerID, runID uuid.UUID
}

// seedHoldCase seeds the baseline (a queued run bound to a linked, idle subscription login, with its
// identity frozen) and applies the row's relative changes with plain UPDATEs.
func seedHoldCase(t *testing.T, env codexTestEnv, tc codexHoldCase) holdCaseFixture {
	t.Helper()
	userID, workerID, repoID := env.seedCodexInfra(t)
	env.exec(`UPDATE workers SET protocol_capabilities = $2, last_heartbeat_at = now() WHERE id = $1`,
		workerID, []string{capability.CodexHarnessV1, capability.CodexCustomModelV1})
	fx := holdCaseFixture{userID: userID, workerID: workerID}
	if tc.alias == "static" {
		aliasID := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
		fx.runID = uuid.New()
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
			VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'queued')`, fx.runID, userID, repoID)
		svc := &Service{q: env.q, box: env.box}
		if err := svc.FreezeCodexBinding(env.ctx, userID, fx.runID, aliasID, codexAuthModeAPIKey); err != nil {
			t.Fatalf("freeze api_key binding: %v", err)
		}
		return fx
	}
	aliasID := env.seedLinkedSubscription(t, userID, "codex-sub-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	fx.runID = env.seedQueuedCodexRun(t, userID, repoID, aliasID, 1)
	var accountID uuid.UUID
	if err := env.pool.QueryRow(env.ctx, `SELECT provider_account_id FROM codex_credential_state
		WHERE user_secret_id = $1`, aliasID).Scan(&accountID); err != nil {
		t.Fatalf("read linked account: %v", err)
	}
	if tc.unfrozen {
		env.exec(`UPDATE runs SET codex_account_key = NULL, codex_account_revision = NULL WHERE id = $1`, fx.runID)
	}
	switch tc.account {
	case "":
		env.exec(`UPDATE codex_credential_state SET status = $2, provider_account_id = NULL
			WHERE user_secret_id = $1`, aliasID, tc.alias)
	case "other":
		otherAlias := env.seedLinkedSubscription(t, userID, "codex-other-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
		if err := env.pool.QueryRow(env.ctx, `SELECT provider_account_id FROM codex_credential_state
			WHERE user_secret_id = $1`, otherAlias).Scan(&accountID); err != nil {
			t.Fatalf("read other account: %v", err)
		}
		env.exec(`UPDATE codex_credential_state SET provider_account_id = $2 WHERE user_secret_id = $1`, aliasID, accountID)
	}
	if tc.materialAhead {
		env.exec(`UPDATE codex_credential_state SET material_revision = material_revision + 1
			WHERE user_secret_id = $1`, aliasID)
	}
	if tc.account != "" {
		if tc.revBumped {
			env.exec(`UPDATE codex_provider_account SET credential_revision = credential_revision + 1 WHERE id = $1`, accountID)
		}
		setCoordState(t, env, accountID, tc.coord)
	}
	if tc.mode == codexAuthModeAPIKey {
		env.exec(`UPDATE runs SET codex_auth_mode = 'api_key' WHERE id = $1`, fx.runID)
	}
	if tc.harness == "claude" {
		// runs_codex_harness_coherence_check forbids a Claude run carrying a Codex binding, so
		// the row is a Claude run of an owner whose Codex login is on hold.
		env.exec(`UPDATE runs SET harness = 'claude', codex_secret_id = NULL, codex_auth_mode = NULL,
			codex_secret_label = NULL, codex_account_key = NULL, codex_material_revision = NULL,
			codex_account_revision = NULL WHERE id = $1`, fx.runID)
	}
	if tc.kind == "chat" {
		// runs_kind_shape: a chat run carries no repo, issue or branch.
		env.exec(`UPDATE runs SET kind = 'chat', repo_id = NULL, issue_iid = NULL, branch = NULL WHERE id = $1`, fx.runID)
	}
	return fx
}

// setCoordState moves an account to coord (an in_progress lease carries its operation and a live
// deadline, as a real refresh does).
func setCoordState(t *testing.T, env codexTestEnv, accountID uuid.UUID, coord string) {
	t.Helper()
	if coord == "in_progress" {
		env.exec(`UPDATE codex_provider_account SET coord_state = 'in_progress',
			coord_operation_id = gen_random_uuid(), lease_deadline = now() + interval '10 minutes'
			WHERE id = $1`, accountID)
		return
	}
	env.exec(`UPDATE codex_provider_account SET coord_state = $2 WHERE id = $1`, accountID, coord)
}

// claimableInRolledBackTx reports whether ClaimRun would hand the run to the worker, then rolls the
// claim back so the fixture is untouched.
func claimableInRolledBackTx(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID) bool {
	t.Helper()
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	run, err := store.New(tx).ClaimRun(env.ctx, claimRunParamsFor(wkrRow(t, env, workerID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if run.ID != runID {
		t.Fatalf("ClaimRun claimed %s, want the fixture run %s", run.ID, runID)
	}
	return true
}

func healthRowForRun(t *testing.T, env codexTestEnv, runID uuid.UUID) store.ListActiveRunsForHealthRow {
	t.Helper()
	rows, err := env.q.ListActiveRunsForHealth(env.ctx, codexCuratedModelsSlice())
	if err != nil {
		t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	for _, r := range rows {
		if r.ID == runID {
			return r
		}
	}
	t.Fatalf("run %s not in ListActiveRunsForHealth", runID)
	return store.ListActiveRunsForHealthRow{}
}

// TestCodexHoldTableSQLLiveDB drives the SQL half of the shared fixture table
// (codex_account_hold_table_test.go): for every row, ClaimRun's gate, the park page and the health
// projection agree with the expected hold, which TestCodexHoldTableGoClassifier pins for the Go
// classifier.
func TestCodexHoldTableSQLLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, tc := range codexHoldTable {
		t.Run(tc.name, func(t *testing.T) {
			fx := seedHoldCase(t, env, tc)
			if tc.runKind() != "chat" { // chat is neither run-lane claimable nor health-listed
				if got := healthRowForRun(t, env, fx.runID).CodexAccountGated; got != tc.hold {
					t.Fatalf("codex_account_gated = %v, want %v", got, tc.hold)
				}
				if got := claimableInRolledBackTx(t, env, fx.workerID, fx.runID); got == tc.hold {
					t.Fatalf("ClaimRun claimable = %v, want %v", got, !tc.hold)
				}
			}
			run := mustRun(t, env, fx.runID)
			if run.Harness != harnessCodex || run.CodexAuthMode.String != codexAuthModeSubscription {
				// Outside the page's partial index by construction: never examined, never parked.
				if tc.hold || run.Status != "queued" {
					t.Fatalf("a non-candidate row: hold=%v status=%s", tc.hold, run.Status)
				}
				return
			}
			// A one-row page that starts exactly at this run, so no other run is examined.
			row, err := env.q.ParkQueuedCodexAccountUnavailablePage(env.ctx, store.ParkQueuedCodexAccountUnavailablePageParams{
				AfterID: uuidPredecessor(fx.runID), PageCap: 1,
			})
			if err != nil {
				t.Fatalf("park page: %v", err)
			}
			if row.PageSize != 1 || row.LastScannedID != fx.runID {
				t.Fatalf("page = (%d, %s), want exactly this run examined", row.PageSize, row.LastScannedID)
			}
			parked := false
			for _, id := range row.ParkedIds {
				parked = parked || id == fx.runID
			}
			if parked != tc.hold {
				t.Fatalf("parked = %v, want %v", parked, tc.hold)
			}
			after := mustRun(t, env, fx.runID)
			wantStatus := "queued"
			if tc.hold {
				wantStatus = "recovery_wait"
			}
			if after.Status != wantStatus {
				t.Fatalf("status after park page = %s, want %s", after.Status, wantStatus)
			}
		})
	}
}

// gateSweepService returns a Sweep-capable Service whose park page covers every queued Codex
// subscription run in the shared test database, so a fixture run is always examined on the
// first tick regardless of the leftovers of other tests.
func gateSweepService(env codexTestEnv, svc *Service) *Service {
	svc.codexPark.capOverride = 1 << 20
	svc.SetReadyAt(time.Now())
	return svc
}

// assertCodexAccountParked checks the sweeper park's field set against the queued row it
// parked: claim_generation, worker_id and holds untouched.
func assertCodexAccountParked(t *testing.T, env codexTestEnv, runID uuid.UUID, before store.Run) {
	t.Helper()
	r := mustRun(t, env, runID)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != recoveryCauseCodexAccountUnavailable {
		t.Fatalf("status=%s cause=%v, want recovery_wait/codex_account_unavailable", r.Status, r.RecoveryWaitCause)
	}
	if r.FailOrigin.Valid || r.FailureReason.Valid {
		t.Fatalf("park carries fail_origin=%v failure_reason=%v", r.FailOrigin, r.FailureReason)
	}
	if r.ClaimGeneration != before.ClaimGeneration || r.WorkerID != before.WorkerID {
		t.Fatalf("generation %d worker %v, want unchanged %d %v", r.ClaimGeneration, r.WorkerID, before.ClaimGeneration, before.WorkerID)
	}
	if r.CodexClaimEpoch != before.CodexClaimEpoch+1 || len(r.CodexCapHash) != 0 {
		t.Fatalf("epoch %d (before %d) hash %x, want epoch+1 and no capability", r.CodexClaimEpoch, before.CodexClaimEpoch, r.CodexCapHash)
	}
	if r.StartedAt.Valid || r.BudgetPausedSeconds != 0 || r.RecoveryRetryNotBefore.Valid || r.Health != "ok" || r.HealthReason.Valid {
		t.Fatalf("started_at=%v paused=%d retry=%v health=%s/%v, want a reset wall and clean health",
			r.StartedAt, r.BudgetPausedSeconds, r.RecoveryRetryNotBefore, r.Health, r.HealthReason)
	}
}

func countRunHolds(t *testing.T, env codexTestEnv, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestClaimQuarantinedAccountNeverClaimedLiveDB: a queued run whose account is quarantined is never
// handed out by the real Service.Claim: idle, no generation, no hold. It is the regression that
// fails with the ClaimRun gate removed (the claim then advances to generation 2 and opens a hold).
func TestClaimQuarantinedAccountNeverClaimedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	fx.setAccount(t, "coord_state = 'quarantined'")
	before := mustRun(t, env, fx.runID)
	holds := countRunHolds(t, env, fx.runID)
	for _, snap := range []*ActiveSnapshot{nil, {SnapshotEpoch: 1, RegisterNonce: "nonce-B", Active: []ActiveRunEntry{}}} {
		payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), snap)
		if err != nil || payload != nil {
			t.Fatalf("Claim = (%v, %v), want idle", payload != nil, err)
		}
	}
	r := mustRun(t, env, fx.runID)
	if r.Status != "queued" || r.ClaimGeneration != before.ClaimGeneration || r.CodexClaimEpoch != before.CodexClaimEpoch {
		t.Fatalf("status=%s gen=%d epoch=%d, want queued at gen %d epoch %d", r.Status, r.ClaimGeneration,
			r.CodexClaimEpoch, before.ClaimGeneration, before.CodexClaimEpoch)
	}
	if got := countRunHolds(t, env, fx.runID); got != holds {
		t.Fatalf("holds = %d, want %d (no hold opened)", got, holds)
	}
}

// TestClaimInProgressAccountStillClaimableLiveDB: a live refresh lease (coord_state in_progress)
// is not a hold (D1); the run is claimed at generation 2 and delivered.
func TestClaimInProgressAccountStillClaimableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	setCoordState(t, env, fx.accountID, "in_progress")
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil || payload == nil || payload.Secrets.Codex == nil {
		t.Fatalf("Claim = (%v, %v), want a Codex payload", payload != nil, err)
	}
	if r := mustRun(t, env, fx.runID); r.Status != "claimed" || r.ClaimGeneration != 2 {
		t.Fatalf("status=%s gen=%d, want claimed at 2", r.Status, r.ClaimGeneration)
	}
}

// TestCodexAccountHoldNotClaimedThenParkedLiveDB covers the two re-login holds on a run past first
// link: a re-login in flight (the owner's PATCH, BumpCodexMaterialRevision to staging, clears the
// link) and A1's same-identity relink landing before the park tick. Each run is not claimed, the
// sweeper parks it, and a further claim stays idle (it never fails on
// ErrCodexMaterialRevisionStale).
func TestCodexAccountHoldNotClaimedThenParkedLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, fx *codexClaimFix)
	}{
		{"re-login in flight", func(t *testing.T, fx *codexClaimFix) {
			n, err := fx.env.q.BumpCodexMaterialRevision(fx.env.ctx, store.BumpCodexMaterialRevisionParams{
				UserSecretID: fx.aliasID, UserID: fx.userID, Status: "staging",
			})
			if err != nil || n != 1 {
				t.Fatalf("BumpCodexMaterialRevision = (%d, %v)", n, err)
			}
		}},
		{"A1 same-identity relink", func(t *testing.T, fx *codexClaimFix) {
			fx.env.exec(`UPDATE codex_credential_state SET material_revision = material_revision + 1
				WHERE user_secret_id = $1`, fx.aliasID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			tc.mutate(t, fx)
			holds := countRunHolds(t, env, fx.runID)
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want idle", payload != nil, err)
			}
			before := mustRun(t, env, fx.runID)
			if before.Status != "queued" || before.ClaimGeneration != 1 {
				t.Fatalf("status=%s gen=%d, want queued at 1", before.Status, before.ClaimGeneration)
			}
			svc := gateSweepService(env, fx.svc)
			res, err := svc.Sweep(env.ctx)
			if err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			if res.CodexAccountParked < 1 {
				t.Fatalf("CodexAccountParked = %d, want >= 1", res.CodexAccountParked)
			}
			assertCodexAccountParked(t, env, fx.runID, before)
			if payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil); err != nil || payload != nil {
				t.Fatalf("Claim after park = (%v, %v), want idle", payload != nil, err)
			}
			r := mustRun(t, env, fx.runID)
			if r.Status != "recovery_wait" || r.FailOrigin.Valid {
				t.Fatalf("status=%s origin=%v, want still held", r.Status, r.FailOrigin)
			}
			if got := countRunHolds(t, env, fx.runID); got != holds {
				t.Fatalf("holds = %d, want %d", got, holds)
			}
		})
	}
}

// TestClaimRelinkToDifferentIdentityIsClaimableLiveDB: a relink to a DIFFERENT identity is not a
// hold. The gate lets the claim through and the existing terminal classification fails it
// (credential_unavailable) at generation 2.
func TestClaimRelinkToDifferentIdentityIsClaimableLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	other := env.seedLinkedSubscription(t, fx.userID, "codex-other-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	env.exec(`UPDATE codex_credential_state s SET material_revision = s.material_revision + 1,
		provider_account_id = o.provider_account_id
		FROM codex_credential_state o WHERE s.user_secret_id = $1 AND o.user_secret_id = $2`, fx.aliasID, other)
	if healthRowForRun(t, env, fx.runID).CodexAccountGated {
		t.Fatal("a different-identity relink must not be gated")
	}
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	r := mustRun(t, env, fx.runID)
	if r.ClaimGeneration != 2 || r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" {
		t.Fatalf("gen=%d status=%s origin=%v, want claimed at 2 then failed credential_unavailable",
			r.ClaimGeneration, r.Status, r.FailOrigin)
	}
}

// TestUnfrozenQuarantinedRunFailsNotHeldLiveDB: a subscription run with no frozen identity
// (codex_account_key NULL: nothing freezes it after create) on a quarantined account can never
// pass evalCodexReleasePredicate (ErrCodexAccountKeyUnfrozen), so D1 does not hold it. It is not
// gated, the park page examines it and leaves it queued, and the claim fails it
// credential_unavailable with the unfrozen error, as before M2, instead of parking it in a hold
// the promoter could never release.
func TestUnfrozenQuarantinedRunFailsNotHeldLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	env.exec(`UPDATE runs SET codex_account_key = NULL, codex_account_revision = NULL WHERE id = $1`, fx.runID)
	fx.setAccount(t, "coord_state = 'quarantined'")
	if healthRowForRun(t, env, fx.runID).CodexAccountGated {
		t.Fatal("an unfrozen run must not be gated")
	}
	svc := gateSweepService(env, fx.svc)
	svc.codexPark.after, svc.codexPark.capOverride = uuidPredecessor(fx.runID), 1
	if n, err := svc.parkCodexAccountUnavailable(env.ctx); err != nil || n != 0 {
		t.Fatalf("park = (%d, %v), want (0, nil)", n, err)
	}
	if r := mustRun(t, env, fx.runID); r.Status != "queued" || r.RecoveryWaitCause.Valid {
		t.Fatalf("after the park page: status=%s cause=%v, want still queued", r.Status, r.RecoveryWaitCause)
	}
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	r := mustRun(t, env, fx.runID)
	if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" || r.RecoveryWaitCause.Valid ||
		!strings.Contains(r.FailureReason.String, ErrCodexAccountKeyUnfrozen.Error()) {
		t.Fatalf("status=%s origin=%v cause=%v reason=%q, want failed/credential_unavailable on the unfrozen key",
			r.Status, r.FailOrigin, r.RecoveryWaitCause, r.FailureReason.String)
	}
}

// TestCodexAccountHoldNoClaimRequeueLoopLiveDB: N Sweep + Claim iterations on a quarantined
// account's run keep claim_generation and the hold count constant; the run parks once and stays.
func TestCodexAccountHoldNoClaimRequeueLoopLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	fx.setAccount(t, "coord_state = 'quarantined'")
	svc := gateSweepService(env, fx.svc)
	gen := mustRun(t, env, fx.runID).ClaimGeneration
	holds := countRunHolds(t, env, fx.runID)
	var epoch int64
	for i := 0; i < 5; i++ {
		if _, err := svc.Sweep(env.ctx); err != nil {
			t.Fatalf("iteration %d Sweep: %v", i, err)
		}
		if payload, err := svc.Claim(env.ctx, fx.claimant(t, true), nil); err != nil || payload != nil {
			t.Fatalf("iteration %d Claim = (%v, %v), want idle", i, payload != nil, err)
		}
		r := mustRun(t, env, fx.runID)
		if r.Status != "recovery_wait" || r.ClaimGeneration != gen || countRunHolds(t, env, fx.runID) != holds {
			t.Fatalf("iteration %d: status=%s gen=%d holds=%d, want recovery_wait gen %d holds %d",
				i, r.Status, r.ClaimGeneration, countRunHolds(t, env, fx.runID), gen, holds)
		}
		if i > 0 && r.CodexClaimEpoch != epoch {
			t.Fatalf("iteration %d: epoch moved %d -> %d, want a single park", i, epoch, r.CodexClaimEpoch)
		}
		epoch = r.CodexClaimEpoch
	}
}

// pageRecordingStore records each park page the service requests, to assert rows EXAMINED.
type pageRecordingStore struct {
	*store.Queries
	pages []store.ParkQueuedCodexAccountUnavailablePageRow
	after []uuid.UUID
}

func (p *pageRecordingStore) ParkQueuedCodexAccountUnavailablePage(ctx context.Context, arg store.ParkQueuedCodexAccountUnavailablePageParams) (store.ParkQueuedCodexAccountUnavailablePageRow, error) {
	row, err := p.Queries.ParkQueuedCodexAccountUnavailablePage(ctx, arg)
	if err == nil {
		p.pages = append(p.pages, row)
		p.after = append(p.after, arg.AfterID)
	}
	return row, err
}

func countQueuedCodexSubscription(t *testing.T, env codexTestEnv) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE status = 'queued'
		AND harness = 'codex' AND codex_auth_mode = 'subscription'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestParkCodexAccountSparseQueueBoundedLiveDB: 250 queued Codex subscription runs, 3 of them
// gated, spread through the id space with one BEHIND the cursor's start, and a page cap of 50.
// Every tick examines at most cap rows (the returned page size, not just the rows updated), and
// every gated run parks within ceil(N/cap)+1 ticks, the one behind the cursor after the wrap.
func TestParkCodexAccountSparseQueueBoundedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	healthy := env.seedLinkedSubscription(t, userID, "codex-ok-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	gated := env.seedLinkedSubscription(t, userID, "codex-q-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	env.exec(`UPDATE codex_provider_account SET coord_state = 'quarantined' WHERE id =
		(SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1)`, gated)
	// Templates carry each alias's real frozen binding; the bulk rows copy it, then the templates go.
	tplOK := env.seedQueuedCodexRun(t, userID, repoID, healthy, 100000)
	tplQ := env.seedQueuedCodexRun(t, userID, repoID, gated, 100001)

	const total, pageCap = 250, 50
	ids := make([]uuid.UUID, total)
	for i := range ids {
		ids[i] = uuid.New()
	}
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	eligible := map[uuid.UUID]bool{ids[10]: true, ids[120]: true, ids[240]: true}
	cursorStart := ids[60] // ids[10] is behind it
	var gatedIDs, okIDs []uuid.UUID
	for _, id := range ids {
		if eligible[id] {
			gatedIDs = append(gatedIDs, id)
		} else {
			okIDs = append(okIDs, id)
		}
	}
	insert := func(runIDs []uuid.UUID, template uuid.UUID, iidBase int) {
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status,
			harness, codex_secret_id, codex_auth_mode, codex_secret_label, codex_account_key,
			codex_material_revision, codex_account_revision)
			SELECT n.id, t.user_id, t.repo_id, 'issue', $3 + n.ord, 't', 'd', 'queued',
			       t.harness, t.codex_secret_id, t.codex_auth_mode, t.codex_secret_label, t.codex_account_key,
			       t.codex_material_revision, t.codex_account_revision
			FROM unnest($1::uuid[]) WITH ORDINALITY AS n(id, ord), runs t WHERE t.id = $2`,
			runIDs, template, iidBase)
	}
	insert(okIDs, tplOK, 1000)
	insert(gatedIDs, tplQ, 5000)
	env.exec(`DELETE FROM runs WHERE id = ANY($1)`, []uuid.UUID{tplOK, tplQ})

	rec := &pageRecordingStore{Queries: env.q}
	svc := New(rec, env.box, testParams())
	svc.codexPark.capOverride = pageCap
	svc.codexPark.after = cursorStart
	n := countQueuedCodexSubscription(t, env) // includes other tests' leftovers
	var ahead int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE status = 'queued'
		AND harness = 'codex' AND codex_auth_mode = 'subscription' AND id > $1`, cursorStart).Scan(&ahead); err != nil {
		t.Fatal(err)
	}
	if ahead < pageCap {
		t.Fatalf("only %d queued Codex subscription runs ahead of the cursor, want >= %d", ahead, pageCap)
	}
	bound := (n+pageCap-1)/pageCap + 1
	wrapped := false
	for tick := 1; tick <= bound; tick++ {
		if _, err := svc.parkCodexAccountUnavailable(env.ctx); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		page := rec.pages[len(rec.pages)-1]
		if page.PageSize > pageCap {
			t.Fatalf("tick %d examined %d rows, want <= %d", tick, page.PageSize, pageCap)
		}
		// Tick 1 starts at the cursor with >= pageCap rows ahead, almost all healthy: the LIMIT
		// applies before the account predicate, so the page is exactly full. With the predicate
		// inside the page it would hold only the gated rows ahead (2 of ours plus any leftovers).
		if tick == 1 && page.PageSize != pageCap {
			t.Fatalf("tick 1 examined %d rows from the cursor with %d ahead, want exactly %d (LIMIT before the account predicate)",
				page.PageSize, ahead, pageCap)
		}
		if mustRun(t, env, ids[10]).Status == "recovery_wait" && !wrapped {
			t.Fatalf("tick %d parked the run behind the cursor before the cursor wrapped", tick)
		}
		if page.PageSize < pageCap {
			wrapped = true
		}
		done := true
		for _, id := range gatedIDs {
			done = done && mustRun(t, env, id).Status == "recovery_wait"
		}
		if done {
			for _, id := range okIDs {
				if s := mustRun(t, env, id).Status; s != "queued" {
					t.Fatalf("healthy run %s status %s, want queued", id, s)
				}
			}
			t.Logf("all %d gated runs parked in %d ticks (N=%d, cap=%d, bound=%d)", len(gatedIDs), tick, n, pageCap, bound)
			return
		}
	}
	t.Fatalf("gated runs not all parked within %d ticks (N=%d, cap=%d)", bound, n, pageCap)
}

// TestParkCodexAccountRevokesCapabilityLiveDB: the sweeper park revokes a capability the queued
// row still carries. The run is seeded with a non-empty codex_cap_hash and a known
// codex_claim_epoch by direct SQL (the shared fixture's rows have no hash, so a park that left it
// in place would pass assertCodexAccountParked); Sweep must NULL the hash and bump the epoch by
// exactly one.
func TestParkCodexAccountRevokesCapabilityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	const epoch = 7
	env.exec(`UPDATE runs SET codex_cap_hash = $2, codex_claim_epoch = $3 WHERE id = $1`,
		fx.runID, bytes.Repeat([]byte{0xab}, 32), epoch)
	fx.setAccount(t, "coord_state = 'quarantined'")
	before := mustRun(t, env, fx.runID)
	if before.Status != "queued" || len(before.CodexCapHash) == 0 || before.CodexClaimEpoch != epoch {
		t.Fatalf("seed: status=%s hash=%x epoch=%d, want queued with a hash at epoch %d",
			before.Status, before.CodexCapHash, before.CodexClaimEpoch, epoch)
	}
	svc := gateSweepService(env, fx.svc)
	if _, err := svc.Sweep(env.ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	r := mustRun(t, env, fx.runID)
	if r.CodexCapHash != nil || r.CodexClaimEpoch != epoch+1 {
		t.Fatalf("hash=%x epoch=%d, want NULL hash and epoch %d", r.CodexCapHash, r.CodexClaimEpoch, epoch+1)
	}
	assertCodexAccountParked(t, env, fx.runID, before)
}

// TestParkCodexAccountSkipsLockedCandidateLiveDB: a gated run another connection holds FOR UPDATE
// for a full tick is examined but skipped (SKIP LOCKED, no wait), then parked on a later tick.
func TestParkCodexAccountSkipsLockedCandidateLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	fx.setAccount(t, "coord_state = 'quarantined'")
	before := mustRun(t, env, fx.runID)
	svc := gateSweepService(env, fx.svc)

	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(env.ctx, `SELECT id FROM runs WHERE id = $1 FOR UPDATE`, fx.runID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(env.ctx, 10*time.Second)
	defer cancel()
	if _, err := svc.parkCodexAccountUnavailable(ctx); err != nil {
		t.Fatalf("locked tick: %v (SKIP LOCKED must not wait or fail)", err)
	}
	if s := mustRun(t, env, fx.runID).Status; s != "queued" {
		t.Fatalf("locked candidate status %s, want queued (skipped)", s)
	}
	if err := tx.Rollback(env.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.parkCodexAccountUnavailable(env.ctx); err != nil {
		t.Fatalf("unlocked tick: %v", err)
	}
	assertCodexAccountParked(t, env, fx.runID, before)
}

// TestQueuedReasonCodexAccountGatedLiveDB: before the park tick, a gated queued run's health row
// carries codex_account_gated and the queued arm names the account hold, not a worker reason.
func TestQueuedReasonCodexAccountGatedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	if healthRowForRun(t, env, fx.runID).CodexAccountGated {
		t.Fatal("a healthy account's run must not be gated")
	}
	fx.setAccount(t, "coord_state = 'quarantined'")
	row := healthRowForRun(t, env, fx.runID)
	if !row.CodexAccountGated {
		t.Fatal("a quarantined account's queued run must be gated")
	}
	if got := fx.svc.queuedReason(env.ctx, time.Now(), row); got != reasonCodexAccountUnavailable {
		t.Fatalf("queuedReason = %q, want %q", got, reasonCodexAccountUnavailable)
	}
}
