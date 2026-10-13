package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #2460 acceptance 4 at the claim: a Claude plan-checker child (checking a Codex lead)
// whose Anthropic credential is unusable decides its check failed/checker_unavailable and
// delivers nothing, and never falls back to a Codex checker or to a Codex credential.

// claudeCheckerFix is a Codex lead running on one worker and a queued Claude checker child
// claimable by another worker whose Anthropic binding is pinned or auto. The owner also holds
// a usable Codex credential, which must never be spent for the Claude checker.
type claudeCheckerFix struct {
	env       codexTestEnv
	svc       *Service
	userID    uuid.UUID
	workerID  uuid.UUID
	lead, kid uuid.UUID
	tok       uuid.UUID // the Anthropic credential the claiming worker would spend
}

const claudeCheckerFailureReason = "plan cross-check: checker unavailable"

// newClaudeCheckerFix builds the fixture. bindMode BindModePinned binds the worker to tok;
// BindModeAuto leaves the auto pool EMPTY (no pooled token) with tok a non-pooled default.
func newClaudeCheckerFix(t *testing.T, bindMode string) *claudeCheckerFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, bindMode, false)
	env.sealBotPAT(t, o.userID)
	t.Cleanup(func() { env.exec(`DELETE FROM users WHERE id=$1`, o.userID) })
	fx := &claudeCheckerFix{env: env, userID: o.userID, workerID: o.workerID, kid: uuid.New(), lead: uuid.New()}

	codexKey := env.seedStaticAPIKey(t, o.userID, "codex-key", "fixture-"+uuid.NewString())
	env.exec(`UPDATE user_secrets SET is_default=true WHERE id=$1`, codexKey)
	switch bindMode {
	case BindModePinned:
		fx.tok = o.deadTok
		env.exec(`UPDATE workers SET anthropic_secret_id=$2 WHERE id=$1`, o.workerID, fx.tok)
	case BindModeAuto:
		// Empty pool: nothing is auto_eligible, and the only token is the non-pooled default.
		env.exec(`UPDATE user_secrets SET auto_eligible=false WHERE id=ANY($1)`, []uuid.UUID{o.deadTok, o.altTok})
		env.exec(`DELETE FROM user_secrets WHERE id=ANY($1)`, []uuid.UUID{o.deadTok, o.altTok})
		fx.tok = env.seedAnthropicSecret(t, o.userID, "default-"+uuid.NewString(), true)
	}
	env.exec(`UPDATE workers SET last_heartbeat_at=now(), snapshot_register_nonce=$2, protocol_capabilities=$3 WHERE id=$1`,
		o.workerID, "nonce-"+uuid.NewString(), []string{capability.CrossCheckV1, capability.CrossCheckCodexLeadV1})

	leadWorker := uuid.New()
	env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status) VALUES ($1,$2,$3,$4,'online')`,
		leadWorker, o.userID, "lead-"+leadWorker.String(), leadWorker[:])
	env.exec(`INSERT INTO runs (id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,harness,
		auto_approve,plan_cross_check_required,claim_generation)
		VALUES ($1,$2,$3,$4,2460,'lead issue','lead body','running','codex',true,true,1)`, fx.lead, o.userID, o.repoID, leadWorker)
	env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,target_run_id,harness,report_only,budget_wall_seconds,status,
		priority,trigger_source,auto_approve,dispatched_at,issue_title,issue_description,required_capabilities)
		VALUES ($1,$2,$3,'cross_check',$4,'claude',true,1800,'queued',2,'cross_check',true,now(),'lead issue','lead body','{}')`,
		fx.kid, o.userID, o.repoID, fx.lead)
	c := PlanCrossCheckCandidate{PlanMd: "Review this plan", Milestones: json.RawMessage(`[]`),
		RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s",
		BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff --git a/file b/file"}
	digest, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,
		base_commit,planning_diff,candidate_digest,checker_run_id,checker_harness,deadline_at,created_at)
		VALUES ($1,'plan',1,1,$2,$3,'s',$4,$5,$6,$7,'claude',now()+interval '30 minutes',now()-interval '90 seconds')`,
		fx.lead, c.PlanMd, c.Milestones, c.BaseCommit, c.PlanningDiff, digest, fx.kid)
	fx.svc = New(env.q, env.box, testParams())
	fx.svc.SetTxBeginner(env.pool)
	return fx
}

func (fx *claudeCheckerFix) claim(t *testing.T) *ClaimPayload {
	t.Helper()
	ctx, cancel := context.WithTimeout(fx.env.ctx, 30*time.Second)
	defer cancel()
	p, err := fx.svc.Claim(ctx, wkrRow(t, fx.env, fx.workerID), nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return p
}

func (fx *claudeCheckerFix) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := fx.env.pool.QueryRow(fx.env.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertCheckerUnavailable is the whole acceptance-4 outcome for one Claude child.
// A credential failure carries no detail (exactly claudeCheckerFailureReason); an intrinsic pin
// failure keeps the pin detail after that prefix, so it passes detailedReason.
func (fx *claudeCheckerFix) assertCheckerUnavailable(t *testing.T, payload *ClaimPayload, wantCredentialUntouched bool, detailedReason ...bool) {
	t.Helper()
	if payload != nil {
		t.Fatalf("a credential-less checker delivered a payload (anthropic token set=%v, codex=%v)",
			payload.Secrets.AnthropicOAuthToken != "", payload.Secrets.Codex != nil)
	}
	child := mustRun(t, fx.env, fx.kid)
	reasonOK := child.FailureReason.String == claudeCheckerFailureReason
	if len(detailedReason) > 0 && detailedReason[0] {
		reasonOK = strings.HasPrefix(child.FailureReason.String, claudeCheckerFailureReason+": ")
	}
	if child.Status != "failed" || !reasonOK || child.FailOrigin.String != "guardrail_blocked" {
		t.Fatalf("child status=%s reason=%q origin=%q, want failed / %q / guardrail_blocked",
			child.Status, child.FailureReason.String, child.FailOrigin.String, claudeCheckerFailureReason)
	}
	var verdict, reason string
	var credited bool
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT verdict,reason_class,wait_credited FROM cross_checks WHERE lead_run_id=$1`, fx.lead).
		Scan(&verdict, &reason, &credited); err != nil {
		t.Fatal(err)
	}
	if verdict != "failed" || reason != "checker_unavailable" || !credited {
		t.Fatalf("check verdict=%s reason=%s credited=%v, want failed/checker_unavailable/credited", verdict, reason, credited)
	}
	if paused := mustRun(t, fx.env, fx.lead).BudgetPausedSeconds; paused < 85 || paused > 150 {
		t.Fatalf("lead wait banked %ds, want the ~90s the check was pending, once", paused)
	}
	if n := fx.count(t, `SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'`, fx.lead); n != 1 {
		t.Fatalf("cross_check children for the lead = %d, want only the failed Claude child (never a Codex checker)", n)
	}
	if n := fx.count(t, `SELECT count(*) FROM run_messages WHERE run_id=$1 AND kind='cross_check'`, fx.lead); n != 1 {
		t.Fatalf("lead cross_check events = %d, want exactly one decision event", n)
	}
	if child.CodexSecretID.Valid || len(child.CodexCapHash) != 0 {
		t.Fatalf("Claude checker was bound to a Codex credential: secret=%v capability minted=%v", child.CodexSecretID, len(child.CodexCapHash) != 0)
	}
	if wantCredentialUntouched {
		if child.AnthropicSecretID.Valid {
			t.Fatalf("an Anthropic credential was recorded on the child: %v", child.AnthropicSecretID)
		}
		if n := fx.count(t, `SELECT count(*) FROM run_credential_epochs WHERE run_id=$1`, fx.kid); n != 0 {
			t.Fatalf("credential epochs written for a child that spent nothing: %d", n)
		}
	}
}

func TestClaudeCheckerChildClaimDeliversOnlyAnthropicCustodyLiveDB(t *testing.T) {
	fx := newClaudeCheckerFix(t, BindModePinned)
	p := fx.claim(t)
	if p == nil || p.RunID != fx.kid.String() || p.Kind != "cross_check" || p.ClaimGeneration != 1 {
		t.Fatalf("Claude checker not claimed: %+v", p)
	}
	if p.Secrets.AnthropicOAuthToken == "" || p.Secrets.Codex != nil {
		t.Fatalf("custody: anthropic token set=%v, codex present=%v; want Anthropic only", p.Secrets.AnthropicOAuthToken != "", p.Secrets.Codex != nil)
	}
	if p.CrossCheck == nil || p.CrossCheck.LeadRunID != fx.lead.String() {
		t.Fatalf("checker claim lacks the stored candidate: %+v", p.CrossCheck)
	}
	child := mustRun(t, fx.env, fx.kid)
	if child.Status != "claimed" || child.Harness != "claude" || child.CodexSecretID.Valid || !child.AnthropicSecretID.Valid {
		t.Fatalf("child status=%s harness=%s codex=%v anthropic=%v", child.Status, child.Harness, child.CodexSecretID, child.AnthropicSecretID)
	}
}

// Assembly cannot open the Claude credential: the check is decided checker_unavailable and
// the child fails with the bare reason. The empty-pool case is the regression: an auto pool
// with nothing pooled must not hold the child in pool_wait, spend the non-pooled default, or
// borrow the owner's usable Codex credential.
func TestClaudeCheckerChildCredentialUnavailableAtClaimLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, bind string
		mutate     func(fx *claudeCheckerFix)
		untouched  bool
	}{
		{"deleted token", BindModePinned, func(fx *claudeCheckerFix) { fx.env.exec(`DELETE FROM user_secrets WHERE id=$1`, fx.tok) }, false},
		{"disabled token", BindModePinned, func(fx *claudeCheckerFix) {
			fx.env.exec(`UPDATE user_secrets SET disabled_at=now(), enablement_rev=enablement_rev+1 WHERE id=$1`, fx.tok)
		}, false},
		{"empty auto pool with a non-pooled default and a usable Codex credential", BindModeAuto, func(*claudeCheckerFix) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newClaudeCheckerFix(t, tc.bind)
			tc.mutate(fx)
			p := fx.claim(t)
			fx.assertCheckerUnavailable(t, p, tc.untouched)
		})
	}
}

// An ordinary (run-lane) run still holds in pool_wait on an empty pool: the Claude-checker
// classification is keyed on the cross_check kind and the claude harness only.
func TestOrdinaryRunEmptyAutoPoolStillHoldsInPoolWaitLiveDB(t *testing.T) {
	fx := newClaudeCheckerFix(t, BindModeAuto)
	fx.env.exec(`DELETE FROM cross_checks WHERE lead_run_id=$1`, fx.lead)
	fx.env.exec(`DELETE FROM runs WHERE id=$1`, fx.kid)
	repo := mustRun(t, fx.env, fx.lead).RepoID
	issue := uuid.New()
	fx.env.exec(`INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,status,harness)
		VALUES ($1,$2,$3,77,'ordinary','body','queued','claude')`, issue, fx.userID, repo)
	if p := fx.claim(t); p != nil {
		t.Fatalf("empty pool delivered a payload to an ordinary run")
	}
	if r := mustRun(t, fx.env, issue); r.Status != "pool_wait" {
		t.Fatalf("ordinary run status=%s, want pool_wait", r.Status)
	}
}

// The credential is disabled after assembly opened it. The finisher's locked re-check decides
// the check unavailable and delivers no payload. Whether or not the credential was disabled,
// every Claude checker claim takes the LEAD lock before the child lock: a concurrent lead-lock
// holder must make the finisher wait while the child row is still unlocked.
func TestClaudeCheckerChildFinishTakesLeadLockBeforeChildLiveDB(t *testing.T) {
	for _, disable := range []bool{true, false} {
		name := "assembled credential stays enabled"
		if disable {
			name = "credential disabled after assembly"
		}
		t.Run(name, func(t *testing.T) {
			fx := newClaudeCheckerFix(t, BindModePinned)
			var (
				mu            sync.Mutex
				sawWaiting    bool
				childLockErr  error
				probeFinished = make(chan struct{})
			)
			fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(ctx context.Context, run store.Run, p *ClaimPayload, err error) {
				if err != nil || p == nil {
					t.Errorf("assembly = (%v, %v), want a payload", p != nil, err)
				}
				if disable {
					fx.env.exec(`UPDATE user_secrets SET disabled_at=now(), enablement_rev=enablement_rev+1 WHERE id=$1`, fx.tok)
				}
				holder, berr := fx.env.pool.Begin(ctx)
				if berr != nil {
					t.Errorf("begin lead holder: %v", berr)
					close(probeFinished)
					return
				}
				if _, xerr := holder.Exec(ctx, `SELECT id FROM runs WHERE id=$1 FOR UPDATE`, fx.lead); xerr != nil {
					t.Errorf("lock lead: %v", xerr)
					_ = holder.Rollback(ctx)
					close(probeFinished)
					return
				}
				// The probe outlives the hook's request context only to release its locks.
				bg := context.WithoutCancel(ctx)
				go func() {
					defer close(probeFinished)
					defer func() { _ = holder.Rollback(bg) }()
					deadline := time.Now().Add(10 * time.Second)
					for time.Now().Before(deadline) {
						var waiting int
						if qerr := fx.env.pool.QueryRow(bg, `SELECT count(*) FROM pg_stat_activity
							WHERE datname=current_database() AND wait_event_type='Lock'
							AND query LIKE '%LockPlanCrossCheckLeadForVerdict%'`).Scan(&waiting); qerr == nil && waiting > 0 {
							mu.Lock()
							sawWaiting = true
							mu.Unlock()
							break
						}
						time.Sleep(25 * time.Millisecond)
					}
					probe, perr := fx.env.pool.Begin(bg)
					if perr != nil {
						return
					}
					_, lerr := probe.Exec(bg, `SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT`, fx.kid)
					mu.Lock()
					childLockErr = lerr
					mu.Unlock()
					_ = probe.Rollback(bg)
				}()
			}}
			p := fx.claim(t)
			<-probeFinished
			mu.Lock()
			defer mu.Unlock()
			if !sawWaiting {
				t.Fatal("the finisher never waited on the lead lock: a Claude checker claim must lock the lead first")
			}
			if childLockErr != nil {
				t.Fatalf("the child row was locked while the finisher waited on the lead (child-before-lead order): %v", childLockErr)
			}
			if disable {
				fx.assertCheckerUnavailable(t, p, false)
				return
			}
			if p == nil || p.RunID != fx.kid.String() || p.Secrets.AnthropicOAuthToken == "" || p.Secrets.Codex != nil {
				t.Fatalf("enabled credential must still deliver the Claude-only payload: %+v", p)
			}
		})
	}
}

// crossCheckClaimParams adds the cross-check eligibility clock fn_cross_check_child_eligible
// reads (deadline and affinity grace), which the service's ClaimRun sets from claimNow.
func crossCheckClaimParams(w store.Worker) store.ClaimRunParams {
	p := claimRunParamsFor(w)
	now := time.Now()
	p.CrossCheckEvaluatedAt = pgconv.Time(now)
	p.CrossCheckAffinityCutoff = pgconv.Time(now.Add(-2 * time.Minute))
	return p
}

// A locked vault is transient: the child is requeued and its check stays pending, never
// decided checker_unavailable.
func TestClaudeCheckerChildVaultLockedStaysTransientLiveDB(t *testing.T) {
	fx := newClaudeCheckerFix(t, BindModePinned)
	w := wkrRow(t, fx.env, fx.workerID)
	run, err := fx.env.q.ClaimRun(fx.env.ctx, crossCheckClaimParams(w))
	if err != nil || run.ID != fx.kid {
		t.Fatalf("ClaimRun: id=%s err=%v", run.ID, err)
	}
	payload, err := fx.svc.finishRunClaim(fx.env.ctx, run, nil, errVaultLocked, claimRecoveryIdentity{workerID: fx.workerID})
	if err != nil || payload != nil {
		t.Fatalf("finishRunClaim = (%v, %v), want idle", payload != nil, err)
	}
	if r := mustRun(t, fx.env, fx.kid); r.Status != "queued" || r.FailureReason.Valid {
		t.Fatalf("vault-locked child status=%s reason=%v, want a clean requeue", r.Status, r.FailureReason)
	}
	var verdict string
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT verdict FROM cross_checks WHERE lead_run_id=$1`, fx.lead).Scan(&verdict); err != nil || verdict != "pending" {
		t.Fatalf("check verdict=%q err=%v, want still pending", verdict, err)
	}
	if paused := mustRun(t, fx.env, fx.lead).BudgetPausedSeconds; paused != 0 {
		t.Fatalf("a transient requeue banked the lead's wait: %d", paused)
	}
	if errors.Is(errVaultLocked, errCheckerPinUnavailable) {
		t.Fatal("sentinels must stay distinct")
	}
}

// PRD #2151 x #2460: the Plan cross-check x Claude cell reaches the Claude checker. A pin is
// delivered with model_source=pin; with the cell at Default the checker follows the owner's
// Claude lanes (default_claude_model / default_effort), never the Codex lanes.
func TestClaudeCheckerChildPinsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		model, effort                                    any
		wantModel, wantEffort, modelSource, effortSource string
	}{
		{"pinned sonnet", "sonnet", "high", "sonnet", "high", "pin", "pin"},
		{"model-only pin keeps the worker effort", "sonnet", nil, "sonnet", "low", "pin", "worker default"},
		{"Default follows the Claude worker lanes", nil, nil, "opus", "low", "worker default", "worker default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newClaudeCheckerFix(t, BindModePinned)
			fx.env.exec(`UPDATE users SET default_claude_model='opus',default_effort='low',
				default_codex_model='gpt-6-sol',default_codex_effort='xhigh' WHERE id=$1`, fx.userID)
			fx.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, fx.workerID,
				[]string{capability.CrossCheckV1, capability.CrossCheckCodexLeadV1, capability.CrossCheckPinsV1})
			// A Codex-cell pin, different on purpose, must not leak into the Claude checker.
			fx.env.exec(`INSERT INTO user_cross_check_pins (user_id,stage,harness,model,effort) VALUES ($1,'plan','codex','gpt-6-astra','max')`, fx.userID)
			if tc.model != nil || tc.effort != nil {
				fx.env.exec(`INSERT INTO user_cross_check_pins (user_id,stage,harness,model,effort) VALUES ($1,'plan','claude',$2,$3)`, fx.userID, tc.model, tc.effort)
			}
			p := fx.claim(t)
			if p == nil || p.Config.DefaultModel == nil || p.Config.DefaultEffort == nil || p.CrossCheck == nil ||
				p.CrossCheck.ModelSource == nil || p.CrossCheck.EffortSource == nil {
				t.Fatalf("Claude checker claim lacks model, effort or provenance: %+v", p)
			}
			if *p.Config.DefaultModel != tc.wantModel || *p.Config.DefaultEffort != tc.wantEffort ||
				*p.CrossCheck.ModelSource != tc.modelSource || *p.CrossCheck.EffortSource != tc.effortSource {
				t.Fatalf("delivered %s/%s (%s/%s), want %s/%s (%s/%s)", *p.Config.DefaultModel, *p.Config.DefaultEffort,
					*p.CrossCheck.ModelSource, *p.CrossCheck.EffortSource, tc.wantModel, tc.wantEffort, tc.modelSource, tc.effortSource)
			}
			var model, effort string
			if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT checker_model,checker_effort FROM cross_checks WHERE lead_run_id=$1`, fx.lead).Scan(&model, &effort); err != nil {
				t.Fatal(err)
			}
			if model != tc.wantModel || effort != tc.wantEffort {
				t.Fatalf("recorded checker snapshot %s/%s differs from the delivered claim", model, effort)
			}
		})
	}
}

// A stored non-canonical pin on the active Claude cell is an intrinsic pin failure: the
// check is decided checker_unavailable, lead-first, exactly as for the Codex cell.
func TestClaudeCheckerChildInvalidPinIsCheckerUnavailableLiveDB(t *testing.T) {
	fx := newClaudeCheckerFix(t, BindModePinned)
	fx.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, fx.workerID,
		[]string{capability.CrossCheckV1, capability.CrossCheckCodexLeadV1, capability.CrossCheckPinsV1})
	fx.env.exec(`INSERT INTO user_cross_check_pins (user_id,stage,harness,model) VALUES ($1,'plan','claude','gpt-6-sol')`, fx.userID)
	fx.assertCheckerUnavailable(t, fx.claim(t), false, true)
}

// The assembly guard itself: a Claude child's payload carrying Codex credentials (or no
// Anthropic token) is refused before the check row records any execution.
func TestClaudeCheckerChildAssemblyRefusesLeakedFamilyLiveDB(t *testing.T) {
	fx := newClaudeCheckerFix(t, BindModePinned)
	worker := wkrRow(t, fx.env, fx.workerID)
	child, err := fx.env.q.ClaimRun(fx.env.ctx, crossCheckClaimParams(worker))
	if err != nil || child.ID != fx.kid {
		t.Fatalf("ClaimRun: id=%s err=%v", child.ID, err)
	}
	payload := func(anthropic string, codex *ClaimCodexSecrets) *ClaimPayload {
		p := &ClaimPayload{IssueTitle: "lead issue", IssueDescription: "lead body"}
		p.Secrets.AnthropicOAuthToken, p.Secrets.Codex = anthropic, codex
		return p
	}
	for name, p := range map[string]*ClaimPayload{
		"anthropic token plus Codex credentials": payload("tok", &ClaimCodexSecrets{Capability: "cap"}),
		"Codex credentials only":                 payload("", &ClaimCodexSecrets{Capability: "cap"}),
		"no credential":                          payload("", nil),
	} {
		if err := fx.svc.assemblePlanCrossCheckInput(fx.env.ctx, worker, child, p); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("%s: err=%v, want ErrCrossCheckRefused", name, err)
		}
	}
	var model any
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT checker_model FROM cross_checks WHERE lead_run_id=$1`, fx.lead).Scan(&model); err != nil || model != nil {
		t.Fatalf("a refused claim recorded an execution snapshot: %v (err=%v)", model, err)
	}
	ok := payload("tok", nil)
	if err := fx.svc.assemblePlanCrossCheckInput(fx.env.ctx, worker, child, ok); err != nil || ok.CrossCheck == nil {
		t.Fatalf("Anthropic-only Claude child refused: %v", err)
	}
}
