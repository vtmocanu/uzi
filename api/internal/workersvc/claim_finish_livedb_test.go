package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claim_finish_livedb_test.go pins PRD #1590 M1 end to end on the REAL claim path:
// Service.Claim -> ClaimRun -> assembleClaim -> finishRunClaim, against a real Postgres. The
// quarantine or authority change the observed incident saw lands AFTER ClaimRun's pre-claim gate
// (which would otherwise refuse the claim), through the Service.claimHooks seams. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh. Every name
// ends LiveDB so the store-IT sweep selects it.

// codexClaimFix is the observed-case prelude: a subscription Codex run first claimed by worker A
// at generation 1, then requeued by the sweeper with worker_id still A, started three hours ago
// with banked pause, plus a Codex-capable claimant B. The owner ALSO holds a default Anthropic
// token, so any Claude fallback would be visible.
type codexClaimFix struct {
	env                codexTestEnv
	userID, repoID     uuid.UUID
	aliasID, accountID uuid.UUID
	workerA, workerB   uuid.UUID
	runID              uuid.UUID
	holdA              uuid.UUID // the gen-1 hold on A; uuid.Nil when not seeded
	access             string    // the linked login's access token at seed time
	svc                *Service
}

func newCodexClaimFix(t *testing.T, env codexTestEnv, olderHold bool) *codexClaimFix {
	t.Helper()
	f := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, f.userID)
	seedDefaultAnthropicToken(t, env, f.userID)
	fx := &codexClaimFix{env: env, userID: f.userID, aliasID: f.aliasID, workerA: f.workerID, runID: f.runID, access: f.accessToken}
	fx.repoID = uuid.UUID(mustRun(t, env, f.runID).RepoID.Bytes)
	if err := env.pool.QueryRow(env.ctx, `SELECT provider_account_id FROM codex_credential_state
		WHERE user_secret_id = $1`, f.aliasID).Scan(&fx.accountID); err != nil {
		t.Fatalf("read linked account: %v", err)
	}
	env.exec(`UPDATE runs SET claim_generation = 1, status = 'claimed', codex_cap_hash = NULL WHERE id = $1`, f.runID)
	if olderHold {
		fx.holdA = claimRecoveryHold(t, env, f.runID, fx.workerA, 1)
	}
	// The sweeper's requeue: worker_id kept for affinity, capability revoked. A stale
	// recovery_retry_not_before is left in place as history (as PromoteRecoveryWaitRuns leaves
	// it), so a codex_account_unavailable park must clear it.
	env.exec(`UPDATE runs SET status = 'queued', status_since = now(), worker_id = $2,
		recovery_retry_not_before = now() - interval '1 hour',
		started_at = now() - interval '3 hours', budget_paused_seconds = 120,
		updated_at = now() - interval '3 hours' WHERE id = $1`, f.runID, fx.workerA)
	fx.workerB = uuid.New()
	env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities,
		last_heartbeat_at, snapshot_register_nonce, snapshot_epoch)
		VALUES ($1, $2, $3, $4, 'online', $5, now(), 'nonce-B', 0)`,
		fx.workerB, f.userID, "wb-"+fx.workerB.String(), fx.workerB[:],
		[]string{capability.CodexHarnessV1, capability.RecoveryArchiveV1})
	fx.svc = New(env.q, env.box, testParams())
	fx.svc.SetTxBeginner(env.pool)
	return fx
}

// claimant returns worker B as Claim sees it; capable controls recovery_archive_v1.
func (fx *codexClaimFix) claimant(t *testing.T, capable bool) store.Worker {
	t.Helper()
	w := wkrRow(t, fx.env, fx.workerB)
	w.ProtocolCapabilities = []string{capability.CodexHarnessV1}
	if capable {
		w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.RecoveryArchiveV1)
	}
	return w
}

func (fx *codexClaimFix) setAccount(t *testing.T, set string) {
	t.Helper()
	fx.env.exec(`UPDATE codex_provider_account SET `+set+` WHERE id = $1`, fx.accountID)
}

// holdB returns worker B's hold at generation 2 (the one this claim opened), or uuid.Nil.
func (fx *codexClaimFix) holdB(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	// original_worker_id is immutable provenance; a release nulls only the live FKs.
	err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT id FROM recovery_custody_holds
		WHERE run_id = $1 AND generation = 2 AND original_worker_id = $2`, fx.runID, fx.workerB).Scan(&id)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func (fx *codexClaimFix) countHolds(t *testing.T, generation int64) int {
	t.Helper()
	var n int
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM recovery_custody_holds
		WHERE run_id = $1 AND generation = $2`, fx.runID, generation).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertNoClaudeFallback: a Codex run never records an Anthropic epoch (no Claude credential
// was opened for it), and the claim that parked or failed it delivered nothing.
func (fx *codexClaimFix) assertNoClaudeFallback(t *testing.T, payload *ClaimPayload) {
	t.Helper()
	if payload != nil {
		t.Fatalf("a parked/failed Codex claim delivered a payload (carries an anthropic token: %v)", payload.Secrets.AnthropicOAuthToken != "")
	}
	if n := fx.env.countRunCredentialEpochs(t, fx.runID); n != 0 {
		t.Fatalf("run_credential_epochs rows = %d, want 0 (no Claude credential for a Codex run)", n)
	}
}

// assertParked checks the full D2 park field set on the run.
func (fx *codexClaimFix) assertParked(t *testing.T, before store.Run, wantWorker uuid.UUID) store.Run {
	t.Helper()
	r := mustRun(t, fx.env, fx.runID)
	if r.Status != "recovery_wait" || r.RecoveryWaitCause.String != recoveryCauseCodexAccountUnavailable {
		t.Fatalf("status=%s cause=%v, want recovery_wait/codex_account_unavailable", r.Status, r.RecoveryWaitCause)
	}
	if r.FailOrigin.Valid || r.FailureReason.Valid {
		t.Fatalf("a park carries fail_origin=%v failure_reason=%v, want both NULL", r.FailOrigin, r.FailureReason)
	}
	if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch <= before.CodexClaimEpoch {
		t.Fatalf("capability not revoked: hash=%x epoch=%d (before %d)", r.CodexCapHash, r.CodexClaimEpoch, before.CodexClaimEpoch)
	}
	if r.StartedAt.Valid || r.BudgetPausedSeconds != 0 || r.Health != "ok" {
		t.Fatalf("wall not reset: started_at=%v paused=%d health=%s", r.StartedAt, r.BudgetPausedSeconds, r.Health)
	}
	if r.RecoveryRetryNotBefore.Valid {
		t.Fatalf("recovery_retry_not_before = %v, want NULL (the account, not the timer, resumes this cause)", r.RecoveryRetryNotBefore)
	}
	if r.WorkerID != pgconv.UUID(wantWorker) {
		t.Fatalf("worker_id = %v, want %s", r.WorkerID, wantWorker)
	}
	return r
}

// TestClaimObservedCaseReplayParksLiveDB is the M1 regression: the #1582 replay. Gen-1 hold on
// worker A, run requeued with worker_id A, then a cold claim by capable worker B at generation 2
// whose account is quarantined after ClaimRun's gate (the race the gate cannot see). The claim
// must park the run on codex_account_unavailable, release ONLY B's gen-2 hold, keep A's gen-1
// hold, prefer A, and deliver nothing. On the unfixed code the run is failed
// credential_unavailable and both holds stay open.
func TestClaimObservedCaseReplayParksLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	before := mustRun(t, env, fx.runID)
	fx.svc.claimHooks = &claimTestHooks{beforeAssembly: func(context.Context, store.Run) {
		fx.setAccount(t, "coord_state = 'quarantined'")
	}}

	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	r := fx.assertParked(t, before, fx.workerA)
	if r.ClaimGeneration != 2 {
		t.Fatalf("claim_generation = %d, want 2 (the cold claim)", r.ClaimGeneration)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
	gen2 := fx.holdB(t)
	if gen2 == uuid.Nil {
		t.Fatal("the gen-2 hold B's claim opened is missing")
	}
	assertClaimRecoveryHold(t, env, gen2, "released", true)
}

// TestClaimPreMintQuarantineUsesSnapshotIdentityLiveDB (e): a quarantine that lands before the
// mint means no capability was minted, so the exact-claim check compares the ClaimRun snapshot's
// epoch/hash, which still match, and parks. A warm claim (no older hold) keeps worker_id.
func TestClaimPreMintQuarantineUsesSnapshotIdentityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	before := mustRun(t, env, fx.runID)
	var claimed store.Run
	fx.svc.claimHooks = &claimTestHooks{beforeAssembly: func(_ context.Context, run store.Run) {
		claimed = run
		fx.setAccount(t, "coord_state = 'quarantined'")
	}}
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	r := fx.assertParked(t, before, fx.workerB)
	if r.CodexClaimEpoch != claimed.CodexClaimEpoch+1 {
		t.Fatalf("epoch = %d, want the snapshot epoch %d + 1 (no mint happened)", r.CodexClaimEpoch, claimed.CodexClaimEpoch)
	}
	assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
}

// TestClaimQuarantineAfterSuccessfulAssemblyParksLiveDB (b): a quarantine landing after a
// SUCCESSFUL assembly (the token was decrypted and would have shipped) parks the run and discards
// the payload, on BOTH Claim return paths: the no-snapshot path and the snapshot + tx path.
func TestClaimQuarantineAfterSuccessfulAssemblyParksLiveDB(t *testing.T) {
	for _, withSnapshot := range []bool{false, true} {
		name := "no snapshot"
		if withSnapshot {
			name = "snapshot tx"
		}
		t.Run(name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			before := mustRun(t, env, fx.runID)
			var assembled *ClaimPayload
			fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
				if err != nil || p == nil || p.Secrets.Codex == nil {
					t.Fatalf("assembly did not succeed: payload=%v err=%v", p != nil, err)
				}
				assembled = p
				fx.setAccount(t, "coord_state = 'quarantined'")
			}}
			var snap *ActiveSnapshot
			if withSnapshot {
				snap = &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-B", Active: []ActiveRunEntry{}}
			}
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), snap)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if assembled == nil {
				t.Fatal("assembly hook never ran")
			}
			fx.assertNoClaudeFallback(t, payload)
			fx.assertParked(t, before, fx.workerA)
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
			assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
		})
	}
}

// TestClaimRevocationAfterAssemblyFailsLiveDB (c, N6): a bumped account credential_revision is a
// revocation, terminal whatever assembly saw. After a successful assembly the run fails
// credential_unavailable with the revision error as its reason; when assembly had already seen a
// quarantine (a hold-class error), the lock-time revocation decides, and the recorded reason is the
// revocation, never the assembly's quarantine text. Only the current hold is released; the
// run is card-backed (issue_iid set), so move_pending_since is stamped.
func TestClaimRevocationAfterAssemblyFailsLiveDB(t *testing.T) {
	for _, quarantinedFirst := range []bool{false, true} {
		name := "after success"
		if quarantinedFirst {
			name = "after assembly quarantine"
		}
		t.Run(name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			fx.svc.claimHooks = &claimTestHooks{
				beforeAssembly: func(context.Context, store.Run) {
					if quarantinedFirst {
						fx.setAccount(t, "coord_state = 'quarantined'")
					}
				},
				afterAssembly: func(_ context.Context, _ store.Run, _ *ClaimPayload, err error) {
					if quarantinedFirst != (err != nil) {
						t.Fatalf("assembly err = %v, want an error iff quarantined first", err)
					}
					fx.setAccount(t, "credential_revision = credential_revision + 1")
				},
			}
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			fx.assertNoClaudeFallback(t, payload)
			r := mustRun(t, env, fx.runID)
			if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" {
				t.Fatalf("status=%s origin=%v, want failed/credential_unavailable", r.Status, r.FailOrigin)
			}
			if !strings.Contains(r.FailureReason.String, ErrCodexAccountRevisionStale.Error()) ||
				strings.Contains(r.FailureReason.String, ErrCodexAccountQuarantined.Error()) {
				t.Fatalf("failure_reason = %q, want the revocation error that decided it", r.FailureReason.String)
			}
			if len(r.CodexCapHash) != 0 || !r.MovePendingSince.Valid || r.RecoveryWaitCause.Valid {
				t.Fatalf("terminal row: hash=%x move_pending=%v cause=%v", r.CodexCapHash, r.MovePendingSince, r.RecoveryWaitCause)
			}
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
			assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
		})
	}
}

// TestClaimSameWorkerRemintIsIdleLiveDB (d): a same-worker re-mint (a newer capability on the
// SAME claim generation) plus a quarantine between this attempt's mint and its finish makes this
// attempt superseded: idle, with the newer capability and the open hold untouched. It holds for
// an attempt whose assembly SUCCEEDED and for one that failed AFTER minting (the mint identity
// rides codexMintedClaimError). The control proves the minted identity is honoured: the same
// post-mint failure without the re-mint parks.
func TestClaimSameWorkerRemintIsIdleLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remint bool
		after  bool // re-mint after assembly (success) vs after the mint (assembly then fails)
	}{
		{"remint after successful assembly", true, true},
		{"remint after mint, assembly fails", true, false},
		{"control: post-mint failure without remint parks", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, false)
			before := mustRun(t, env, fx.runID)
			var wire string
			mutate := func() {
				if tc.remint {
					wire = env.mintCap(t, fx.runID, fx.workerB)
				}
				fx.setAccount(t, "coord_state = 'quarantined'")
			}
			hooks := &claimTestHooks{}
			if tc.after {
				hooks.afterAssembly = func(context.Context, store.Run, *ClaimPayload, error) { mutate() }
			} else {
				hooks.afterMint = func(context.Context, store.Run) { mutate() }
			}
			fx.svc.claimHooks = hooks
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want idle", payload != nil, err)
			}
			if !tc.remint {
				fx.assertParked(t, before, fx.workerB)
				assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
				return
			}
			epoch, secret, ok := parseCodexCapability(wire)
			if !ok {
				t.Fatal("re-minted capability did not parse")
			}
			r := mustRun(t, env, fx.runID)
			if r.Status != "claimed" || r.CodexClaimEpoch != epoch ||
				string(r.CodexCapHash) != string(hashCodexCapability(secret)) ||
				r.RecoveryWaitCause.Valid || r.FailOrigin.Valid {
				t.Fatalf("newer claim mutated: status=%s epoch=%d (want %d) cause=%v origin=%v",
					r.Status, r.CodexClaimEpoch, epoch, r.RecoveryWaitCause, r.FailOrigin)
			}
			assertClaimRecoveryHold(t, env, fx.holdB(t), "open", false)
		})
	}
}

// TestClaimStaleGenerationMintIsFencedLiveDB (PRD #1590 review): the capability mint is fenced on
// claim_generation. Worker B claims the run at generation G, the claim is requeued (the real
// RequeueClaimedRunToQueued, which keeps worker_id = B), and B reclaims it through Service.Claim
// as G+1. Right after G+1 minted, the stale generation-G attempt resumes through the REAL
// codexClaimSecrets: it must refuse with errRunVanished and leave G+1's capability (hash and
// epoch) untouched, so G+1's release barrier still sees its own mint and delivers the payload.
// Without the fence the G attempt matches on worker_id + status, overwrites G+1's capability and
// bumps the epoch, and G+1 drops its payload as superseded.
func TestClaimStaleGenerationMintIsFencedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false)
	wkr := fx.claimant(t, false)

	// Generation G: B's first claim, paused before its mint (nothing assembled yet).
	runAtG, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(wkr))
	if err != nil {
		t.Fatalf("ClaimRun (generation G): %v", err)
	}
	if runAtG.ID != fx.runID || uuid.UUID(runAtG.WorkerID.Bytes) != fx.workerB {
		t.Fatalf("ClaimRun claimed run %s for worker %v, want %s for B", runAtG.ID, runAtG.WorkerID, fx.runID)
	}
	// The claim is requeued the way the sweeper does it: status back to queued, worker_id kept.
	if n, err := env.q.RequeueClaimedRunToQueued(env.ctx, fx.runID); err != nil || n != 1 {
		t.Fatalf("RequeueClaimedRunToQueued = (%d, %v), want (1, nil)", n, err)
	}

	var (
		staleErr             error
		staleSecrets         *ClaimCodexSecrets
		liveHash             []byte
		liveEpoch            int64
		liveGen              int64
		afterReplay          store.Run
		hookRan, replayFired bool
	)
	hooks := &claimTestHooks{}
	hooks.afterMint = func(ctx context.Context, live store.Run) {
		hookRan = true
		liveGen = live.ClaimGeneration
		mintedLive := mustRun(t, env, fx.runID)
		liveHash = append([]byte(nil), mintedLive.CodexCapHash...)
		liveEpoch = mintedLive.CodexClaimEpoch
		// Replay the stale generation-G attempt through the real claim-secrets path, with the
		// race hooks cleared so the nested call does not re-enter this hook.
		fx.svc.claimHooks = nil
		staleSecrets, staleErr = fx.svc.codexClaimSecrets(ctx, wkr, runAtG)
		fx.svc.claimHooks = hooks
		replayFired = true
		afterReplay = mustRun(t, env, fx.runID)
	}
	fx.svc.claimHooks = hooks

	// Generation G+1: B reclaims the requeued run through the real claim path.
	payload, err := fx.svc.Claim(env.ctx, wkr, nil)
	fx.svc.claimHooks = nil
	if !hookRan || !replayFired {
		t.Fatalf("afterMint hook ran=%v replay=%v, want both (the reclaim never minted)", hookRan, replayFired)
	}
	if liveGen != runAtG.ClaimGeneration+1 {
		t.Fatalf("reclaim generation = %d, want G+1 = %d", liveGen, runAtG.ClaimGeneration+1)
	}
	if !errors.Is(staleErr, errRunVanished) || staleSecrets != nil {
		t.Fatalf("stale generation-G codexClaimSecrets = (secrets=%v, %v), want (nil, errRunVanished)", staleSecrets != nil, staleErr)
	}
	if len(liveHash) == 0 || string(afterReplay.CodexCapHash) != string(liveHash) || afterReplay.CodexClaimEpoch != liveEpoch {
		t.Fatalf("stale replay overwrote the live capability: epoch=%d (want %d) hash_matches=%v",
			afterReplay.CodexClaimEpoch, liveEpoch, string(afterReplay.CodexCapHash) == string(liveHash))
	}
	if err != nil || payload == nil || payload.Secrets.Codex == nil || payload.Secrets.Codex.AccessToken == "" {
		t.Fatalf("G+1 Claim = (payload=%v, codex=%v, %v), want a payload carrying Codex secrets",
			payload != nil, payload != nil && payload.Secrets.Codex != nil, err)
	}
	epoch, secret, ok := parseCodexCapability(payload.Secrets.Codex.Capability)
	if !ok || epoch != liveEpoch || string(hashCodexCapability(secret)) != string(liveHash) {
		t.Fatalf("G+1 payload capability is not G+1's mint: epoch=%d (want %d) parsed=%v", epoch, liveEpoch, ok)
	}
	final := mustRun(t, env, fx.runID)
	if final.Status != "claimed" || final.ClaimGeneration != liveGen ||
		final.CodexClaimEpoch != liveEpoch || string(final.CodexCapHash) != string(liveHash) {
		t.Fatalf("final run: status=%s gen=%d epoch=%d (want claimed, %d, %d) hash_matches=%v",
			final.Status, final.ClaimGeneration, final.CodexClaimEpoch, liveGen, liveEpoch,
			string(final.CodexCapHash) == string(liveHash))
	}

	// The release barriers carry their own generation check (defence in depth behind the SQL
	// fence, which stops the stale mint before a barrier runs, so the scenario above cannot
	// reach it). Pin it directly against the live row: with G+1's own mint identity, the
	// barriers accept generation G+1 and refuse generation G, in both barrier modes.
	q, ok := fx.svc.codexStore()
	if !ok {
		t.Fatal("codex store unavailable")
	}
	for _, checkCap := range []bool{false, true} {
		if err := fx.svc.reauthorizeCodexRelease(env.ctx, q, fx.runID, wkr, liveGen, liveEpoch, liveHash, checkCap); err != nil {
			t.Fatalf("reauthorize at live generation (checkCapability=%v) = %v, want nil", checkCap, err)
		}
		if err := fx.svc.reauthorizeCodexRelease(env.ctx, q, fx.runID, wkr, runAtG.ClaimGeneration, liveEpoch, liveHash, checkCap); !errors.Is(err, errRunVanished) {
			t.Fatalf("reauthorize at stale generation G (checkCapability=%v) = %v, want errRunVanished", checkCap, err)
		}
	}
}

// TestClaimAmbiguousMintNoMutationLiveDB (f): an ambiguous capability mint (the write may have
// landed) returns the error with no payload and changes nothing, on the real ClaimRun row.
func TestClaimAmbiguousMintNoMutationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	run, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(fx.claimant(t, true)))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	fx.setAccount(t, "coord_state = 'quarantined'")
	before := mustRun(t, env, fx.runID)
	for _, cause := range []error{
		errCodexMintAmbiguous,
		// The judge wrap keeps the marker (wrapJudgeCodexClaimError).
		wrapJudgeCodexClaimError(errCodexMintAmbiguous),
	} {
		payload, err := fx.svc.finishRunClaim(env.ctx, run, nil, cause,
			claimRecoveryIdentity{workerID: fx.workerB, recoveryCapable: true})
		if payload != nil || !errors.Is(err, errCodexMintAmbiguous) {
			t.Fatalf("finishRunClaim = (%v, %v), want (nil, errCodexMintAmbiguous)", payload != nil, err)
		}
	}
	after := mustRun(t, env, fx.runID)
	if after.Status != "claimed" || after.CodexClaimEpoch != before.CodexClaimEpoch ||
		!after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) || after.RecoveryWaitCause.Valid || after.FailOrigin.Valid {
		t.Fatalf("ambiguous mint mutated the run: status=%s epoch=%d", after.Status, after.CodexClaimEpoch)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
	assertClaimRecoveryHold(t, env, fx.holdB(t), "open", false)
}

// claimRunParamsFor mirrors the params Service.Claim builds for w (claimRunParams plus the
// worker's protocol capabilities, recovery capability and the curated Codex vocabulary).
func claimRunParamsFor(w store.Worker) store.ClaimRunParams {
	p := claimRunParams(w)
	p.WorkerProtocolCaps = w.ProtocolCapabilities
	p.RecoveryCapable = false
	for _, c := range w.ProtocolCapabilities {
		if c == capability.RecoveryArchiveV1 {
			p.RecoveryCapable = true
		}
	}
	p.WorkerIdentity = workerIdentity(w)
	p.CodexCuratedModels = codexCuratedModelsSlice()
	return p
}

// TestClaimVerifiedReloginAfterAssemblyParksLiveDB (g, A1): a verified same-identity re-login
// that lands after assembly (the alias material advanced and is linked back to the SAME account,
// identity and credential revision) parks the run and discards the old token, as does a re-login
// still staging on the same alias.
func TestClaimVerifiedReloginAfterAssemblyParksLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, set string }{
		{"linked same identity", "material_revision = material_revision + 1"},
		{"staging", "material_revision = material_revision + 1, status = 'staging', provider_account_id = NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			before := mustRun(t, env, fx.runID)
			fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
				if err != nil || p == nil {
					t.Fatalf("assembly did not succeed: %v", err)
				}
				env.exec(`UPDATE codex_credential_state SET `+tc.set+` WHERE user_secret_id = $1`, fx.aliasID)
			}}
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			fx.assertNoClaudeFallback(t, payload)
			fx.assertParked(t, before, fx.workerA)
			assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
		})
	}
}

// TestClaimCodexJudgeQuarantineStaysTerminalLiveDB (h, D7): a Codex JUDGE whose account is
// quarantined after assembly fails credential_unavailable. It never parks, releases no hold (a
// judge opens none) and, being card-less (issue_iid NULL), keeps move_pending_since NULL.
func TestClaimCodexJudgeQuarantineStaysTerminalLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	// Retire the issue run so the judge is the only claimable run.
	env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, fx.runID)
	judgeID := env.seedCodexJudgeRun(t, fx.userID, fx.workerA, fx.runID)
	if err := fx.svc.FreezeCodexBinding(env.ctx, fx.userID, judgeID, fx.aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding on the judge: %v", err)
	}
	env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL WHERE id = $1`, judgeID)
	fx.svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, p *ClaimPayload, err error) {
		if run.ID != judgeID || err != nil || p == nil {
			t.Fatalf("judge assembly: run=%s payload=%v err=%v", run.ID, p != nil, err)
		}
		fx.setAccount(t, "coord_state = 'quarantined'")
	}}
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want idle", payload != nil, err)
	}
	r := mustRun(t, env, judgeID)
	if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" || r.RecoveryWaitCause.Valid {
		t.Fatalf("judge status=%s origin=%v cause=%v, want failed/credential_unavailable", r.Status, r.FailOrigin, r.RecoveryWaitCause)
	}
	if !strings.Contains(r.FailureReason.String, ErrCodexAccountQuarantined.Error()) {
		t.Fatalf("judge failure_reason = %q, want the quarantine", r.FailureReason.String)
	}
	if r.IssueIid.Valid || r.MovePendingSince.Valid {
		t.Fatalf("card-less judge: issue_iid=%v move_pending_since=%v, want both NULL", r.IssueIid, r.MovePendingSince)
	}
	var judgeHolds int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, judgeID).Scan(&judgeHolds); err != nil {
		t.Fatal(err)
	}
	if judgeHolds != 0 {
		t.Fatalf("judge holds = %d, want 0", judgeHolds)
	}
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
}

// TestClaimHoldClassRecoveredBeforeFinishParksLiveDB (i): assembly saw the quarantine and minted
// nothing; the account recovered before the exact-claim check. The discarded attempt still parks
// (the account-driven promoter resumes it) rather than failing or delivering.
func TestClaimHoldClassRecoveredBeforeFinishParksLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	before := mustRun(t, env, fx.runID)
	fx.svc.claimHooks = &claimTestHooks{
		beforeAssembly: func(context.Context, store.Run) { fx.setAccount(t, "coord_state = 'quarantined'") },
		afterAssembly: func(_ context.Context, _ store.Run, _ *ClaimPayload, err error) {
			if !errors.Is(err, ErrCodexAccountQuarantined) {
				t.Fatalf("assembly err = %v, want the quarantine", err)
			}
			fx.setAccount(t, "coord_state = 'idle'")
		},
	}
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	fx.assertNoClaudeFallback(t, payload)
	fx.assertParked(t, before, fx.workerA)
	assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
}

// TestClaimLockNotAvailableNoMutationLiveDB (j, N4): a concurrent writer holding the account row
// for longer than the bounded retry makes the classifier's NOWAIT lock fail every attempt. The
// claim returns the 55P03 with no payload and no mutation: the claim, its capability and its hold
// are exactly as assembly left them.
func TestClaimLockNotAvailableNoMutationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, true)
	blocker, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(env.ctx) }()
	var afterAssembly store.Run
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil {
			t.Fatalf("assembly did not succeed: %v", err)
		}
		afterAssembly = mustRun(t, env, fx.runID)
		if _, err := blocker.Exec(env.ctx, `SELECT id FROM codex_provider_account WHERE id = $1 FOR UPDATE`, fx.accountID); err != nil {
			t.Fatalf("hold account row: %v", err)
		}
	}}
	start := time.Now()
	payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
	if payload != nil || !isLockNotAvailable(err) {
		t.Fatalf("Claim = (%v, %v), want (nil, 55P03)", payload != nil, err)
	}
	if elapsed := time.Since(start); elapsed < time.Duration(finishRunClaimAttempts-1)*finishRunClaimRetryDelay {
		t.Fatalf("returned after %s: the %d-attempt retry did not run", elapsed, finishRunClaimAttempts)
	}
	r := mustRun(t, env, fx.runID)
	if r.Status != "claimed" || r.CodexClaimEpoch != afterAssembly.CodexClaimEpoch ||
		string(r.CodexCapHash) != string(afterAssembly.CodexCapHash) || !r.UpdatedAt.Time.Equal(afterAssembly.UpdatedAt.Time) {
		t.Fatalf("55P03 mutated the claim: status=%s epoch=%d (want %d)", r.Status, r.CodexClaimEpoch, afterAssembly.CodexClaimEpoch)
	}
	assertClaimRecoveryHold(t, env, fx.holdB(t), "open", false)
	assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
}

// TestClaimTerminalAssemblyFencedLiveDB (k): provisioning and guardrail refusals at claim go
// through the same exact-claim transaction: failed with their own origin, releasing ONLY the
// current generation's hold (worker A's older hold is preserved), with no payload.
func TestClaimTerminalAssemblyFencedLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, origin string }{
		{"provisioning", "provisioning_failed"},
		{"guardrail", "guardrail_blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			if tc.origin == "provisioning_failed" {
				env.exec(`INSERT INTO repo_tool_profiles (user_id, repo_id, packages) VALUES ($1, $2, $3)`,
					fx.userID, fx.repoID, []byte(`["definitely-not-allowlisted-`+uuid.NewString()[:8]+`"]`))
			} else {
				fx.svc.SetRepoGuard(&fakeGuard{res: privcheck.GuardResult{Blocked: true, Findings: []privcheck.Finding{
					{Code: privcheck.CodeWriteRoleCanPush, Severity: privcheck.SeverityBlock, Message: "the bot can push to the default branch"},
				}}})
			}
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			fx.assertNoClaudeFallback(t, payload)
			r := mustRun(t, env, fx.runID)
			if r.Status != "failed" || r.FailOrigin.String != tc.origin || !r.MovePendingSince.Valid || len(r.CodexCapHash) != 0 {
				t.Fatalf("status=%s origin=%v move_pending=%v, want failed/%s", r.Status, r.FailOrigin, r.MovePendingSince, tc.origin)
			}
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
			assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
		})
	}
}

// TestClaimCapableAndIncapableClaimantsParkLiveDB (l): a capable claimant's claim opened a gen-2
// hold that the park releases; an incapable claimant opened none, so the park releases nothing.
// Either way worker A's older hold stays open and the park prefers A.
func TestClaimCapableAndIncapableClaimantsParkLiveDB(t *testing.T) {
	for _, capable := range []bool{true, false} {
		name := "incapable"
		if capable {
			name = "capable"
		}
		t.Run(name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			before := mustRun(t, env, fx.runID)
			fx.svc.claimHooks = &claimTestHooks{beforeAssembly: func(context.Context, store.Run) {
				fx.setAccount(t, "coord_state = 'quarantined'")
			}}
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, capable), nil)
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			fx.assertNoClaudeFallback(t, payload)
			fx.assertParked(t, before, fx.workerA)
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
			if capable {
				assertClaimRecoveryHold(t, env, fx.holdB(t), "released", true)
			} else if n := fx.countHolds(t, 2); n != 0 {
				t.Fatalf("an incapable claim has %d gen-2 holds, want 0", n)
			}
		})
	}
}

// TestFinishRunClaimHoldCountMatrixLiveDB (m): the claim-time expectation (from the claimant's
// recovery capability) against the observed open holds for (run, claimant, generation). 0/0 parks
// with no release; 1/0, 0/1 and two holds roll back with errClaimRecoveryCustody, no payload and
// the row unchanged.
func TestFinishRunClaimHoldCountMatrixLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		capable bool
		extra   int // holds seeded at the claim generation beyond what ClaimRun opened
		park    bool
	}{
		{"expected 0 actual 0", false, 0, true},
		{"expected 1 actual 0", true, -1, false},
		{"expected 0 actual 1", false, 1, false},
		{"expected 1 actual 2", true, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			w := fx.claimant(t, tc.capable)
			run, err := env.q.ClaimRun(env.ctx, claimRunParamsFor(w))
			if err != nil {
				t.Fatalf("ClaimRun: %v", err)
			}
			switch {
			case tc.extra < 0:
				env.exec(`DELETE FROM recovery_custody_holds WHERE run_id = $1 AND generation = $2`, fx.runID, run.ClaimGeneration)
			case tc.extra > 0:
				claimRecoveryHold(t, env, fx.runID, fx.workerB, run.ClaimGeneration)
			}
			fx.setAccount(t, "coord_state = 'quarantined'")
			before := mustRun(t, env, fx.runID)
			payload, err := fx.svc.finishRunClaim(env.ctx, run, nil,
				fmt.Errorf("%w: %w", errCredentialUnavailable, ErrCodexAccountQuarantined),
				claimRecoveryIdentity{workerID: fx.workerB, recoveryCapable: tc.capable})
			if payload != nil {
				t.Fatal("finishRunClaim delivered a payload")
			}
			if tc.park {
				if err != nil {
					t.Fatalf("finishRunClaim: %v", err)
				}
				fx.assertParked(t, before, fx.workerA)
				return
			}
			if !errors.Is(err, errClaimRecoveryCustody) {
				t.Fatalf("err = %v, want errClaimRecoveryCustody", err)
			}
			after := mustRun(t, env, fx.runID)
			if after.Status != "claimed" || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) || after.CodexClaimEpoch != before.CodexClaimEpoch {
				t.Fatalf("custody mismatch mutated the run: status=%s", after.Status)
			}
			var released int
			if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM recovery_custody_holds
				WHERE run_id = $1 AND state <> 'open'`, fx.runID).Scan(&released); err != nil {
				t.Fatal(err)
			}
			if released != 0 {
				t.Fatalf("custody mismatch released %d holds", released)
			}
		})
	}
}

// TestClaimLostBetweenClaimAndFinishIsIdleLiveDB (n): a cancel, a reclaim by another worker, or a
// same-worker generation bump between the claim and its finish wins: the claim is idle, nothing is
// written (even though the account is quarantined), and B's hold is left for its owner.
func TestClaimLostBetweenClaimAndFinishIsIdleLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, set string }{
		{"cancel", "status = 'cancelled'"},
		{"reclaim by another worker", "worker_id = $2, claim_generation = claim_generation + 1"},
		{"same-worker generation mismatch", "claim_generation = claim_generation + 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, true)
			var won store.Run
			fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(context.Context, store.Run, *ClaimPayload, error) {
				fx.setAccount(t, "coord_state = 'quarantined'")
				if strings.Contains(tc.set, "$2") {
					env.exec(`UPDATE runs SET `+tc.set+` WHERE id = $1`, fx.runID, fx.workerA)
				} else {
					env.exec(`UPDATE runs SET `+tc.set+` WHERE id = $1`, fx.runID)
				}
				won = mustRun(t, env, fx.runID)
			}}
			payload, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want idle", payload != nil, err)
			}
			r := mustRun(t, env, fx.runID)
			if r.Status != won.Status || !r.UpdatedAt.Time.Equal(won.UpdatedAt.Time) || r.CodexClaimEpoch != won.CodexClaimEpoch ||
				r.ClaimGeneration != won.ClaimGeneration || r.RecoveryWaitCause.Valid || r.FailOrigin.Valid {
				t.Fatalf("the winning transition was overwritten: status=%s (want %s)", r.Status, won.Status)
			}
			assertClaimRecoveryHold(t, env, fx.holdB(t), "open", false)
			assertClaimRecoveryHold(t, env, fx.holdA, "open", false)
		})
	}
}

// TestParkedCodexRunRefusesCredentialOpsLiveDB (o, D3): a codex_account_unavailable park revokes
// the claim capability, so neither the capability the discarded payload carried nor any other
// capability authorizes a release or a start-refresh against the parked run.
func TestParkedCodexRunRefusesCredentialOpsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx := newCodexClaimFix(t, env, false) // warm: the park keeps worker_id = B, the presenter
	var wire string
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil || p.Secrets.Codex == nil {
			t.Fatalf("assembly did not succeed: %v", err)
		}
		wire = p.Secrets.Codex.Capability
		fx.setAccount(t, "coord_state = 'quarantined'")
	}}
	w := fx.claimant(t, true)
	if payload, err := fx.svc.Claim(env.ctx, w, nil); err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want idle (parked)", payload != nil, err)
	}
	r := mustRun(t, env, fx.runID)
	if r.Status != "recovery_wait" || r.WorkerID != pgconv.UUID(fx.workerB) {
		t.Fatalf("status=%s worker=%v, want recovery_wait on B", r.Status, r.WorkerID)
	}
	// Recover the account, so only the revoked capability can explain a refusal.
	fx.setAccount(t, "coord_state = 'idle'")
	current := formatCodexCapability(r.CodexClaimEpoch, "not-the-minted-secret")
	for _, scope := range []CodexOpScope{ScopeReleaseAccessToken, ScopeStartRefresh} {
		if _, err := fx.svc.AuthorizeCodexCredentialOp(env.ctx, w, fx.runID, wire, scope); !errors.Is(err, ErrCodexCapabilityEpoch) {
			t.Fatalf("scope %v with the discarded capability: err = %v, want ErrCodexCapabilityEpoch", scope, err)
		}
		if _, err := fx.svc.AuthorizeCodexCredentialOp(env.ctx, w, fx.runID, current, scope); !errors.Is(err, ErrCodexCapabilityMismatch) {
			t.Fatalf("scope %v at the current epoch: err = %v, want ErrCodexCapabilityMismatch (hash revoked)", scope, err)
		}
	}
	// The parked run is not claimable, so no later claim can carry a Claude token for it.
	if payload, err := fx.svc.Claim(env.ctx, w, nil); err != nil || payload != nil {
		t.Fatalf("a claim while parked = (%v, %v), want idle", payload != nil, err)
	}
	fx.assertNoClaudeFallback(t, nil)
}

// TestFailClaimAssemblyExactCardlessKeepsMovePendingNullLiveDB (B1, #1482): the fenced terminal
// writer stamps move_pending_since only for a card-backed run, exactly as MarkRunFailedByID. A
// card-less run (a prompt run, issue_iid NULL) failed through it keeps move_pending_since NULL; a
// card-backed run is stamped.
func TestFailClaimAssemblyExactCardlessKeepsMovePendingNullLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	for _, cardless := range []bool{true, false} {
		runID := uuid.New()
		iid := pgtype.Int8{Int64: nextOutageIID(), Valid: !cardless}
		kind := "issue"
		if cardless {
			kind = "prompt"
		}
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
			status, worker_id, claim_generation) VALUES ($1, $2, $3, $4, $5, 't', 'd', 'claimed', $6, 1)`,
			runID, userID, repoID, kind, iid, workerID)
		n, err := env.q.FailClaimAssemblyExact(env.ctx, store.FailClaimAssemblyExactParams{
			ID: runID, WorkerID: pgconv.UUID(workerID), ClaimGeneration: 1,
			FailureReason: pgconv.TextOrNull("x"), FailOrigin: pgconv.TextOrNull("credential_unavailable"),
		})
		if err != nil || n != 1 {
			t.Fatalf("FailClaimAssemblyExact = (%d, %v)", n, err)
		}
		r := mustRun(t, env, runID)
		if r.Status != "failed" || r.MovePendingSince.Valid == cardless {
			t.Fatalf("cardless=%v: status=%s move_pending_since=%v", cardless, r.Status, r.MovePendingSince)
		}
	}
}
