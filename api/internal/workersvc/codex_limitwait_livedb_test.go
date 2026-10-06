package workersvc

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Observe the actual service path, including queries that must stay absent for Codex.
type codexParkStore struct {
	*store.Queries
	candidates, five, seven int
}

func (q *codexParkStore) ListAutoSelectCandidates(ctx context.Context, id uuid.UUID) ([]store.ListAutoSelectCandidatesRow, error) {
	q.candidates++
	return q.Queries.ListAutoSelectCandidates(ctx, id)
}
func (q *codexParkStore) MarkFiveHourExhausted(ctx context.Context, id uuid.UUID) (int64, error) {
	q.five++
	return q.Queries.MarkFiveHourExhausted(ctx, id)
}
func (q *codexParkStore) MarkSevenDayExhausted(ctx context.Context, id uuid.UUID) (int64, error) {
	q.seven++
	return q.Queries.MarkSevenDayExhausted(ctx, id)
}

func claimCodexForPark(t *testing.T, env codexTestEnv) (*codexClaimFix, store.Worker, *ClaimPayload) {
	t.Helper()
	fx := newCodexClaimFix(t, env, false)
	fx.svc.p.Autoselect = autoParams().Autoselect
	fx.svc.p.RunLimitMaxWaits = 5
	fx.svc.p.RunLimitMaxPark = 8 * 24 * time.Hour
	env.exec("UPDATE runs SET wait_on_limit = true WHERE id = $1", fx.runID)
	env.exec("UPDATE workers SET anthropic_bind_mode = 'auto' WHERE id = $1", fx.workerB)
	w := fx.claimant(t, true)
	p, err := fx.svc.Claim(env.ctx, w, nil)
	if err != nil || p == nil || p.Secrets.Codex == nil {
		t.Fatalf("Claim: payload=%v err=%v", p != nil, err)
	}
	gen := p.ClaimGeneration
	if _, applied, err := fx.svc.SetState(env.ctx, w, fx.runID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("start: applied=%v err=%v", applied, err)
	}
	return fx, w, p
}

func parkCodexFlight(t *testing.T, env codexTestEnv, svc *Service, w store.Worker, id uuid.UUID, gen int64) store.Run {
	t.Helper()
	reset := time.Now().UTC().Add(6 * 24 * time.Hour).Truncate(time.Second).UnixMilli()
	r, applied, err := svc.SetState(env.ctx, w, id, StateRequest{State: "limit_wait", ClaimGeneration: &gen, LimitResetsAt: &reset, RateLimitType: strPtr("seven_day")})
	if err != nil || !applied || r.Status != "limit_wait" {
		t.Fatalf("park: status=%s applied=%v err=%v", r.Status, applied, err)
	}
	return r
}

func TestCodexLimitParkRevokesAndReclaimsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, w, p := claimCodexForPark(t, env)
	// An eligible Anthropic pool on an auto-bound worker must not affect a real Codex claim.
	alt := env.seedAnthropicSecret(t, fx.userID, "pool-"+uuid.NewString(), false)
	env.exec("UPDATE user_secrets SET auto_eligible = true WHERE id = $1", alt)
	env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id,user_id,five_hour_pct,seven_day_pct,source,synced_at)
 VALUES ($1,$2,20,10,'usage_endpoint',now())`, alt, fx.userID)
	candidates, err := env.q.ListAutoSelectCandidates(env.ctx, fx.userID)
	if err != nil || len(candidates) == 0 {
		t.Fatalf("pool precondition: %v %v", candidates, err)
	}
	eligible := false
	for _, candidate := range candidates {
		if candidate.UserSecretID == alt && autoselect.Classify(autoselectrow.FromCandidateRow(candidate), fx.svc.p.Autoselect, time.Now()).Status == autoselect.StatusEligible {
			eligible = true
		}
	}
	if !eligible {
		t.Fatal("Anthropic alternative is not eligible")
	}
	observed := &codexParkStore{Queries: env.q}
	fx.svc.q = observed
	before := mustRun(t, env, fx.runID)
	parked := parkCodexFlight(t, env, fx.svc, w, fx.runID, p.ClaimGeneration)
	if parked.AnthropicSecretID.Valid || parked.LimitDeadSecretID.Valid {
		t.Fatal("Codex acquired an Anthropic exclusion")
	}
	if observed.candidates != 0 || observed.five != 0 || observed.seven != 0 {
		t.Fatalf("Anthropic operations: %+v", observed)
	}
	if lo, hi := parked.LimitResetsAt.Time.Add(limitParkJitterMin), parked.LimitResetsAt.Time.Add(limitParkJitterMax); parked.RetryNotBefore.Time.Before(lo) || parked.RetryNotBefore.Time.After(hi) {
		t.Fatalf("pool lowered reset: %v", parked.RetryNotBefore)
	}
	var five, seven int
	if err := env.pool.QueryRow(env.ctx, "SELECT five_hour_pct, seven_day_pct FROM anthropic_rate_limits WHERE user_secret_id=$1", alt).Scan(&five, &seven); err != nil || five != 20 || seven != 10 {
		t.Fatalf("gauge changed: %d %d %v", five, seven, err)
	}
	reevaluate, err := env.q.ListLimitWaitReeval(env.ctx, pgconv.Time(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reevaluate {
		if r.ID == fx.runID {
			t.Fatal("Codex included in pool reevaluation")
		}
	}
	if len(parked.CodexCapHash) != 0 || parked.CodexClaimEpoch != before.CodexClaimEpoch+1 {
		t.Fatalf("park did not revoke: epoch=%d hash=%x", parked.CodexClaimEpoch, parked.CodexCapHash)
	}
	for _, scope := range []CodexOpScope{ScopePersistRecovery, ScopeStartRefresh, ScopeReleaseAccessToken} {
		if _, err := fx.svc.AuthorizeCodexCredentialOp(env.ctx, w, fx.runID, p.Secrets.Codex.Capability, scope); !errors.Is(err, ErrCodexCapabilityEpoch) {
			t.Fatalf("scope %v: %v", scope, err)
		}
	}
	_, hash := mintCodexCapability()
	if _, err := env.q.SetRunCodexClaimCapability(env.ctx, store.SetRunCodexClaimCapabilityParams{ID: fx.runID, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: p.ClaimGeneration, Hash: hash}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mint after park: %v", err)
	}
	afterMint := mustRun(t, env, fx.runID)
	if afterMint.CodexClaimEpoch != parked.CodexClaimEpoch || len(afterMint.CodexCapHash) != 0 {
		t.Fatal("refused mint changed parked capability")
	}
	promoted, err := env.q.PromoteLimitWaitRuns(env.ctx, pgconv.Time(parked.RetryNotBefore.Time))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range promoted {
		if r.ID == fx.runID {
			found = true
		}
	}
	if !found {
		t.Fatal("timed promotion missed parked run")
	}
	fresh, err := fx.svc.Claim(env.ctx, w, nil)
	if err != nil || fresh == nil || fresh.Secrets.Codex == nil {
		t.Fatalf("reclaim: %v %v", fresh != nil, err)
	}
	live := mustRun(t, env, fx.runID)
	if fresh.ClaimGeneration != p.ClaimGeneration+1 || fresh.Secrets.Codex.Capability == p.Secrets.Codex.Capability || live.CodexClaimEpoch <= parked.CodexClaimEpoch || live.CodexAccountKey != before.CodexAccountKey {
		t.Fatal("reclaim did not mint fresh same-account flight")
	}
	if _, err := fx.svc.AuthorizeCodexCredentialOp(env.ctx, w, fx.runID, fresh.Secrets.Codex.Capability, ScopeReleaseAccessToken); err != nil {
		t.Fatalf("fresh authorization: %v", err)
	}
}

// Isolate the mint fence from park revocation so the regression detects a late
// writer resurrecting even an already-revoked parked flight.
func TestCodexLimitParkLateMintRefusedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)
	env.exec("UPDATE runs SET status='running' WHERE id=$1", f.runID)
	env.mintCap(t, f.runID, f.workerID)
	env.exec("UPDATE runs SET status='limit_wait', codex_cap_hash=NULL, codex_claim_epoch=codex_claim_epoch+1 WHERE id=$1", f.runID)
	before := mustRun(t, env, f.runID)
	_, hash := mintCodexCapability()
	_, err := env.q.SetRunCodexClaimCapability(env.ctx, store.SetRunCodexClaimCapabilityParams{
		ID: f.runID, WorkerID: pgconv.UUID(f.workerID), ClaimGeneration: before.ClaimGeneration, Hash: hash,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late mint: %v, want pgx.ErrNoRows", err)
	}
	after := mustRun(t, env, f.runID)
	if after.CodexClaimEpoch != before.CodexClaimEpoch || len(after.CodexCapHash) != 0 {
		t.Fatal("late mint resurrected parked capability")
	}
}

func TestCodexLimitRefusedParkRetainsCapabilityLiveDB(t *testing.T) {
	for _, name := range []string{"stale generation", "wrong worker", "released legacy", "not running", "judge", "job"} {
		t.Run(name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			f := newSubscriptionFixture(t, env)
			env.exec("UPDATE runs SET status='running', claim_generation=7 WHERE id=$1", f.runID)
			wire := env.mintCap(t, f.runID, f.workerID)
			gen := int64(7)
			arg := store.SetRunLimitWaitParams{ID: f.runID, WorkerID: pgconv.UUID(f.workerID), ClaimGeneration: pgconv.Int8Ptr(&gen), RetryNotBefore: pgconv.Time(time.Now().Add(time.Hour))}
			switch name {
			case "stale generation":
				gen = 6
				arg.ClaimGeneration = pgconv.Int8Ptr(&gen)
			case "wrong worker":
				arg.WorkerID = pgconv.UUID(uuid.New())
			case "released legacy":
				env.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", f.runID)
				arg.ClaimGeneration = pgconv.Int8Ptr(nil)
			case "not running":
				env.exec("UPDATE runs SET status='claimed' WHERE id=$1", f.runID)
			case "judge":
				env.exec("UPDATE runs SET kind='judge', repo_id=NULL, issue_iid=NULL, branch=NULL, target_run_id=$1 WHERE id=$1", f.runID)
			case "job":
				env.exec("UPDATE runs SET kind='job', repo_id=NULL, issue_iid=NULL, branch=NULL, job_type='research' WHERE id=$1", f.runID)
			}
			before := mustRun(t, env, f.runID)
			n, err := env.q.SetRunLimitWait(env.ctx, arg)
			if err != nil || n != 0 {
				t.Fatalf("refused park: %d %v", n, err)
			}
			after := mustRun(t, env, f.runID)
			if after.Status != before.Status || after.LimitWaitCount != before.LimitWaitCount || after.CodexClaimEpoch != before.CodexClaimEpoch || !bytes.Equal(after.CodexCapHash, before.CodexCapHash) {
				t.Fatal("refused park mutated flight")
			}
			if name != "released legacy" {
				if _, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.runID, wire, ScopeReleaseAccessToken); err != nil {
					t.Fatalf("retained capability: %v", err)
				}
			}
		})
	}
}

func TestCodexLimitParkQuarantinePromoteClaimSweepLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, w, p := claimCodexForPark(t, env)
	parked := parkCodexFlight(t, env, fx.svc, w, fx.runID, p.ClaimGeneration)
	fx.setAccount(t, "coord_state = 'quarantined'")
	if _, err := env.q.PromoteLimitWaitRuns(env.ctx, pgconv.Time(parked.RetryNotBefore.Time)); err != nil {
		t.Fatal(err)
	}
	before := mustRun(t, env, fx.runID)
	if before.Status != "queued" {
		t.Fatalf("promotion status=%s", before.Status)
	}
	if got, err := fx.svc.Claim(env.ctx, w, nil); err != nil || got != nil {
		t.Fatalf("quarantined claim: payload=%v err=%v", got != nil, err)
	}
	if r := mustRun(t, env, fx.runID); r.Status != "queued" || r.ClaimGeneration != before.ClaimGeneration {
		t.Fatal("idle claim changed queued flight")
	}
	svc := gateSweepService(env, fx.svc)
	svc.codexPromote.after = uuid.Max
	if _, err := svc.Sweep(env.ctx); err != nil {
		t.Fatal(err)
	}
	assertCodexAccountParked(t, env, fx.runID, before)
}

func TestCodexLimitParkHumanApprovalReclaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, w, p := claimCodexForPark(t, env)
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.InputReceiptsV1)
	plan := "Implement the provider-aware park and preserve the account binding."
	gen := p.ClaimGeneration
	if _, ok, err := fx.svc.SetState(env.ctx, w, fx.runID, StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, SessionID: strPtr("codex-thread-" + uuid.NewString())}); err != nil || !ok {
		t.Fatalf("agent plan: %v %v", ok, err)
	}
	gate := mustRun(t, env, fx.runID)
	if gate.AutoApprove || gate.PlanSource != "agent" {
		t.Fatalf("not a human-gated agent plan: source=%s auto=%v", gate.PlanSource, gate.AutoApprove)
	}
	if _, err := fx.svc.SubmitInput(env.ctx, fx.userID, fx.runID, "approve_plan", "", nil); err != nil {
		t.Fatalf("human approve: %v", err)
	}
	var id int64
	if err := env.pool.QueryRow(env.ctx, "SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'", fx.runID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if receipt, err := fx.svc.AckInputs(env.ctx, w, fx.runID, gen, []int64{id}); err != nil || !receipt.Active {
		t.Fatalf("ACK: %+v %v", receipt, err)
	}
	if receipt, err := fx.svc.ApplyInputs(env.ctx, w, fx.runID, gen, []int64{id}); err != nil || !receipt.Active {
		t.Fatalf("APPLIED: %+v %v", receipt, err)
	}
	if _, ok, err := fx.svc.SetState(env.ctx, w, fx.runID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !ok {
		t.Fatalf("approved start: %v %v", ok, err)
	}
	parked := parkCodexFlight(t, env, fx.svc, w, fx.runID, gen)
	if _, err := env.q.PromoteLimitWaitRuns(env.ctx, pgconv.Time(parked.RetryNotBefore.Time)); err != nil {
		t.Fatal(err)
	}
	resumed, err := fx.svc.Claim(env.ctx, w, nil)
	if err != nil || resumed == nil {
		t.Fatalf("approved reclaim: %v %v", resumed != nil, err)
	}
	r := mustRun(t, env, fx.runID)
	if !resumed.PlanApproved || resumed.ResumePhase != "implementing" || r.PlanMd.String != plan || r.PlanSource != "agent" || r.AutoApprove || r.CodexAccountKey != gate.CodexAccountKey || r.CodexSecretID != gate.CodexSecretID {
		t.Fatal("reclaim lost human approval, plan, or frozen account")
	}
}

func TestCodexLimitPostParkForgeCustodySettlementLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, w, p := claimCodexForPark(t, env)
	parked := parkCodexFlight(t, env, fx.svc, w, fx.runID, p.ClaimGeneration)
	if len(parked.CodexCapHash) != 0 {
		t.Fatal("park failed to revoke capability")
	}
	forge := newMemForge()
	fx.svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	fx.svc.SetPublishFn(forge.publish)
	fx.svc.SetRetentionLockPool(env.pool)
	result, err := fx.svc.Publish(env.ctx, w, fx.runID, retentionTestTip, []byte("pack"))
	if err != nil || !result.Published {
		t.Fatalf("postpark forge publish: %+v %v", result, err)
	}
	if tip, ok := forge.ref(result.Ref); !ok || tip != retentionTestTip {
		t.Fatal("forge did not retain checkpoint")
	}
	rs := recovery.New(env.q, env.pool, nil, recovery.Limits{CustodyHoldLimit: 8}, nil)
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.RecoveryArchiveV2)
	gen := p.ClaimGeneration
	released, err := rs.Release(env.ctx, w, fx.runID, apitypes.RecoveryReleaseRequest{Generation: &gen, ReleaseEvidence: strPtr("publication")})
	if err != nil || !released.Released || released.HoldsReleased != 1 {
		t.Fatalf("postpark custody release: %+v %v", released, err)
	}
	var state, evidence string
	if err := env.pool.QueryRow(env.ctx, "SELECT state, release_evidence FROM recovery_custody_holds WHERE run_id=$1 AND generation=$2", fx.runID, gen).Scan(&state, &evidence); err != nil || state != "released" || evidence != "publication" {
		t.Fatalf("custody settlement: %s %s %v", state, evidence, err)
	}
	if r := mustRun(t, env, fx.runID); r.Status != "limit_wait" || r.CodexClaimEpoch != parked.CodexClaimEpoch || len(r.CodexCapHash) != 0 {
		t.Fatal("custody settlement resurrected credential authority")
	}
}

func TestCodexLimitParkRefreshDiscardsTokenLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	access, refresh := codexToken("rotated-access"), codexToken("rotated-refresh")
	fake := &fakeRefreshClient{result: codexauth.RefreshResult{AccessToken: access, RefreshToken: &refresh}}
	f := newRefreshFixture(t, env, fake)
	f.svc.p = testParams()
	f.svc.p.RunLimitMaxWaits = 5
	f.svc.p.RunLimitMaxPark = 8 * 24 * time.Hour
	f.svc.now = time.Now
	env.exec("UPDATE runs SET status='running', wait_on_limit=true WHERE id=$1", f.runID)
	wire := env.mintCap(t, f.runID, f.workerID)
	gen := mustRun(t, env, f.runID).ClaimGeneration
	revision := env.mustAccount(t, f.userID, f.accountID).CredentialRevision
	fake.onRefresh = func() { parkCodexFlight(t, env, f.svc, f.wkr, f.runID, gen) }
	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, wire, uuid.New(), 0)
	if !errors.Is(err, ErrCodexCapabilityEpoch) || res.AccessToken != "" || res.Outcome != CodexRefreshContended {
		t.Fatalf("refresh: outcome=%v token-present=%v err=%v", res.Outcome, res.AccessToken != "", err)
	}
	acct := env.mustAccount(t, f.userID, f.accountID)
	blob := env.accountBlob(t, f.userID, f.accountID)
	if fake.calls != 1 || acct.Generation != 1 || blob.AccessToken != access || blob.RefreshToken != refresh {
		t.Fatal("park discarded durable account rotation")
	}
	if acct.CredentialRevision != revision {
		t.Fatal("park or refresh changed the account credential revision")
	}
}
