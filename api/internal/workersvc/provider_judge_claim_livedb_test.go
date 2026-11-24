package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_judge_claim_livedb_test.go is PRD #4322 M1a milestone m3 against a real Postgres: a
// provider-bound Claude judge (created by the m2 policy) claims against its frozen binding, a
// revoked or unmappable binding settles it terminally (never a park), a Codex-bound judge stays
// refused, the health reason names the missing judge capability, and a limit report from a provider
// judge never parks or waits. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// run via ./e2e/run-store-it.sh. Every test name ends LiveDB so the store-IT sweep selects it.

var pjFullCaps = []string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1, capability.ClaudeProviderV1, capability.ClaudeProviderJudgeV1}

// pjFix is a jdFix whose gateway key is REALLY sealed and whose worker advertises both provider
// capabilities. The owner also holds a usable default vendor token, so an unintended vendor fallback
// would succeed and fail the test.
type pjFix struct {
	*jdFix
	plain string
}

func newPJFix(t *testing.T) *pjFix {
	t.Helper()
	j := newJdFix(t)
	p := &pjFix{jdFix: j, plain: "gw-key-" + uuid.NewString()}
	sealed, err := j.env.box.Seal([]byte(p.plain))
	if err != nil {
		t.Fatal(err)
	}
	j.env.exec(`UPDATE user_secrets SET ciphertext = $2 WHERE id = $1`, j.key, sealed)
	p.setCaps(pjFullCaps)
	j.env.exec(`UPDATE workers SET last_heartbeat_at = now(), snapshot_register_nonce = $2 WHERE id = $1`, j.workerID, "nonce-"+uuid.NewString())
	return p
}

func (p *pjFix) setCaps(caps []string) {
	p.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, p.workerID, caps)
}

func (p *pjFix) worker() store.Worker { return wkrRow(p.t, p.env, p.workerID) }

// createdJudge creates the provider judge for a fresh target and returns it.
func (p *pjFix) createdJudge() store.Run {
	p.t.Helper()
	target := p.newTarget()
	p.auto(target)
	r := p.mustJudge(target)
	p.wantBoundTo(r, p.provider, p.key, 1)
	return r
}

// wantNoVendor: a provider judge records no vendor credential, epoch or rejection mark.
func (p *pjFix) wantNoVendor(r store.Run) {
	p.t.Helper()
	got := mustRun(p.t, p.env, r.ID)
	if got.AnthropicSecretID.Valid {
		p.t.Fatalf("provider judge recorded a vendor credential: %v", got.AnthropicSecretID)
	}
	if n := p.env.countRunCredentialEpochs(p.t, r.ID); n != 0 {
		p.t.Fatalf("provider judge journaled %d vendor credential epochs", n)
	}
}

func (p *pjFix) wantFailedTerminally(id uuid.UUID, reasonPart string) store.Run {
	p.t.Helper()
	r := mustRun(p.t, p.env, id)
	if r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" || !strings.Contains(r.FailureReason.String, reasonPart) {
		p.t.Fatalf("judge %s: status=%s origin=%v reason=%q hold=%v, want failed/credential_unavailable containing %q (never paused)",
			id, r.Status, r.FailOrigin, r.FailureReason.String, r.HoldReason, reasonPart)
	}
	p.wantNoVendor(r)
	return r
}

func TestProviderJudgeCreateClaimFrozenLiveDB(t *testing.T) {
	p := newPJFix(t)
	judge := p.createdJudge()
	payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
	if err != nil || payload == nil || payload.RunID != judge.ID.String() {
		t.Fatalf("Claim = (%+v, %v), want the provider judge", payload, err)
	}
	b := payload.Secrets.Provider
	if b == nil || payload.Secrets.AnthropicOAuthToken != "" || payload.Secrets.Codex != nil {
		t.Fatalf("secrets = %+v, want only a provider binding", payload.Secrets)
	}
	if b.ProviderID != p.provider.String() || b.Revision != 1 || b.Engine != "claude" || b.BaseURL != pvBase || b.Credential != p.plain ||
		b.ModelMap["opus"].Model != "corp-opus" {
		t.Fatalf("binding = %+v", b)
	}
	if payload.JudgeModel == nil || *payload.JudgeModel != "opus" || !modelMapped(b.ModelMap, *payload.JudgeModel) {
		t.Fatalf("judge model = %v, want the mapped built-in alias", payload.JudgeModel)
	}
	if payload.TargetRunID == nil {
		t.Fatal("the judge claim lost its target")
	}
	if got := mustRun(t, p.env, judge.ID); got.Status != "claimed" {
		t.Fatalf("status = %s, want claimed", got.Status)
	}
	p.wantNoVendor(judge)
}

func TestProviderJudgePostEnqueueNoRebindLiveDB(t *testing.T) {
	p := newPJFix(t)
	judge := p.createdJudge()
	// The owner changes source and pins a vendor token for judges while the judge is queued.
	p.addAlt()
	p.selectAltDirect()
	tok := p.token
	if _, err := p.judge.SetUserJudgeBinding(p.env.ctx, p.userID, BindModePinned, &tok, nil); err != nil {
		t.Fatalf("pin: %v", err)
	}
	payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
	if err != nil || payload == nil || payload.RunID != judge.ID.String() {
		t.Fatalf("Claim = (%+v, %v)", payload, err)
	}
	b := payload.Secrets.Provider
	if b == nil || b.ProviderID != p.provider.String() || b.Credential != p.plain || payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("secrets = %+v, want the FROZEN binding and no vendor token", payload.Secrets)
	}
	p.wantNoVendor(judge)
}

func TestProviderJudgeRevocationFinalDeliveryLiveDB(t *testing.T) {
	revocations := []struct {
		name   string
		revoke func(p *pjFix)
		reason string
	}{
		{"key disabled", func(p *pjFix) {
			p.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
		}, holdProviderBindingStale},
		{"key deleted", func(p *pjFix) { p.env.exec(`DELETE FROM user_secrets WHERE id = $1`, p.key) }, holdProviderBindingStale},
		{"url removed from the allowlist", func(p *pjFix) { p.svc.p.ProviderAllowedBaseURLs = []string{pcOtherURL} }, holdProviderDestinationNotAllowed},
	}
	for _, tc := range revocations {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJFix(t)
			judge := p.createdJudge()
			// An ordinary provider run on the same binding is the control: it still parks.
			ordinary := uuid.New()
			p.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
			    status, status_since, worker_id, started_at, budget_wall_seconds, session_id, harness,
			    model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
			    VALUES ($1, $2, $3, 'issue', 71000, 't', 'd', 'queued', now(), $4, now() - interval '15 minutes', 3600, 'sess-pj', 'claude',
			            $5, 1, 'user_key', $6, $7)`, ordinary, p.userID, p.repoID, p.workerID, p.provider, p.key, p.keyRev())
			tc.revoke(p)
			for i := 0; i < 2; i++ {
				payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
				if err != nil || payload != nil {
					t.Fatalf("Claim %d = (%v, %v), want nothing delivered", i, payload != nil, err)
				}
			}
			p.wantFailedTerminally(judge.ID, tc.reason)
			if r := mustRun(t, p.env, ordinary); r.Status != "paused" || r.HoldReason.String != tc.reason {
				t.Fatalf("ordinary provider run: status=%s hold=%v, want paused/%s (unchanged)", r.Status, r.HoldReason, tc.reason)
			}
		})
	}
}

func TestProviderJudgeQueuedModelUnmappedLiveDB(t *testing.T) {
	p := newPJFix(t)
	judge := p.createdJudge()
	p.env.exec(`UPDATE users SET judge_model = 'claude-not-in-the-map' WHERE id = $1`, p.userID)
	payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want nothing delivered", payload != nil, err)
	}
	p.wantFailedTerminally(judge.ID, holdProviderModelUnmapped)
}

func TestProviderJudgeCodexBoundRefusedLiveDB(t *testing.T) {
	p := newPJFix(t)
	target := p.newTarget()
	id := uuid.New()
	p.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id, harness,
	    model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
	    VALUES ($1, $2, 'judge', 't', 'd', 'claimed', $3, 'codex', $4, 1, 'user_key', $5, $6)`,
		id, p.userID, target.ID, p.provider, p.key, p.keyRev())
	w := p.worker()
	w.ProtocolCapabilities = append(append([]string{}, pjFullCaps...), capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3)
	spy := &keyOpenSpy{Store: p.svc.q, rd: p.svc.q.(providerClaimReader)}
	p.svc.q = spy
	_, err := p.svc.assembleClaim(p.env.ctx, w, mustRun(t, p.env, id))
	if !errors.Is(err, errProviderLaneClaimRefused) {
		t.Fatalf("assembleClaim = %v, want errProviderLaneClaimRefused", err)
	}
	if b, c := spy.bindingReads.Load(), spy.ciphertextReads.Load(); b != 0 || c != 0 {
		t.Fatalf("the refusal reached the key: %d binding read(s), %d ciphertext read(s)", b, c)
	}
}

// TestProviderJudgeWorkerCapabilityBackstopLiveDB: the Go backstop of the SQL claim clause. A worker
// with claude_provider_v1 but not claude_provider_judge_v1 is never handed a provider Claude judge
// (and nothing is opened); the same judge claims on a worker with both (the positive control).
func TestProviderJudgeWorkerCapabilityBackstopLiveDB(t *testing.T) {
	p := newPJFix(t)
	judge := p.createdJudge()
	p.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, judge.ID, p.workerID)
	w := p.worker()

	w.ProtocolCapabilities = []string{capability.ClaudeProviderV1}
	if _, err := p.svc.assembleClaim(p.env.ctx, w, mustRun(t, p.env, judge.ID)); !errors.Is(err, errProviderLaneClaimRefused) {
		t.Fatalf("assembleClaim without judge capability = %v, want errProviderLaneClaimRefused", err)
	}
	w.ProtocolCapabilities = pjFullCaps
	payload, err := p.svc.assembleClaim(p.env.ctx, w, mustRun(t, p.env, judge.ID))
	if err != nil || payload == nil || payload.Secrets.Provider == nil {
		t.Fatalf("assembleClaim with both capabilities = (%+v, %v), want the provider payload", payload, err)
	}
}

// TestProviderJudgeSQLCapabilityGateLiveDB: ClaimRun itself does not hand the judge to a worker
// without claude_provider_judge_v1 (m1's SQL clause), and the judge stays queued.
func TestProviderJudgeSQLCapabilityGateLiveDB(t *testing.T) {
	p := newPJFix(t)
	judge := p.createdJudge()
	p.setCaps([]string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1, capability.ClaudeProviderV1})
	if payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil); err != nil || payload != nil {
		t.Fatalf("Claim by a provider-only worker = (%v, %v), want idle", payload != nil, err)
	}
	if got := mustRun(t, p.env, judge.ID).Status; got != "queued" {
		t.Fatalf("status = %s, want queued", got)
	}
	p.setCaps(pjFullCaps)
	if payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil); err != nil || payload == nil || payload.RunID != judge.ID.String() {
		t.Fatalf("Claim by the judge-capable worker = (%+v, %v)", payload, err)
	}
}

func TestQueuedReasonNoProviderJudgeCapableWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	row := func(runID [16]byte) store.ListActiveRunsForHealthRow {
		return store.ListActiveRunsForHealthRow{ID: runID, UserID: e.userID, Status: "queued", Health: healthOK, Harness: harnessClaude, Kind: runkind.Judge,
			ModelProviderKeyMode: pgtype.Text{String: "user_key", Valid: true}}
	}
	run := e.seedQueuedRun(t, nil, nil)
	w := e.seedWorker(t, []string{capability.ClaudeProviderV1})
	got := svc.queuedReason(e.ctx, time.Now(), row(run), thWithLimit(testCustodyLimit))
	if got != reasonNoProviderJudgeCapableWorker || !strings.Contains(got, capability.ClaudeProviderJudgeV1) {
		t.Fatalf("queuedReason with only claude_provider_v1 workers = %q, want %q", got, reasonNoProviderJudgeCapableWorker)
	}
	e.exec(t, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w, []string{capability.ClaudeProviderV1, capability.ClaudeProviderJudgeV1})
	if got := svc.queuedReason(e.ctx, time.Now(), row(run), thWithLimit(testCustodyLimit)); got == reasonNoProviderJudgeCapableWorker || got == reasonNoProviderCapableWorker {
		t.Fatalf("queuedReason with a judge-capable worker = %q, want no provider capability reason", got)
	}
}

// pjRunning claims the provider judge and starts it, returning the claim generation.
func pjRunning(t *testing.T, p *pjFix, judge store.Run) int64 {
	t.Helper()
	payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
	if err != nil || payload == nil || payload.RunID != judge.ID.String() {
		t.Fatalf("Claim = (%+v, %v)", payload, err)
	}
	gen := payload.ClaimGeneration
	if _, applied, err := p.svc.SetState(p.env.ctx, p.worker(), judge.ID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("start: applied=%v err=%v", applied, err)
	}
	return gen
}

// TestProviderJudgeGatewayLimitFailsTerminallyLiveDB: a provider judge's limit report (whatever type or
// reset the worker sent) fails it terminally with the gateway wording and no reset; it never parks. Controls:
// without limit facts the plain failed path; a vendor judge's report keeps the vendor wording.
func TestProviderJudgeGatewayLimitFailsTerminallyLiveDB(t *testing.T) {
	t.Run("provider judge with a limit type", func(t *testing.T) {
		p := newPJFix(t)
		judge := p.createdJudge()
		gen := pjRunning(t, p, judge)
		reset := time.Now().Add(5 * time.Hour).UnixMilli()
		r, applied, err := p.svc.SetState(p.env.ctx, p.worker(), judge.ID, StateRequest{State: "failed", ClaimGeneration: &gen,
			FailureReason: strPtr("worker text"), RateLimitType: strPtr("five_hour"), LimitResetsAt: &reset})
		if err != nil || !applied || r.Status != "failed" {
			t.Fatalf("failed report: status=%s applied=%v err=%v", r.Status, applied, err)
		}
		got := mustRun(t, p.env, judge.ID)
		if got.FailureReason.String != providerJudgeGatewayLimitReason || strings.Contains(got.FailureReason.String, "Anthropic") ||
			got.LimitResetsAt.Valid || got.RetryNotBefore.Valid {
			t.Fatalf("reason=%q resets=%v retry=%v, want the gateway wording and no reset", got.FailureReason.String, got.LimitResetsAt, got.RetryNotBefore)
		}
	})
	t.Run("provider judge without limit facts", func(t *testing.T) {
		p := newPJFix(t)
		judge := p.createdJudge()
		gen := pjRunning(t, p, judge)
		if _, applied, err := p.svc.SetState(p.env.ctx, p.worker(), judge.ID, StateRequest{State: "failed", ClaimGeneration: &gen, FailureReason: strPtr("plain agent failure")}); err != nil || !applied {
			t.Fatalf("failed report: applied=%v err=%v", applied, err)
		}
		got := mustRun(t, p.env, judge.ID)
		if got.Status != "failed" || got.FailureReason.String != "plain agent failure" {
			t.Fatalf("status=%s reason=%q, want the plain failed path", got.Status, got.FailureReason.String)
		}
	})
	t.Run("ordinary provider run keeps the provider run wording", func(t *testing.T) {
		pf := newPCFix(t)
		runID := pf.run(t)
		pf.env.exec(`UPDATE runs SET wait_on_limit = false WHERE id = $1`, runID)
		w := pf.worker(t)
		payload, err := pf.svc.Claim(pf.env.ctx, w, nil)
		if err != nil || payload == nil {
			t.Fatalf("Claim = (%v, %v)", payload != nil, err)
		}
		gen := payload.ClaimGeneration
		if _, applied, err := pf.svc.SetState(pf.env.ctx, w, runID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
			t.Fatalf("start: applied=%v err=%v", applied, err)
		}
		if _, applied, err := pf.svc.SetState(pf.env.ctx, w, runID, StateRequest{State: "failed", ClaimGeneration: &gen, RateLimitType: strPtr(RateLimitTypeProviderGatewayLimit)}); err != nil || !applied {
			t.Fatalf("failed report: applied=%v err=%v", applied, err)
		}
		if got := mustRun(t, pf.env, runID).FailureReason.String; got != providerGatewayLimitReason {
			t.Fatalf("ordinary provider run reason = %q, want %q", got, providerGatewayLimitReason)
		}
	})
	t.Run("vendor judge keeps the vendor wording", func(t *testing.T) {
		fx := newCDFix(t)
		judge := fx.judgeRun(t)
		fx.env.exec(`UPDATE runs SET status = 'running', worker_id = $2, claim_generation = 1, started_at = now() WHERE id = $1`, judge, fx.workerID)
		gen := int64(1)
		if _, applied, err := fx.svc.SetState(fx.env.ctx, fx.worker(t), judge, StateRequest{State: "failed", ClaimGeneration: &gen, RateLimitType: strPtr("five_hour")}); err != nil || !applied {
			t.Fatalf("failed report: applied=%v err=%v", applied, err)
		}
		if got := mustRun(t, fx.env, judge).FailureReason.String; got != "Anthropic usage limit (five_hour) reached" {
			t.Fatalf("vendor judge reason = %q, want the unchanged vendor wording", got)
		}
	})
}
