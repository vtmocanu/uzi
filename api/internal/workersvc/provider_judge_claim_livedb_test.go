package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_judge_claim_livedb_test.go is PRD #4322 M1a milestone m3 against a real Postgres: a
// provider-bound Claude judge (created by the m2 policy) claims against its frozen binding, a
// revoked or unmappable binding settles it terminally (never a park), the health reason names the
// missing judge capability, and a limit report from a provider judge never parks or waits. PRD #4574
// M1 adds the Codex twin: a Codex-bound judge claims only on a worker with codex_provider_judge_v1. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
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
	got := p.wantFailedTerminally(judge.ID, holdProviderModelUnmapped)
	// The token appears once: the refusal's Detail is bare text, not the typed error's own rendering.
	if n := strings.Count(got.FailureReason.String, holdProviderModelUnmapped); n != 1 {
		t.Fatalf("failure_reason %q carries %s %d times, want once", got.FailureReason.String, holdProviderModelUnmapped, n)
	}
}

// TestProviderJudgeRotationBetweenAssemblyAndFinishLiveDB: the judge twin of
// TestProviderClaimRotationBetweenAssemblyAndFinishLiveDB. The binding is intact at assembly (the
// hook proves assembly delivered a payload) and revoked only afterwards, so the finisher's locked
// re-check is the sole thing that can catch it: nothing is delivered and the judge fails terminally
// (credential_unavailable), never parks, and records no vendor credential or epoch.
func TestProviderJudgeRotationBetweenAssemblyAndFinishLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		revoke func(p *pjFix)
		reason string
	}{
		{"key rotated", func(p *pjFix) {
			p.env.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
		}, holdProviderBindingStale},
		{"key disabled", func(p *pjFix) {
			p.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
		}, holdProviderBindingStale},
		{"key deleted", func(p *pjFix) { p.env.exec(`DELETE FROM user_secrets WHERE id = $1`, p.key) }, holdProviderBindingStale},
		{"url removed from the allowlist", func(p *pjFix) { p.svc.p.ProviderAllowedBaseURLs = []string{pcOtherURL} }, holdProviderDestinationNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJFix(t)
			judge := p.createdJudge()
			assembled := false
			p.svc.claimHooks = &claimTestHooks{
				afterAssembly: func(_ context.Context, run store.Run, payload *ClaimPayload, err error) {
					if run.ID == judge.ID && err == nil && payload != nil && payload.Secrets.Provider != nil {
						assembled = true
					}
					tc.revoke(p)
				},
			}
			payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want nothing delivered", payload != nil, err)
			}
			if !assembled {
				t.Fatal("assembly did not succeed before the revocation, so the locked re-check was not what refused")
			}
			p.wantFailedTerminally(judge.ID, tc.reason)
		})
	}
}

// TestProviderJudgeSharedDefinitionClaimLiveDB: PRD #4322 "a shared edit or retirement preserves a
// valid frozen judge". A recipient's queued provider judge on an admin-shared per-user-key definition
// survives the admin editing the definition (new revision, remapped models) and then retiring it:
// providerBindingRefusal treats a retired provider as intact (retiring stops new selections only), so
// the claim delivers the ORIGINAL revision's profile and the RECIPIENT's own key, no vendor token,
// and the judge is not failed. Control: a fresh judge after the retirement is refused.
func TestProviderJudgeSharedDefinitionClaimLiveDB(t *testing.T) {
	p := newPJFix(t)
	plainB := "gw-key-B-" + uuid.NewString()
	p.useShared(plainB) // the recipient (the fixture owner): shared definition selected with their own key
	// Another recipient and the admin each hold their own sealed key on the same definition; neither
	// may ever be the key a claim for the fixture owner's judge opens.
	other := p.env.seedUser(t)
	seedSealedKey(t, p.env, other, p.provider, "gw-key-OTHER-"+uuid.NewString())
	admin := p.env.seedUser(t)
	seedSealedKey(t, p.env, admin, p.provider, "gw-key-ADMIN-"+uuid.NewString())

	judge := p.createdJudge() // binds the shared revision 1 and the recipient's key
	if uuid.UUID(mustRun(t, p.env, judge.ID).ModelProviderKeySecretID.Bytes) != p.key {
		t.Fatalf("judge froze key %v, want the recipient's %s", mustRun(t, p.env, judge.ID).ModelProviderKeySecretID, p.key)
	}

	// While the judge is queued the admin edits the shared definition (revision 2, still mapping the
	// judge model, but to different upstream models) and then retires it.
	rev, err := p.env.q.BumpSharedModelProviderRevision(p.env.ctx, p.provider)
	if err != nil || rev != 2 {
		t.Fatalf("bump shared = (%d, %v), want revision 2", rev, err)
	}
	if err := p.env.q.InsertModelProviderRevision(p.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.provider, Revision: rev, Protocol: "anthropic_messages", BaseUrl: pvBase, AuthStyle: "bearer",
		ModelMap: []byte(`{"opus":{"model":"edited-opus"},"sonnet":{"model":"edited-sonnet"},"haiku":{"model":"edited-haiku"}}`),
	}); err != nil {
		t.Fatalf("insert edited revision: %v", err)
	}
	if n, err := p.env.q.RetireSharedModelProvider(p.env.ctx, p.provider); err != nil || n != 1 {
		t.Fatalf("retire shared = (%d, %v)", n, err)
	}

	// Control: a fresh judge after the retirement is refused.
	fresh := p.newTarget()
	p.auto(fresh)
	p.wantNoJudge(fresh, "a retired definition")
	wantSkip(t, p.env, fresh.ID, "claude", JudgeSkipProviderSourceUnusable)

	payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
	if err != nil || payload == nil || payload.RunID != judge.ID.String() {
		t.Fatalf("Claim = (%+v, %v), want the frozen provider judge", payload, err)
	}
	b := payload.Secrets.Provider
	if b == nil || payload.Secrets.AnthropicOAuthToken != "" || payload.Secrets.Codex != nil {
		t.Fatalf("secrets = %+v, want only a provider binding", payload.Secrets)
	}
	if b.ProviderID != p.provider.String() || b.Revision != 1 || b.BaseURL != pvBase || b.Credential != plainB ||
		b.ModelMap["opus"].Model != "corp-opus" || b.ModelMap["opus"].Model == "edited-opus" {
		t.Fatalf("binding = %+v, want the ORIGINAL revision 1 profile and the recipient's own key", b)
	}
	if payload.JudgeModel == nil || !modelMapped(b.ModelMap, *payload.JudgeModel) {
		t.Fatalf("judge model = %v, want one mapped by the frozen revision", payload.JudgeModel)
	}
	if got := mustRun(t, p.env, judge.ID); got.Status != "claimed" || got.FailOrigin.Valid {
		t.Fatalf("judge status=%s origin=%v, want claimed and not failed", got.Status, got.FailOrigin)
	}
	p.wantNoVendor(judge)
}

// TestProviderJudgeStaleFinisherLiveDB: the exact-generation fence on a judge provider refusal. The
// key is revoked in the assembly-to-finish window (so an un-superseded finisher fails the judge
// terminally, see TestProviderJudgeRotationBetweenAssemblyAndFinishLiveDB), but the claim was
// meanwhile superseded by a newer generation. The
// stale finisher delivers nothing and settles nothing: the newer claim's row is untouched.
func TestProviderJudgeStaleFinisherLiveDB(t *testing.T) {
	cases := []struct{ name, set string }{
		{"newer generation, still claimed", "claim_generation = claim_generation + 1"},
		{"newer generation, already running", "claim_generation = claim_generation + 1, status = 'running', started_at = now()"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPJFix(t)
			judge := p.createdJudge()
			assembled := false
			var newer store.Run
			p.svc.claimHooks = &claimTestHooks{
				afterAssembly: func(_ context.Context, run store.Run, payload *ClaimPayload, err error) {
					if run.ID == judge.ID && err == nil && payload != nil && payload.Secrets.Provider != nil {
						assembled = true
					}
					// Revoke the key AND let a newer claim supersede this one.
					p.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, p.key)
					p.env.exec(`UPDATE runs SET `+tc.set+` WHERE id = $1`, judge.ID)
					newer = mustRun(t, p.env, judge.ID)
				},
			}
			payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want idle: the stale finisher delivers nothing and does not error", payload != nil, err)
			}
			if !assembled {
				t.Fatal("assembly did not succeed before the revocation, so the fence was not what refused")
			}
			got := mustRun(t, p.env, judge.ID)
			if got.Status != newer.Status || got.WorkerID != newer.WorkerID || got.ClaimGeneration != newer.ClaimGeneration ||
				got.Status == "failed" || got.FailOrigin.Valid || got.FailureReason.Valid || !got.UpdatedAt.Time.Equal(newer.UpdatedAt.Time) {
				t.Fatalf("the newer claim's row was touched: got status=%s worker=%v gen=%d origin=%v reason=%v; want status=%s worker=%v gen=%d untouched",
					got.Status, got.WorkerID, got.ClaimGeneration, got.FailOrigin, got.FailureReason, newer.Status, newer.WorkerID, newer.ClaimGeneration)
			}
			p.wantNoVendor(got)
		})
	}
}

// pjCodexBaseCaps is the Codex provider claim set WITHOUT codex_provider_judge_v1: what a worker that
// serves ordinary Codex provider runs advertises before PRD #4574 M1.
var pjCodexBaseCaps = []string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1, capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}

func pjCodexJudgeCaps() []string {
	return append(append([]string{}, pjCodexBaseCaps...), capability.CodexProviderJudgeV1)
}

// pjcFix is a pjFix whose owner ALSO has a real, sealed, usable Codex source selected and judges on
// Codex, and whose worker advertises the full Codex judge capability set. The owner holds no vendor
// Codex credential, so a vendor fallback could not succeed either.
type pjcFix struct {
	*pjFix
	cProvider uuid.UUID
	cKey      uuid.UUID
	cPlain    string
}

func newPJCFix(t *testing.T) *pjcFix {
	t.Helper()
	p := newPJFix(t)
	c := &pjcFix{pjFix: p, cPlain: "codex-gw-key-" + uuid.NewString()}
	prov, err := p.env.q.InsertModelProvider(p.env.ctx, store.InsertModelProviderParams{OwnerUserID: p.userID, Name: "pjc-" + uuid.NewString()[:8], Engine: harnessCodex})
	if err != nil {
		t.Fatalf("insert codex provider: %v", err)
	}
	if err := p.env.q.InsertModelProviderRevision(p.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: prov.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"gateway"}}`),
	}); err != nil {
		t.Fatalf("insert codex revision: %v", err)
	}
	sealed, err := p.env.box.Seal([]byte(c.cPlain))
	if err != nil {
		t.Fatal(err)
	}
	key, err := p.env.q.InsertProviderKeySecret(p.env.ctx, store.InsertProviderKeySecretParams{
		UserID: p.userID, Label: prov.ID.String(), Ciphertext: sealed, SealedWith: "master", ProviderID: pgtype.UUID{Bytes: prov.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("insert codex key: %v", err)
	}
	c.cProvider, c.cKey = prov.ID, key.ID
	p.env.exec(`UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = (SELECT scope_owner_id FROM model_providers WHERE id = $2), judge_runs_on = 'codex' WHERE id = $1`, p.userID, c.cProvider)
	t.Cleanup(func() {
		_, _ = p.env.pool.Exec(context.Background(), `UPDATE users SET codex_provider_id = NULL, codex_provider_scope_id = NULL WHERE id = $1`, p.userID)
	})
	p.setCaps(pjCodexJudgeCaps())
	return c
}

// createdCodexJudge creates the Codex provider judge for a fresh target and checks its binding.
func (c *pjcFix) createdCodexJudge() store.Run {
	c.t.Helper()
	target := c.newTarget()
	c.auto(target)
	r := c.mustJudge(target)
	if r.Harness != "codex" || !r.ModelProviderID.Valid || uuid.UUID(r.ModelProviderID.Bytes) != c.cProvider ||
		!r.ModelProviderKeySecretID.Valid || uuid.UUID(r.ModelProviderKeySecretID.Bytes) != c.cKey ||
		r.CodexSecretID.Valid || r.CodexAuthMode.Valid || r.AnthropicSecretID.Valid {
		c.t.Fatalf("judge = harness %q binding %+v codex=%v auth=%v anthropic=%v, want a Codex judge bound to %s", r.Harness, bindingOfRun(r), r.CodexSecretID, r.CodexAuthMode, r.AnthropicSecretID, c.cProvider)
	}
	return r
}

// TestProviderJudgeCodexBoundRefusedLiveDB: the preservation control for a worker that lacks
// codex_provider_judge_v1. A real Codex provider judge is refused by the Go backstop with no key read
// and never claimed by the SQL clause, even though the worker holds the full Codex provider set.
func TestProviderJudgeCodexBoundRefusedLiveDB(t *testing.T) {
	c := newPJCFix(t)
	judge := c.createdCodexJudge()

	c.setCaps(pjCodexBaseCaps)
	if payload, err := c.svc.Claim(c.env.ctx, c.worker(), nil); err != nil || payload != nil {
		t.Fatalf("Claim by a worker without codex_provider_judge_v1 = (%v, %v), want idle", payload != nil, err)
	}
	if got := mustRun(t, c.env, judge.ID).Status; got != "queued" {
		t.Fatalf("status = %s, want queued", got)
	}

	c.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, judge.ID, c.workerID)
	w := c.worker()
	spy := &keyOpenSpy{Store: c.svc.q, rd: c.svc.q.(providerClaimReader)}
	orig := c.svc.q
	c.svc.q = spy
	_, err := c.svc.assembleClaim(c.env.ctx, w, mustRun(t, c.env, judge.ID))
	c.svc.q = orig
	if !errors.Is(err, errProviderLaneClaimRefused) {
		t.Fatalf("assembleClaim without the judge capability = %v, want errProviderLaneClaimRefused", err)
	}
	if b, k := spy.bindingReads.Load(), spy.ciphertextReads.Load(); b != 0 || k != 0 {
		t.Fatalf("the refusal reached the key: %d binding read(s), %d ciphertext read(s)", b, k)
	}
}

// TestCodexProviderJudgeClaimLiveDB: a worker advertising the Codex trio, codex_provider_v1 and
// codex_provider_judge_v1 claims the Codex provider judge. The payload carries only the provider
// binding (engine codex, the sealed key opened), no Codex capability and no Anthropic token; the
// run row gains no vendor Codex authority and no credential epoch; the judge model is the Codex
// judge default.
func TestCodexProviderJudgeClaimLiveDB(t *testing.T) {
	c := newPJCFix(t)
	judge := c.createdCodexJudge()
	payload, err := c.svc.Claim(c.env.ctx, c.worker(), nil)
	if err != nil || payload == nil || payload.RunID != judge.ID.String() {
		t.Fatalf("Claim = (%+v, %v), want the Codex provider judge", payload, err)
	}
	b := payload.Secrets.Provider
	if b == nil || payload.Secrets.Codex != nil || payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("secrets = %+v, want only a provider binding", payload.Secrets)
	}
	if b.ProviderID != c.cProvider.String() || b.Engine != "codex" || b.Protocol != "openai_responses" || b.Revision != 1 || b.BaseURL != pvBase ||
		b.Credential != c.cPlain || b.ModelMap["default"].Model != "gateway" {
		t.Fatalf("binding = %+v", b)
	}
	if payload.JudgeModel == nil || *payload.JudgeModel != agenttmpl.DefaultCodexJudgeModel {
		t.Fatalf("judge model = %v, want the Codex judge default %q", payload.JudgeModel, agenttmpl.DefaultCodexJudgeModel)
	}
	if payload.TargetRunID == nil {
		t.Fatal("the judge claim lost its target")
	}
	got := mustRun(t, c.env, judge.ID)
	if got.Status != "claimed" {
		t.Fatalf("status = %s, want claimed", got.Status)
	}
	if got.CodexSecretID.Valid || got.CodexAuthMode.Valid || got.CodexMaterialRevision.Valid || len(got.CodexCapHash) != 0 || got.AnthropicSecretID.Valid {
		t.Fatalf("vendor authority recorded: codex=%v auth=%v material=%v cap-hash=%d anthropic=%v",
			got.CodexSecretID, got.CodexAuthMode, got.CodexMaterialRevision, len(got.CodexCapHash), got.AnthropicSecretID)
	}
	var epochs int
	if err := c.env.pool.QueryRow(c.env.ctx, `SELECT count(*) FROM run_credential_epochs WHERE run_id = $1`, judge.ID).Scan(&epochs); err != nil || epochs != 0 {
		t.Fatalf("run_credential_epochs rows = %d (err %v), want 0: no vendor or Codex capability epoch for a provider judge", epochs, err)
	}
}

// TestCodexProviderJudgeRotationBetweenAssemblyAndFinishLiveDB: the Codex twin of
// TestProviderJudgeRotationBetweenAssemblyAndFinishLiveDB. The binding is intact at assembly and
// revoked only afterwards, so the finisher's locked re-check is what refuses: nothing is delivered
// and the judge fails terminally (credential_unavailable), never parks.
func TestCodexProviderJudgeRotationBetweenAssemblyAndFinishLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		revoke func(c *pjcFix)
	}{
		{"key rotated", func(c *pjcFix) {
			c.env.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, c.cKey)
		}},
		{"key disabled", func(c *pjcFix) {
			c.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, c.cKey)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newPJCFix(t)
			judge := c.createdCodexJudge()
			assembled := false
			c.svc.claimHooks = &claimTestHooks{
				afterAssembly: func(_ context.Context, run store.Run, payload *ClaimPayload, err error) {
					if run.ID == judge.ID && err == nil && payload != nil && payload.Secrets.Provider != nil {
						assembled = true
					}
					tc.revoke(c)
				},
			}
			payload, err := c.svc.Claim(c.env.ctx, c.worker(), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want nothing delivered", payload != nil, err)
			}
			if !assembled {
				t.Fatal("assembly did not succeed before the revocation, so the locked re-check was not what refused")
			}
			c.wantFailedTerminally(judge.ID, holdProviderBindingStale)
		})
	}
}

// TestProviderJudgeWorkerCapabilityBackstopLiveDB: the Go backstop of the SQL claim clause. A worker
// holding every other capability but not claude_provider_judge_v1 is never handed a provider Claude
// judge and opens nothing; the same judge claims on a worker with all of them (the positive control).
func TestProviderJudgeWorkerCapabilityBackstopLiveDB(t *testing.T) {
	p := newPJFix(t)
	judge := p.createdJudge()
	p.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, judge.ID, p.workerID)
	w := p.worker()

	// Only the judge capability is missing, so the refusal is pinned to it and not to another flag.
	w.ProtocolCapabilities = []string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1, capability.ClaudeProviderV1}
	spy := &keyOpenSpy{Store: p.svc.q, rd: p.svc.q.(providerClaimReader)}
	orig := p.svc.q
	p.svc.q = spy
	if _, err := p.svc.assembleClaim(p.env.ctx, w, mustRun(t, p.env, judge.ID)); !errors.Is(err, errProviderLaneClaimRefused) {
		t.Fatalf("assembleClaim without judge capability = %v, want errProviderLaneClaimRefused", err)
	}
	if b, c := spy.bindingReads.Load(), spy.ciphertextReads.Load(); b != 0 || c != 0 {
		t.Fatalf("the refusal reached the key: %d binding read(s), %d ciphertext read(s)", b, c)
	}
	p.svc.q = orig
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

// TestQueuedReasonNoCodexProviderJudgeCapableWorkerLiveDB: a provider-bound Codex judge whose owner
// has only workers without codex_provider_judge_v1 is told so, and the reason clears once a capable
// worker is online.
func TestQueuedReasonNoCodexProviderJudgeCapableWorkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.permitService(t)
	row := func(runID [16]byte) store.ListActiveRunsForHealthRow {
		return store.ListActiveRunsForHealthRow{ID: runID, UserID: e.userID, Status: "queued", Health: healthOK, Harness: harnessCodex, Kind: runkind.Judge,
			ModelProviderKeyMode: pgtype.Text{String: "user_key", Valid: true}}
	}
	run := e.seedQueuedRun(t, nil, nil)
	w := e.seedWorker(t, pjCodexBaseCaps)
	got := svc.queuedReason(e.ctx, time.Now(), row(run), thWithLimit(testCustodyLimit))
	if got != reasonNoCodexProviderJudgeCapableWorker || !strings.Contains(got, capability.CodexProviderJudgeV1) {
		t.Fatalf("queuedReason with only codex_provider_v1 workers = %q, want %q", got, reasonNoCodexProviderJudgeCapableWorker)
	}
	e.exec(t, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, w, pjCodexJudgeCaps())
	if got := svc.queuedReason(e.ctx, time.Now(), row(run), thWithLimit(testCustodyLimit)); got == reasonNoCodexProviderJudgeCapableWorker || got == reasonNoProviderCapableWorker {
		t.Fatalf("queuedReason with a Codex judge-capable worker = %q, want no provider capability reason", got)
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

// TestProviderJudgeClaimSignalScrubsKeyBeforeClipLiveDB: the claim-produced judge_signal of a
// provider judge carries no key material. The target's trace reports a missing command whose token
// ends in the gateway key, long enough that the pre-scan's evidence clip would cut the key, plus a
// plain jq miss as the control. The server scrubs the whole row first, so the key-bearing candidate
// never forms and only jq reaches the worker (PRD #4322 M1a landing review).
func TestProviderJudgeClaimSignalScrubsKeyBeforeClipLiveDB(t *testing.T) {
	p := newPJFix(t)
	target := p.newTarget()
	token := strings.Repeat("a", 170) + p.plain
	p.env.exec(`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 900, 'tool_result', $2)`,
		target.ID, `{"text":"bash: jq: command not found\nbash: `+token+`: command not found"}`)
	p.auto(target)
	judge := p.mustJudge(target)
	p.wantBoundTo(judge, p.provider, p.key, 1)

	payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
	if err != nil || payload == nil || payload.RunID != judge.ID.String() {
		t.Fatalf("Claim = (%+v, %v), want the provider judge", payload, err)
	}
	if payload.JudgeSignal == nil || len(payload.JudgeSignal.MissingTools) != 1 || payload.JudgeSignal.MissingTools[0].Command != "jq" {
		t.Fatalf("judge_signal = %+v, want only the jq control candidate", payload.JudgeSignal)
	}
	if m := payload.JudgeSignal.MissingTools[0]; strings.Contains(m.Command+m.Evidence, p.plain[:12]) {
		t.Fatalf("a key prefix reached the claim: %+v", m)
	}
}
