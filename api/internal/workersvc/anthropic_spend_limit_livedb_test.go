package workersvc

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3767: the owner-held recovery_wait cause 'anthropic_spend_limit', against a REAL
// Postgres. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh); a package that prints `ok` with PASS=0 is INVALID, not green.

// clearSpendLimitCause removes the cause from this owner's rows when the test ends: the other
// LiveDB rollback tests share the database and narrow runs_recovery_wait_cause_check back to a
// vocabulary without it, which any leftover row (held, cancelled or queued) would violate.
func clearSpendLimitCause(t *testing.T, exec func(sql string, args ...any), userID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		exec(`UPDATE runs SET recovery_wait_cause=NULL WHERE user_id=$1 AND recovery_wait_cause='anthropic_spend_limit'`, userID)
	})
}

func spendLimitReport(gen int64) StateRequest {
	return StateRequest{State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseAnthropicSpendLimit), ClaimGeneration: i64Ptr(gen)}
}

func (e interlockLiveDB) spendLimitService(t *testing.T) *Service {
	t.Helper()
	svc := New(e.q, newBox(t), testParams())
	svc.SetTxBeginner(e.pool)
	svc.SetBackground(func(func()) {})
	return svc
}

// spendLimitHeld parks a fresh running run (generation 2, one open custody hold) through the real
// SetState and returns it with the worker.
func (e interlockLiveDB) spendLimitHeld(t *testing.T, svc *Service) (uuid.UUID, uuid.UUID, store.Run) {
	t.Helper()
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 2)
	e.exec(t, `UPDATE runs SET started_at=now(), session_id='retained' WHERE id=$1`, id)
	e.exec(t, `INSERT INTO recovery_custody_holds
     (user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
     VALUES ($1,$2,$3,2,'open',$4,'fixture',$4,$3,false)`, e.userID, e.repoID, id, w)
	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, spendLimitReport(2))
	if err != nil || !applied {
		t.Fatalf("spend-limit park applied=%v err=%v", applied, err)
	}
	return id, w, run
}

func TestAnthropicSpendLimitParkStoresNullRetryLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	clearSpendLimitCause(t, func(q string, a ...any) { e.exec(t, q, a...) }, e.userID)
	svc := e.spendLimitService(t)
	id, w, run := e.spendLimitHeld(t, svc)
	if run.Status != "recovery_wait" || run.RecoveryWaitCause.String != "anthropic_spend_limit" || run.RecoveryRetryNotBefore.Valid ||
		!run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != w || run.SessionID.String != "retained" || run.ClaimReleasedAt.Valid {
		t.Fatalf("held run = %+v", run)
	}
	var open int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open'`, id).Scan(&open); err != nil || open != 1 {
		t.Fatalf("open custody holds=%d/%v", open, err)
	}
	// A redelivery is a no-op and changes nothing.
	before, _ := e.q.GetRunByID(e.ctx, id)
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, spendLimitReport(2)); err != nil || applied {
		t.Fatalf("redelivery applied=%v err=%v", applied, err)
	}
	if after, _ := e.q.GetRunByID(e.ctx, id); !reflect.DeepEqual(before, after) {
		t.Fatal("redelivery mutated the held run")
	}
}

func TestAnthropicSpendLimitParkFencesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	clearSpendLimitCause(t, func(q string, a ...any) { e.exec(t, q, a...) }, e.userID)
	svc := e.spendLimitService(t)
	w := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, w, 2)
	// Nil generation is refused before any write.
	nilGen := spendLimitReport(2)
	nilGen.ClaimGeneration = nil
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, nilGen); err == nil || applied {
		t.Fatalf("nil generation applied=%v err=%v", applied, err)
	}
	// A stale generation is a stale claim.
	if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, spendLimitReport(1)); !errors.Is(err, ErrStaleClaim) || applied {
		t.Fatalf("stale applied=%v err=%v", applied, err)
	}
	if got, _ := e.q.GetRunByID(e.ctx, id); got.Status != "running" || got.RecoveryWaitCause.Valid {
		t.Fatalf("refused park changed the run: %+v", got)
	}
	// A stamped stop wins: the worker's report cancels the run instead of parking it.
	e.exec(t, `UPDATE runs SET stop_kind='cancelled' WHERE id=$1`, id)
	if _, _, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, spendLimitReport(2)); err != nil {
		t.Fatalf("stamped stop: %v", err)
	}
	if got, _ := e.q.GetRunByID(e.ctx, id); got.Status != "cancelled" {
		t.Fatalf("stamped stop status=%s, want cancelled", got.Status)
	}
}

func TestAnthropicSpendLimitHoldImmuneToSweepsAndLateReportsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	clearSpendLimitCause(t, func(q string, a ...any) { e.exec(t, q, a...) }, e.userID)
	svc := e.spendLimitService(t)
	id, w, _ := e.spendLimitHeld(t, svc)
	before, _ := e.q.GetRunByID(e.ctx, id)
	if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now().Add(365*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.PromoteVaultLockedRecoveryWaitRuns(e.ctx, e.userID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"running", "failed", "completed", "awaiting_input", "awaiting_followup", "awaiting_approval"} {
		if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, id, StateRequest{State: state, ClaimGeneration: i64Ptr(2)}); err != nil || applied {
			t.Fatalf("late %s applied=%v err=%v", state, applied, err)
		}
	}
	if n, err := e.q.ReleaseCustodyHoldExact(e.ctx, store.ReleaseCustodyHoldExactParams{RunID: id, Generation: 2, WorkerID: w, ReleaseEvidence: pgconv.TextOrNull("forge_no_output")}); err != nil || n != 0 {
		t.Fatalf("custody release=%d/%v", n, err)
	}
	after, _ := e.q.GetRunByID(e.ctx, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("sweep or late report changed the hold:\n%+v\n%+v", before, after)
	}
}

func TestAnthropicSpendLimitResumeGuardsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	clearSpendLimitCause(t, func(q string, a ...any) { e.exec(t, q, a...) }, e.userID)
	svc := e.spendLimitService(t)
	id, _, _ := e.spendLimitHeld(t, svc)
	before, _ := e.q.GetRunByID(e.ctx, id)
	if _, err := e.q.ResumeAnthropicSpendLimitHold(e.ctx, store.ResumeAnthropicSpendLimitHoldParams{ID: id, UserID: uuid.New(), GlobalTimeoutSeconds: 86400}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign resume=%v", err)
	}
	if after, _ := e.q.GetRunByID(e.ctx, id); !reflect.DeepEqual(before, after) {
		t.Fatal("foreign resume mutated the run")
	}
	resumed, err := e.q.ResumeAnthropicSpendLimitHold(e.ctx, store.ResumeAnthropicSpendLimitHoldParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 86400})
	if err != nil || resumed.Status != "queued" || resumed.RecoveryWaitCause.Valid || !resumed.ClaimReleasedAt.Valid {
		t.Fatalf("resume=%+v err=%v", resumed, err)
	}
	if _, err := e.q.ResumeAnthropicSpendLimitHold(e.ctx, store.ResumeAnthropicSpendLimitHoldParams{ID: id, UserID: e.userID, GlobalTimeoutSeconds: 86400}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate resume=%v", err)
	}
}

// set-token is one of the owner's three ways out: an owner switch queues the run on the new
// secret; a foreign user is refused and the row is unchanged.
func TestAnthropicSpendLimitSetTokenLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	clearSpendLimitCause(t, env.exec, o.userID)
	id := seedRunInStatus(t, env, o, 4890, "recovery_wait", time.Now().UTC())
	env.exec(`UPDATE runs SET recovery_wait_cause='anthropic_spend_limit', recovery_retry_not_before=NULL WHERE id=$1`, id)
	before := mustRun(t, env, id)
	if _, err := svc.SetRunCredential(env.ctx, uuid.New(), id, CredentialOverrideModePinned, &o.altTok); err == nil {
		t.Fatal("foreign set-token succeeded")
	}
	if after := mustRun(t, env, id); !reflect.DeepEqual(before, after) {
		t.Fatal("refused foreign set-token changed the run")
	}
	if _, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok); err != nil {
		t.Fatalf("owner set-token: %v", err)
	}
	got := mustRun(t, env, id)
	if got.Status != "queued" || got.CredentialOverrideMode.String != CredentialOverrideModePinned ||
		!got.CredentialOverrideSecretID.Valid || uuid.UUID(got.CredentialOverrideSecretID.Bytes) != o.altTok {
		t.Fatalf("after set-token: status=%s override=%v/%v", got.Status, got.CredentialOverrideMode, got.CredentialOverrideSecretID)
	}
}

// Issue #3767: set-token on a spend-limit hold must fence the OLD claim. A delayed
// old-generation `failed` (or a second spend park) from the worker that was running generation G
// must not overwrite the owner's decision before the run is reclaimed.
func TestAnthropicSpendLimitSetTokenFencesOldClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.SetBackground(func(func()) {})
	userID, workerID, repoID := env.seedCodexInfra(t)
	clearSpendLimitCause(t, env.exec, userID)
	env.sealBotPAT(t, userID)
	tokenA := env.seedAnthropicSecret(t, userID, "fence-worker-a-"+uuid.NewString(), true)
	tokenB := env.seedAnthropicSecret(t, userID, "fence-pinned-b-"+uuid.NewString(), false)
	tokenC := env.seedAnthropicSecret(t, userID, "fence-pinned-c-"+uuid.NewString(), false)
	const iid = 4893
	env.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
	          VALUES ($1, $2, $3, 'spend fence issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`, uuid.New(), repoID, int64(iid))
	waitFalse := false
	run, err := svc.CreateRun(env.ctx, userID, repoID, iid, "desc", &waitFalse, nil, false, nil, nil, nil,
		&RawCredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &tokenB})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	env.exec(`UPDATE workers SET protocol_capabilities = array_append(protocol_capabilities, 'repo_agent_source_v1') WHERE id = $1`, workerID)
	wkr := store.Worker{ID: workerID, UserID: userID, Name: "w", Status: "online", ProtocolCapabilities: []string{"repo_agent_source_v1"},
		AnthropicBindMode: BindModePinned, AnthropicSecretID: pgconv.UUID(tokenA)}
	if p, err := svc.Claim(env.ctx, wkr, nil); err != nil || p == nil || p.RunID != run.ID.String() {
		t.Fatalf("first claim %+v err=%v", p, err)
	}
	claimed := mustRun(t, env, run.ID)
	gen := claimed.ClaimGeneration
	env.exec(`UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	if _, applied, err := svc.SetState(env.ctx, wkr, run.ID, spendLimitReport(gen)); err != nil || !applied {
		t.Fatalf("park applied=%v err=%v", applied, err)
	}
	if held := mustRun(t, env, run.ID); held.Status != "recovery_wait" || held.ClaimReleasedAt.Valid {
		t.Fatalf("held run = status %s claim_released_at valid=%v, want recovery_wait with the claim not yet released", held.Status, held.ClaimReleasedAt.Valid)
	}

	if _, err := svc.SetRunCredential(env.ctx, userID, run.ID, CredentialOverrideModePinned, &tokenC); err != nil {
		t.Fatalf("owner set-token: %v", err)
	}
	afterSwitch := mustRun(t, env, run.ID)
	if afterSwitch.Status != "queued" || !afterSwitch.ClaimReleasedAt.Valid {
		t.Fatalf("after set-token: status=%s claim_released_at valid=%v, want queued with the old claim fenced", afterSwitch.Status, afterSwitch.ClaimReleasedAt.Valid)
	}

	// BEFORE any reclaim: the old worker (same id, generation G) reports failed, then tries a
	// second spend park. Neither may apply.
	if _, applied, _ := svc.SetState(env.ctx, wkr, run.ID, StateRequest{State: "failed", ClaimGeneration: i64Ptr(gen)}); applied {
		t.Fatal("late old-generation failed applied after set-token")
	}
	if _, applied, _ := svc.SetState(env.ctx, wkr, run.ID, spendLimitReport(gen)); applied {
		t.Fatal("second spend park at the old generation applied after set-token")
	}
	got := mustRun(t, env, run.ID)
	if got.Status != "queued" || !got.ClaimReleasedAt.Valid ||
		got.CredentialOverrideMode.String != CredentialOverrideModePinned ||
		!got.CredentialOverrideSecretID.Valid || uuid.UUID(got.CredentialOverrideSecretID.Bytes) != tokenC {
		t.Fatalf("late reports changed the owner's decision: status=%s released=%v override=%v/%v",
			got.Status, got.ClaimReleasedAt.Valid, got.CredentialOverrideMode, got.CredentialOverrideSecretID)
	}

	// A fresh claim still succeeds and clears the stamp.
	if p, err := svc.Claim(env.ctx, wkr, nil); err != nil || p == nil || p.RunID != run.ID.String() {
		t.Fatalf("reclaim %+v err=%v", p, err)
	}
	if again := mustRun(t, env, run.ID); again.Status != "claimed" || again.ClaimReleasedAt.Valid || again.ClaimGeneration <= gen {
		t.Fatalf("after reclaim: status=%s released=%v gen=%d (was %d)", again.Status, again.ClaimReleasedAt.Valid, again.ClaimGeneration, gen)
	}
}

// Control for the fence above: the stamp is scoped to the spend-limit cause. Another promotable
// cause (forge_unreachable with a retry stamp) leaves claim_released_at exactly as it was.
func TestAnthropicSpendLimitSetTokenOtherCauseKeepsClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	id := seedRunInStatus(t, env, o, 4894, "recovery_wait", time.Now().UTC())
	env.exec(`UPDATE runs SET recovery_wait_cause='forge_unreachable' WHERE id=$1`, id)
	if before := mustRun(t, env, id); before.ClaimReleasedAt.Valid || !before.RecoveryRetryNotBefore.Valid {
		t.Fatalf("fixture: released=%v retry=%v", before.ClaimReleasedAt.Valid, before.RecoveryRetryNotBefore.Valid)
	}
	if _, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok); err != nil {
		t.Fatalf("owner set-token: %v", err)
	}
	if got := mustRun(t, env, id); got.Status != "queued" || got.ClaimReleasedAt.Valid {
		t.Fatalf("after set-token: status=%s claim_released_at valid=%v, want queued with it still NULL", got.Status, got.ClaimReleasedAt.Valid)
	}
}

// A run-pinned run resumed from the hold claims the SAME secret again.
func TestAnthropicSpendLimitResumeKeepsPinnedSecretLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.SetBackground(func(func()) {})
	userID, workerID, repoID := env.seedCodexInfra(t)
	clearSpendLimitCause(t, env.exec, userID)
	env.sealBotPAT(t, userID)
	tokenA := env.seedAnthropicSecret(t, userID, "worker-bound-a-"+uuid.NewString(), true)
	tokenB := env.seedAnthropicSecret(t, userID, "run-pinned-b-"+uuid.NewString(), false)
	const iid = 4891
	env.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
	          VALUES ($1, $2, $3, 'spend issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`, uuid.New(), repoID, int64(iid))
	waitFalse := false
	run, err := svc.CreateRun(env.ctx, userID, repoID, iid, "desc", &waitFalse, nil, false, nil, nil, nil,
		&RawCredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &tokenB})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	env.exec(`UPDATE workers SET protocol_capabilities = array_append(protocol_capabilities, 'repo_agent_source_v1') WHERE id = $1`, workerID)
	wkr := store.Worker{ID: workerID, UserID: userID, Name: "w", Status: "online", ProtocolCapabilities: []string{"repo_agent_source_v1"},
		AnthropicBindMode: BindModePinned, AnthropicSecretID: pgconv.UUID(tokenA)}
	if p, err := svc.Claim(env.ctx, wkr, nil); err != nil || p == nil || p.RunID != run.ID.String() {
		t.Fatalf("first claim %+v err=%v", p, err)
	}
	claimed := mustRun(t, env, run.ID)
	env.exec(`UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	if _, applied, err := svc.SetState(env.ctx, wkr, run.ID, spendLimitReport(claimed.ClaimGeneration)); err != nil || !applied {
		t.Fatalf("park applied=%v err=%v", applied, err)
	}
	if _, err := env.q.ResumeAnthropicSpendLimitHold(env.ctx, store.ResumeAnthropicSpendLimitHoldParams{ID: run.ID, UserID: userID, GlobalTimeoutSeconds: 86400}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if p, err := svc.Claim(env.ctx, wkr, nil); err != nil || p == nil || p.RunID != run.ID.String() {
		t.Fatalf("second claim %+v err=%v", p, err)
	}
	again := mustRun(t, env, run.ID)
	if !again.AnthropicSecretID.Valid || uuid.UUID(again.AnthropicSecretID.Bytes) != tokenB {
		t.Fatalf("resumed claim spent %+v, want the pinned %s", again.AnthropicSecretID, tokenB)
	}
}

// Resume from the hold leaves the run's credential choice to the ladder again: an auto-mode
// worker re-runs autoChoice, so a token that now ranks first is spent, with an auto reason and
// not a pinned one.
func TestAnthropicSpendLimitAutoResumeReentersAutoChoiceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	// autoParams carries a real autoselect policy; testParams() leaves it zero (every gauge stale).
	svc := New(env.q, env.box, autoParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.SetBackground(func(func()) {})
	userID, workerID, repoID := env.seedCodexInfra(t)
	clearSpendLimitCause(t, env.exec, userID)
	env.sealBotPAT(t, userID)
	tokenA := env.seedAnthropicSecret(t, userID, "pool-a-"+uuid.NewString(), true)
	tokenB := env.seedAnthropicSecret(t, userID, "pool-b-"+uuid.NewString(), false)
	env.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = ANY($1)`, []uuid.UUID{tokenA, tokenB})
	env.exec(`UPDATE workers SET anthropic_bind_mode = 'auto' WHERE id = $1`, workerID)
	// Only A is measured at first, so A is the one auto pick.
	env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
	          VALUES ($1, $2, 20, 10, 'usage_endpoint', now())`, tokenA, userID)
	const iid = 4892
	env.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
	          VALUES ($1, $2, $3, 'spend issue', 'opened', '["uzi"]', 'https://forge.e2e/i', false, now(), now())`, uuid.New(), repoID, int64(iid))
	waitFalse := false
	run, err := svc.CreateRun(env.ctx, userID, repoID, iid, "desc", &waitFalse, nil, false, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	env.exec(`UPDATE workers SET protocol_capabilities = array_append(protocol_capabilities, 'repo_agent_source_v1') WHERE id = $1`, workerID)
	wkr := store.Worker{ID: workerID, UserID: userID, Name: "w", Status: "online", ProtocolCapabilities: []string{"repo_agent_source_v1"},
		AnthropicBindMode: BindModeAuto}
	if p, err := svc.Claim(env.ctx, wkr, nil); err != nil || p == nil || p.RunID != run.ID.String() {
		t.Fatalf("first claim %+v err=%v", p, err)
	}
	first := mustRun(t, env, run.ID)
	if !first.AnthropicSecretID.Valid || uuid.UUID(first.AnthropicSecretID.Bytes) != tokenA {
		t.Fatalf("first claim spent %+v, want pooled token A %s", first.AnthropicSecretID, tokenA)
	}
	env.exec(`UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	if _, applied, err := svc.SetState(env.ctx, wkr, run.ID, spendLimitReport(first.ClaimGeneration)); err != nil || !applied {
		t.Fatalf("park applied=%v err=%v", applied, err)
	}
	// While held, A stops being measurable and B becomes the only measured pooled token.
	env.exec(`DELETE FROM anthropic_rate_limits WHERE user_secret_id = $1`, tokenA)
	env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
	          VALUES ($1, $2, 5, 5, 'usage_endpoint', now())`, tokenB, userID)
	if _, err := env.q.ResumeAnthropicSpendLimitHold(env.ctx, store.ResumeAnthropicSpendLimitHoldParams{ID: run.ID, UserID: userID, GlobalTimeoutSeconds: 86400}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if p, err := svc.Claim(env.ctx, wkr, nil); err != nil || p == nil || p.RunID != run.ID.String() {
		t.Fatalf("second claim %+v err=%v", p, err)
	}
	again := mustRun(t, env, run.ID)
	if !again.AnthropicSecretID.Valid || uuid.UUID(again.AnthropicSecretID.Bytes) != tokenB {
		t.Fatalf("resumed claim spent %+v (reason %q), want the now top-ranked pooled token B %s (A=%s)", uuid.UUID(again.AnthropicSecretID.Bytes), again.AnthropicSelectReason.String, tokenB, tokenA)
	}
	switch again.AnthropicSelectReason.String {
	case string(autoselect.ReasonAuto), string(autoselect.ReasonBestOfPool):
	default:
		t.Fatalf("selection reason %q (valid=%v), want an auto-select reason", again.AnthropicSelectReason.String, again.AnthropicSelectReason.Valid)
	}
}

// An owner cancel ends a spend-limit hold; another user's cancel is refused and the hold stays.
func TestAnthropicSpendLimitParkedCancelLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	clearSpendLimitCause(t, func(q string, a ...any) { e.exec(t, q, a...) }, e.userID)
	svc := e.spendLimitService(t)
	id, _, held := e.spendLimitHeld(t, svc)
	if held.Status != "recovery_wait" || held.RecoveryWaitCause.String != "anthropic_spend_limit" {
		t.Fatalf("not parked: %+v", held)
	}
	before, _ := e.q.GetRunByID(e.ctx, id)
	if _, err := svc.SubmitInput(e.ctx, uuid.New(), id, "cancel", "", nil); err == nil {
		t.Fatal("foreign cancel succeeded")
	}
	if after, _ := e.q.GetRunByID(e.ctx, id); !reflect.DeepEqual(before, after) {
		t.Fatalf("foreign cancel changed the hold:\n%+v\n%+v", before, after)
	}
	if _, err := svc.SubmitInput(e.ctx, e.userID, id, "cancel", "no longer needed", nil); err != nil {
		t.Fatalf("owner cancel: %v", err)
	}
	got, _ := e.q.GetRunByID(e.ctx, id)
	if got.Status != "cancelled" {
		t.Fatalf("after owner cancel: status=%s", got.Status)
	}
}
