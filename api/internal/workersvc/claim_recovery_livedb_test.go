package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// claimRecoveryHold seeds a real custody row and returns its id so assertions
// distinguish this claim's hold from custody retained by an earlier claim.
func claimRecoveryHold(t *testing.T, env codexTestEnv, runID, workerID uuid.UUID, generation int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	env.exec(`INSERT INTO recovery_custody_holds
		(id, user_id, repo_id, run_id, generation, state, original_worker_id,
		 original_worker_identity, live_worker_id, live_run_id)
		SELECT $1, user_id, repo_id, id, $2, 'open', $3, $4, $3, id
		FROM runs WHERE id = $5`, id, generation, workerID, "worker-"+workerID.String(), runID)
	return id
}

func assertClaimRecoveryHold(t *testing.T, env codexTestEnv, id uuid.UUID, wantState string, wantEvidence bool) {
	t.Helper()
	var state string
	var evidence pgtype.Text
	if err := env.pool.QueryRow(env.ctx,
		"SELECT state, release_evidence FROM recovery_custody_holds WHERE id = $1", id).
		Scan(&state, &evidence); err != nil {
		t.Fatal(err)
	}
	if state != wantState || evidence.Valid != wantEvidence ||
		(wantEvidence && evidence.String != "no_adopted_source") {
		t.Fatalf("hold %s: state=%s evidence=%v, want %s/no_adopted_source=%v",
			id, state, evidence, wantState, wantEvidence)
	}
}

func TestCodexClaimRecoveryTerminalOriginsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, origin string
		cause        error
		capable      bool
	}{
		{"credential capable", "credential_unavailable", errCredentialUnavailable, true},
		{"provisioning capable", "provisioning_failed", errToolPackagesRejected, true},
		{"guardrail capable", "guardrail_blocked", errGuardrailBlockedClaim, true},
		{"credential incapable", "credential_unavailable", errCredentialUnavailable, false},
		{"provisioning incapable", "provisioning_failed", errToolPackagesRejected, false},
		{"guardrail incapable", "guardrail_blocked", errGuardrailBlockedClaim, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newSubscriptionFixture(t, env)
			env.exec("UPDATE runs SET claim_generation = 2, harness = 'codex' WHERE id = $1", f.runID)
			older := claimRecoveryHold(t, env, f.runID, f.workerID, 1)
			var current uuid.UUID
			if tc.capable {
				current = claimRecoveryHold(t, env, f.runID, f.workerID, 2)
			}
			run := mustRun(t, env, f.runID)
			f.svc.txBeginner = env.pool
			payload, err := f.svc.finishRunClaim(env.ctx, run, nil, tc.cause,
				claimRecoveryIdentity{workerID: f.workerID, recoveryCapable: tc.capable})
			if err != nil || payload != nil {
				t.Fatalf("finishRunClaim = (%v, %v), want idle", payload, err)
			}
			after := mustRun(t, env, f.runID)
			if after.Status != "failed" || !after.FailOrigin.Valid || after.FailOrigin.String != tc.origin ||
				!after.FailureReason.Valid || after.FailureReason.String != tc.cause.Error() ||
				after.CodexClaimEpoch != run.CodexClaimEpoch+1 || len(after.CodexCapHash) != 0 {
				t.Fatalf("terminal run: status=%s origin=%v reason=%v epoch=%d hash=%x",
					after.Status, after.FailOrigin, after.FailureReason, after.CodexClaimEpoch, after.CodexCapHash)
			}
			assertClaimRecoveryHold(t, env, older, "open", false)
			if tc.capable {
				assertClaimRecoveryHold(t, env, current, "released", true)
			}
		})
	}
}

func TestCodexClaimRecoveryStaleTransitionLiveDB(t *testing.T) {
	for _, status := range []string{"queued", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newSubscriptionFixture(t, env)
			env.exec("UPDATE runs SET claim_generation = 1, harness = 'codex' WHERE id = $1", f.runID)
			hold := claimRecoveryHold(t, env, f.runID, f.workerID, 1)
			stale := mustRun(t, env, f.runID)
			env.exec("UPDATE runs SET status = $1 WHERE id = $2", status, f.runID)
			f.svc.txBeginner = env.pool
			payload, err := f.svc.finishRunClaim(env.ctx, stale, nil, errCredentialUnavailable,
				claimRecoveryIdentity{workerID: f.workerID, recoveryCapable: true})
			if err != nil || payload != nil {
				t.Fatalf("stale finishRunClaim = (%v, %v), want idle", payload, err)
			}
			after := mustRun(t, env, f.runID)
			if after.Status != status || after.FailOrigin.Valid || after.FailureReason.Valid ||
				after.CodexClaimEpoch != stale.CodexClaimEpoch || after.ClaimGeneration != stale.ClaimGeneration {
				t.Fatalf("stale run mutated: status=%s origin=%v reason=%v epoch=%d generation=%d",
					after.Status, after.FailOrigin, after.FailureReason, after.CodexClaimEpoch, after.ClaimGeneration)
			}
			assertClaimRecoveryHold(t, env, hold, "open", false)
		})
	}
}

func TestCodexClaimRecoveryRemintQuarantineNoopLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	env.exec("UPDATE runs SET claim_generation = 1, harness = 'codex' WHERE id = $1", f.runID)
	hold := claimRecoveryHold(t, env, f.runID, f.workerID, 1)
	stale := mustRun(t, env, f.runID)
	wire := env.mintCap(t, f.runID, f.workerID)
	epoch, secret, ok := parseCodexCapability(wire)
	if !ok {
		t.Fatal("minted capability did not parse")
	}
	env.exec(`UPDATE codex_provider_account SET coord_state = 'quarantined'
		WHERE id = (SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1)`, f.aliasID)
	f.svc.txBeginner = env.pool
	payload, err := f.svc.finishRunClaim(env.ctx, stale, nil,
		errors.Join(errCredentialUnavailable, ErrCodexAccountQuarantined),
		claimRecoveryIdentity{workerID: f.workerID, recoveryCapable: true})
	if err != nil || payload != nil {
		t.Fatalf("stale remint finishRunClaim = (%v, %v), want idle", payload, err)
	}
	after := mustRun(t, env, f.runID)
	if after.Status != "claimed" || after.CodexClaimEpoch != epoch ||
		string(after.CodexCapHash) != string(hashCodexCapability(secret)) ||
		after.FailOrigin.Valid || after.RecoveryWaitCause.Valid {
		t.Fatalf("reminted run mutated: status=%s epoch=%d origin=%v cause=%v",
			after.Status, after.CodexClaimEpoch, after.FailOrigin, after.RecoveryWaitCause)
	}
	assertClaimRecoveryHold(t, env, hold, "open", false)
}

func TestCodexClaimRecoverySuccessfulAssemblyAuthorityLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, accountUpdate, wantStatus, wantOrigin, wantCause string
	}{
		{"quarantine", "coord_state = 'quarantined'", "recovery_wait", "", "codex_account_unavailable"},
		{"revision revoked", "credential_revision = credential_revision + 1", "failed", "credential_unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newSubscriptionFixture(t, env)
			env.exec("UPDATE runs SET claim_generation = 1, harness = 'codex' WHERE id = $1", f.runID)
			hold := claimRecoveryHold(t, env, f.runID, f.workerID, 1)
			wire := env.mintCap(t, f.runID, f.workerID)
			minted := mustRun(t, env, f.runID)
			env.exec(`UPDATE codex_provider_account SET `+tc.accountUpdate+
				` WHERE id = (SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1)`, f.aliasID)
			f.svc.txBeginner = env.pool
			payload := &ClaimPayload{Secrets: ClaimSecrets{Codex: &ClaimCodexSecrets{Capability: wire}}}
			got, err := f.svc.finishRunClaim(env.ctx, minted, payload, nil,
				claimRecoveryIdentity{workerID: f.workerID, recoveryCapable: true})
			if err != nil || got != nil {
				t.Fatalf("finishRunClaim = (%v, %v), want idle", got, err)
			}
			after := mustRun(t, env, f.runID)
			if after.Status != tc.wantStatus || after.FailOrigin.String != tc.wantOrigin ||
				after.RecoveryWaitCause.String != tc.wantCause ||
				after.CodexClaimEpoch != minted.CodexClaimEpoch+1 || len(after.CodexCapHash) != 0 {
				t.Fatalf("authority transition: status=%s origin=%v cause=%v epoch=%d hash=%x",
					after.Status, after.FailOrigin, after.RecoveryWaitCause, after.CodexClaimEpoch, after.CodexCapHash)
			}
			assertClaimRecoveryHold(t, env, hold, "released", true)
		})
	}
}

func TestCodexClaimRecoveryExactHoldLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	olderWorker := uuid.New()
	env.exec("INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')",
		olderWorker, f.userID, "older-"+olderWorker.String(), olderWorker[:])
	env.exec("UPDATE runs SET claim_generation = 2, harness = 'codex' WHERE id = $1", f.runID)
	for _, h := range []struct {
		generation int64
		worker     uuid.UUID
	}{{1, olderWorker}, {2, f.workerID}} {
		env.exec(`INSERT INTO recovery_custody_holds
			(id, user_id, repo_id, run_id, generation, state, original_worker_id,
			 original_worker_identity, live_worker_id, live_run_id)
			SELECT $1, user_id, repo_id, id, $2, 'open', $3, $4, $3, id
			FROM runs WHERE id = $5`,
			uuid.New(), h.generation, h.worker, "worker-"+h.worker.String(), f.runID)
	}
	env.exec(`UPDATE codex_provider_account SET coord_state = 'quarantined'
		WHERE id = (SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1)`, f.aliasID)
	run := mustRun(t, env, f.runID)
	f.svc.txBeginner = env.pool
	payload, err := f.svc.finishRunClaim(env.ctx, run, nil,
		errors.Join(errCredentialUnavailable, ErrCodexAccountQuarantined),
		claimRecoveryIdentity{workerID: f.workerID, recoveryCapable: true})
	if err != nil || payload != nil {
		t.Fatalf("finishRunClaim = (%v, %v), want idle", payload, err)
	}
	var status, cause string
	var hash []byte
	var epoch int64
	var worker uuid.UUID
	if err := env.pool.QueryRow(env.ctx, `SELECT status, recovery_wait_cause, codex_cap_hash,
		codex_claim_epoch, worker_id FROM runs WHERE id = $1`, f.runID).
		Scan(&status, &cause, &hash, &epoch, &worker); err != nil {
		t.Fatal(err)
	}
	if status != "recovery_wait" || cause != "codex_account_unavailable" || len(hash) != 0 ||
		epoch != run.CodexClaimEpoch+1 || worker != olderWorker {
		t.Fatalf("park status=%s cause=%s hash=%x epoch=%d worker=%s", status, cause, hash, epoch, worker)
	}
	var openOld, releasedCurrent int
	if err := env.pool.QueryRow(env.ctx, `SELECT
		count(*) FILTER (WHERE generation = 1 AND state = 'open'),
		count(*) FILTER (WHERE generation = 2 AND state = 'released')
		FROM recovery_custody_holds WHERE run_id = $1`, f.runID).Scan(&openOld, &releasedCurrent); err != nil {
		t.Fatal(err)
	}
	if openOld != 1 || releasedCurrent != 1 {
		t.Fatalf("holds old=%d current released=%d", openOld, releasedCurrent)
	}
}

func TestCodexClaimRecoveryAfterMintTerminalLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	env.exec("UPDATE runs SET claim_generation = 1, harness = 'codex' WHERE id = $1", f.runID)
	run := mustRun(t, env, f.runID)
	wire := env.mintCap(t, f.runID, f.workerID)
	epoch, secret, ok := parseCodexCapability(wire)
	if !ok {
		t.Fatal("minted capability did not parse")
	}
	f.svc.txBeginner = env.pool
	_, err := f.svc.finishRunClaim(env.ctx, run, nil,
		&codexMintedClaimError{cause: errCredentialUnavailable, epoch: epoch, hash: hashCodexCapability(secret)},
		claimRecoveryIdentity{workerID: f.workerID, recoveryCapable: false})
	if err != nil {
		t.Fatal(err)
	}
	after := mustRun(t, env, f.runID)
	if after.Status != "failed" || after.FailOrigin.String != "credential_unavailable" ||
		after.CodexClaimEpoch != epoch+1 || len(after.CodexCapHash) != 0 {
		t.Fatalf("terminal status=%s origin=%s epoch=%d hash=%x",
			after.Status, after.FailOrigin.String, after.CodexClaimEpoch, after.CodexCapHash)
	}
}
