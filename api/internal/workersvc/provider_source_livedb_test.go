package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_source_livedb_test.go is the PRD #4320 M4 ("Selection") contract against a real
// Postgres: the owner's Claude provider source, its precedence over the vendor ladder, the
// create-time freeze, the inherited successor binding and the refusals. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh. Every test
// name ends LiveDB so the store-IT sweep selects it.

const pvBase = "https://gw.example.com/anthropic"

const pvDefaultMap = `{"opus":{"model":"corp-opus"},"sonnet":{"model":"corp-sonnet"},"haiku":{"model":"corp-haiku"}}`

type provFix struct {
	*effFix
	provider uuid.UUID
	key      uuid.UUID
}

func newProvFix(t *testing.T) *provFix {
	t.Helper()
	fx := newEffFix(t)
	fx.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	// Registered after the fixture's own cleanup, so it runs first: a selected provider cannot be
	// deleted (RESTRICT), and the user delete cascades to its providers.
	t.Cleanup(func() {
		if _, err := fx.env.pool.Exec(context.Background(), `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID); err != nil {
			t.Errorf("clear the Claude source in cleanup: %v", err)
		}
	})
	return &provFix{effFix: fx}
}

// seedProviderFor seeds a provider (revision 1 at pvBase) with an enabled key for userID.
func seedProviderFor(t *testing.T, env codexTestEnv, userID uuid.UUID, modelMap string) (provider, key uuid.UUID) {
	t.Helper()
	p, err := env.q.InsertModelProvider(env.ctx, store.InsertModelProviderParams{OwnerUserID: userID, Name: "p-" + uuid.NewString()[:8], Engine: "claude"})
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if err := env.q.InsertModelProviderRevision(env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "anthropic_messages", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(modelMap),
	}); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	k, err := env.q.InsertProviderKeySecret(env.ctx, store.InsertProviderKeySecretParams{
		UserID: userID, Label: p.ID.String(), Ciphertext: []byte("ct"), SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("insert key: %v", err)
	}
	return p.ID, k.ID
}

// addProvider seeds a provider and its enabled key and, when selectIt, selects it as the source.
func (pf *provFix) addProvider(modelMap string, selectIt bool) {
	pf.t.Helper()
	pf.provider, pf.key = seedProviderFor(pf.t, pf.env, pf.userID, modelMap)
	if selectIt {
		pf.selectSource()
	}
}

func (pf *provFix) selectSource() {
	pf.t.Helper()
	pf.env.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, pf.userID, pf.provider)
}

func (pf *provFix) keyRev() int64 {
	pf.t.Helper()
	var rev int64
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT enablement_rev FROM user_secrets WHERE id = $1`, pf.key).Scan(&rev); err != nil {
		pf.t.Fatalf("read key rev: %v", err)
	}
	return rev
}

// editRevision is what an endpoint edit commits: the next revision at url.
func (pf *provFix) editRevision(url string) int32 {
	pf.t.Helper()
	rev, err := pf.env.q.BumpModelProviderRevision(pf.env.ctx, store.BumpModelProviderRevisionParams{ID: pf.provider, OwnerUserID: pf.userID})
	if err != nil {
		pf.t.Fatalf("bump: %v", err)
	}
	if err := pf.env.q.InsertModelProviderRevision(pf.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: pf.provider, Revision: rev, Protocol: "anthropic_messages", BaseUrl: url, AuthStyle: "bearer", ModelMap: []byte(pvDefaultMap),
	}); err != nil {
		pf.t.Fatalf("insert revision: %v", err)
	}
	return rev
}

func (pf *provFix) runCount() int { return pf.env.countRunsForUser(pf.t, pf.userID) }

// wantBound asserts the five binding columns, the Claude harness and an empty vendor credential.
func (pf *provFix) wantBound(run store.Run, revision int32, keyRev int64) {
	pf.t.Helper()
	if !run.ModelProviderID.Valid || uuid.UUID(run.ModelProviderID.Bytes) != pf.provider ||
		run.ModelProviderRevision.Int32 != revision || !run.ModelProviderRevision.Valid ||
		run.ModelProviderKeyMode.String != "user_key" || !run.ModelProviderKeyMode.Valid ||
		!run.ModelProviderKeySecretID.Valid || uuid.UUID(run.ModelProviderKeySecretID.Bytes) != pf.key ||
		!run.ModelProviderKeyRev.Valid || run.ModelProviderKeyRev.Int64 != keyRev {
		pf.t.Fatalf("binding = %+v, want provider %s rev %d user_key key %s keyRev %d", bindingOfRun(run), pf.provider, revision, pf.key, keyRev)
	}
	if run.Harness != "claude" || run.AnthropicSecretID.Valid || run.CodexSecretID.Valid || run.CredentialOverrideSecretID.Valid {
		pf.t.Fatalf("a provider run must be claude with no vendor credential: harness=%q anthropic=%v codex=%v override=%v",
			run.Harness, run.AnthropicSecretID, run.CodexSecretID, run.CredentialOverrideSecretID)
	}
}

func (pf *provFix) wantVendor(run store.Run) {
	pf.t.Helper()
	if b := bindingOfRun(run); b != (bindingParams{}) {
		pf.t.Fatalf("a vendor run carries a binding: %+v", b)
	}
}

// insertAt is a CreateRun closure on a fresh iid that stamps the binding from the resolution.
func (pf *provFix) insertAt(model *string) func(*store.Queries, resolvedHarness) (store.Run, error) {
	pf.iid++
	iid := pf.iid
	return func(q *store.Queries, r resolvedHarness) (store.Run, error) {
		b := bindingOf(r)
		p := store.CreateRunParams{
			UserID: pf.userID, RepoID: pf.repoID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
			IssueTitle: "provider", IssueDescription: "d", PlanSource: planSourceAgent, TriggerSource: "manual",
			Harness:         string(r.Harness),
			ModelProviderID: b.ProviderID, ModelProviderRevision: b.Revision, ModelProviderKeyMode: b.KeyMode,
			ModelProviderKeySecretID: b.KeySecret, ModelProviderKeyRev: b.KeyRev,
		}
		if model != nil {
			p.Model = pgtype.Text{String: *model, Valid: true}
		}
		return q.CreateRun(pf.env.ctx, p)
	}
}

func isUnusable(err error, reason string) bool {
	var u *ProviderSourceUnusableError
	return errors.Is(err, ErrProviderSourceUnusable) && errors.As(err, &u) && u.Reason == reason
}

// ---------------------------------------------------------------------------------------------
// Null source: byte-identical to the vendor ladder.
// ---------------------------------------------------------------------------------------------

// TestProviderNullSourceKeepsTheVendorLadderLiveDB is the golden table for a user with NO provider
// source: each row's resolvedHarness fields and the created row's binding columns are the literals
// written here, the values the resolver produced before PRD #4320. A change to the null path
// reddens a row.
func TestProviderNullSourceKeepsTheVendorLadderLiveDB(t *testing.T) {
	type want struct {
		harness    Harness
		rule       harnessRule
		unset      bool
		both       bool
		skipped    bool
		explicit   *Harness
		wantErr    error
		codexAlias bool
	}
	cases := []struct {
		name    string
		token   bool
		codex   bool
		def     string
		want    want
		explain string
	}{
		{name: "neither credential", want: want{wantErr: errNoUsableCredential}},
		{name: "token only", token: true, want: want{harness: HarnessClaude, rule: harnessRuleSoleUsable, unset: true}},
		{name: "codex only", codex: true, want: want{harness: HarnessCodex, rule: harnessRuleSoleUsable, unset: true, codexAlias: true}},
		{name: "both, default unset", token: true, codex: true, want: want{harness: HarnessClaude, rule: harnessRuleBothClaude, unset: true, both: true}},
		{name: "both, default codex", token: true, codex: true, def: "codex", want: want{harness: HarnessCodex, rule: harnessRuleUserDefault, both: true, codexAlias: true}},
		{name: "default codex but only a token", token: true, def: "codex", want: want{harness: HarnessClaude, rule: harnessRuleSoleUsable, skipped: true}},
		{name: "explicit claude without a token", codex: true, want: want{explicit: h(HarnessClaude), wantErr: errNoCredentialForHarness}},
		{name: "explicit codex with both", token: true, codex: true, want: want{explicit: h(HarnessCodex), harness: HarnessCodex, rule: harnessRuleExplicit, unset: true, both: true, codexAlias: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newProvFix(t)
			if c.token {
				fx.anthropic()
			}
			if c.codex {
				fx.codexDefault()
			}
			fx.lane("default_harness", c.def)
			res, err := fx.svc.resolveRunHarness(fx.env.ctx, fx.userID, c.want.explicit)
			if c.want.wantErr != nil {
				if !errors.Is(err, c.want.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.want.wantErr)
				}
				if fx.runCount() != 0 {
					t.Fatal("a refusal left a run")
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if res.Harness != c.want.harness || res.Rule != c.want.rule || res.DefaultUnset != c.want.unset ||
				res.BothUsable != c.want.both || res.DefaultSkipped != c.want.skipped ||
				res.ClaudeSourceSet || res.ProviderSource != nil || (res.Codex != nil) != c.want.codexAlias {
				t.Fatalf("resolved = %+v, want harness %s rule %s unset=%v both=%v skipped=%v codex=%v and no provider source",
					res, c.want.harness, c.want.rule, c.want.unset, c.want.both, c.want.skipped, c.want.codexAlias)
			}
			run, err := fx.create(c.want.explicit, nil)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			got := mustRun(t, fx.env, run.ID)
			fx.wantVendor(got)
			if got.Harness != string(c.want.harness) {
				t.Fatalf("created harness = %q, want %q", got.Harness, c.want.harness)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------
// Set and usable: the freeze.
// ---------------------------------------------------------------------------------------------

func TestProviderSourceUsableFreezesBindingLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic() // an enabled Anthropic token must not matter
	fx.addProvider(pvDefaultMap, true)
	fx.env.exec(`UPDATE user_secrets SET enablement_rev = 7 WHERE id = $1`, fx.key)

	res, err := fx.svc.resolveRunHarness(fx.env.ctx, fx.userID, nil)
	if err != nil || res.Harness != HarnessClaude || res.ProviderSource == nil || !res.ClaudeSourceSet || res.ProviderSource.KeyRev != 7 {
		t.Fatalf("resolved = %+v err=%v, want claude with the usable source (key rev 7)", res, err)
	}
	run, err := fx.create(nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := mustRun(t, fx.env, run.ID)
	fx.wantBound(got, 1, 7) // key_rev is the key's enablement_rev, not its secret id or material revision
	// Explicit Claude uses the source too, and still never touches the Anthropic token.
	run2, err := fx.create(h(HarnessClaude), nil)
	if err != nil {
		t.Fatalf("explicit claude: %v", err)
	}
	fx.wantBound(mustRun(t, fx.env, run2.ID), 1, 7)
}

// TestProviderSourceCoexistsWithCodexLiveDB: an explicit Codex pin, and a Codex default, keep
// working for a provider-sourced owner (the Claude source is irrelevant to Codex).
func TestProviderSourceCoexistsWithCodexLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.codexDefault()
	fx.addProvider(pvDefaultMap, true)
	run, err := fx.create(h(HarnessCodex), nil)
	if err != nil {
		t.Fatalf("explicit codex: %v", err)
	}
	got := mustRun(t, fx.env, run.ID)
	if got.Harness != "codex" || got.ModelProviderID.Valid {
		t.Fatalf("explicit codex run = harness %q binding %+v, want a plain codex run", got.Harness, bindingOfRun(got))
	}
	fx.lane("default_harness", "codex")
	run, err = fx.create(nil, nil)
	if err != nil {
		t.Fatalf("codex default: %v", err)
	}
	if got := mustRun(t, fx.env, run.ID); got.Harness != "codex" || got.ModelProviderID.Valid {
		t.Fatalf("codex default run = harness %q binding %+v", got.Harness, bindingOfRun(got))
	}
}

// TestProviderBindingGuardCatchesAnUnstampedInsertLiveDB: an insert closure that forgets the five
// binding params (sqlc.narg: NULL) would silently turn a provider-sourced owner's run into a vendor
// run. createRunAtomic fails closed and commits no row.
func TestProviderBindingGuardCatchesAnUnstampedInsertLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	fx.addProvider(pvDefaultMap, true)
	fx.iid++
	iid := fx.iid
	_, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, func(q *store.Queries, r resolvedHarness) (store.Run, error) {
		return q.CreateRun(fx.env.ctx, store.CreateRunParams{
			UserID: fx.userID, RepoID: fx.repoID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
			IssueTitle: "unstamped", IssueDescription: "d", PlanSource: planSourceAgent, TriggerSource: "manual", Harness: string(r.Harness),
		})
	})
	if !errors.Is(err, errProviderBindingMismatch) {
		t.Fatalf("err = %v, want errProviderBindingMismatch", err)
	}
	if n := fx.runCount(); n != 0 {
		t.Fatalf("an unstamped insert left %d run(s)", n)
	}
}

// ---------------------------------------------------------------------------------------------
// Set and unusable: a typed refusal before any fallback.
// ---------------------------------------------------------------------------------------------

func TestProviderSourceUnusableRefusesWithoutFallbackLiveDB(t *testing.T) {
	breakers := []struct {
		name   string
		reason string
		do     func(fx *provFix)
	}{
		{"retired", SourceUnusableRetired, func(fx *provFix) {
			fx.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, fx.provider)
		}},
		{"url removed from the allowlist", SourceUnusableBaseURL, func(fx *provFix) {
			fx.svc.p.ProviderAllowedBaseURLs = []string{"https://other.example.com/x"}
		}},
		{"feature off", SourceUnusableBaseURL, func(fx *provFix) { fx.svc.p.ProviderAllowedBaseURLs = nil }},
		{"key deleted", SourceUnusableKeyMissing, func(fx *provFix) {
			fx.env.exec(`DELETE FROM user_secrets WHERE id = $1`, fx.key)
		}},
		{"key disabled", SourceUnusableKeyDisabled, func(fx *provFix) {
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, fx.key)
		}},
	}
	for _, b := range breakers {
		t.Run(b.name, func(t *testing.T) {
			fx := newProvFix(t)
			fx.anthropic()
			fx.codexDefault()
			fx.addProvider(pvDefaultMap, true)
			b.do(fx)

			// Implicit (unset default): never Codex around an unusable source, never the Anthropic token.
			if _, err := fx.create(nil, nil); !isUnusable(err, b.reason) {
				t.Fatalf("implicit create err = %v, want provider_source_unusable (%s)", err, b.reason)
			}
			// Explicit Claude, and a stored Claude default.
			if _, err := fx.create(h(HarnessClaude), nil); !isUnusable(err, b.reason) {
				t.Fatalf("explicit claude err = %v, want provider_source_unusable", err)
			}
			fx.lane("default_harness", "claude")
			if _, err := fx.create(nil, nil); !isUnusable(err, b.reason) {
				t.Fatalf("claude default err = %v, want provider_source_unusable", err)
			}
			if _, err := fx.svc.ResolveHarnessForUser(fx.env.ctx, fx.userID, nil); !errors.Is(err, ErrProviderSourceUnusable) {
				t.Fatalf("ResolveHarnessForUser err = %v", err)
			}
			if n := fx.runCount(); n != 0 {
				t.Fatalf("refusals left %d run(s)", n)
			}
			// An explicit Codex pin, and a usable Codex default, are not Claude and proceed.
			if run, err := fx.create(h(HarnessCodex), nil); err != nil || mustRun(t, fx.env, run.ID).Harness != "codex" {
				t.Fatalf("explicit codex: %v", err)
			}
			fx.lane("default_harness", "codex")
			if run, err := fx.create(nil, nil); err != nil || mustRun(t, fx.env, run.ID).Harness != "codex" {
				t.Fatalf("codex default: %v", err)
			}
			// A Codex default that is itself unusable falls to the refusal, not to Claude's token.
			fx.env.exec(`DELETE FROM user_secrets WHERE user_id = $1 AND kind IN ('codex_auth','openai_api_key')`, fx.userID)
			if _, err := fx.create(nil, nil); !errors.Is(err, ErrProviderSourceUnusable) {
				t.Fatalf("unusable codex default err = %v, want provider_source_unusable", err)
			}
		})
	}
}

// TestProviderSourceUnusableWithoutAnyVendorCredentialLiveDB: no token, no Codex, source unusable:
// the refusal names the source, not "no usable credential".
func TestProviderSourceUnusableWithoutAnyVendorCredentialLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.addProvider(pvDefaultMap, true)
	fx.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, fx.provider)
	if _, err := fx.create(nil, nil); !isUnusable(err, SourceUnusableRetired) {
		t.Fatalf("err = %v, want provider_source_unusable (retired)", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Automatic choice, overflow and prediction.
// ---------------------------------------------------------------------------------------------

func TestProviderSourceExcludesAutomaticChoiceAndOverflowLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	fx.codexDefault()
	fx.addProvider(pvDefaultMap, true)

	// Unset default, both engines usable: the automatic weekly-reset choice is off (Claude through the
	// source), and the recorded decision says excluded.
	fx.env.exec(`UPDATE users SET default_harness = NULL, harness_overflow_enabled = false WHERE id = $1`, fx.userID)
	run, err := fx.create(nil, nil)
	if err != nil {
		t.Fatalf("automatic-shaped create: %v", err)
	}
	got := mustRun(t, fx.env, run.ID)
	fx.wantBound(got, 1, fx.keyRev())
	if got.HarnessDecisionReason.String != harnessReasonExcluded {
		t.Fatalf("decision reason = %q, want excluded (automatic choice off)", got.HarnessDecisionReason.String)
	}

	// Overflow ON with a Claude default: excluded before the other-side resolution.
	fx.env.exec(`UPDATE users SET default_harness = 'claude', harness_overflow_enabled = true WHERE id = $1`, fx.userID)
	got = mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantBound(got, 1, fx.keyRev())
	if got.HarnessDecisionReason.String != harnessReasonExcluded {
		t.Fatalf("claude-default overflow reason = %q, want excluded", got.HarnessDecisionReason.String)
	}
	// ... and the other direction: a Codex default never moves to Claude's source (nor back).
	fx.env.exec(`UPDATE users SET default_harness = 'codex' WHERE id = $1`, fx.userID)
	got = mustRun(t, fx.env, mustCreate(t, fx).ID)
	if got.Harness != "codex" || got.ModelProviderID.Valid || got.HarnessDecisionReason.String != harnessReasonExcluded {
		t.Fatalf("codex-default overflow run = harness %q binding %+v reason %q, want codex, no binding, excluded", got.Harness, bindingOfRun(got), got.HarnessDecisionReason.String)
	}
}

func mustCreate(t *testing.T, fx *provFix) store.Run {
	t.Helper()
	run, err := fx.create(nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return run
}

// TestProviderSourcePredictionEqualsCreationLiveDB: PredictHarness answers what creation then does,
// for a usable source (exact claude, no overflow or automatic choice, no vendor credential) and for
// an unusable one (the same typed refusal).
func TestProviderSourcePredictionEqualsCreationLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	fx.codexDefault()
	fx.addProvider(pvDefaultMap, true)
	fx.env.exec(`UPDATE users SET harness_overflow_enabled = true WHERE id = $1`, fx.userID)
	for _, def := range []string{"", "claude"} {
		fx.lane("default_harness", def)
		pred, err := fx.predictHarness(nil)
		if err != nil {
			t.Fatalf("predict (default %q): %v", def, err)
		}
		if pred.Harness != HarnessClaude || pred.ChosenAtCreate || pred.Policy != "" || pred.Credential != nil {
			t.Fatalf("prediction (default %q) = %+v, want exact claude, no policy, no vendor credential", def, pred)
		}
		got := mustRun(t, fx.env, mustCreate(t, fx).ID)
		if got.Harness != string(pred.Harness) || !got.ModelProviderID.Valid {
			t.Fatalf("created %q bound=%v differs from the prediction %+v", got.Harness, got.ModelProviderID.Valid, pred)
		}
	}
	fx.lane("default_harness", "codex")
	if pred, err := fx.predictHarness(nil); err != nil || pred.Harness != HarnessCodex || pred.ChosenAtCreate {
		t.Fatalf("codex-default prediction = %+v err=%v, want exact codex (no overflow)", pred, err)
	}

	fx.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, fx.provider)
	fx.lane("default_harness", "")
	_, perr := fx.predictHarness(nil)
	_, cerr := fx.create(nil, nil)
	if !errors.Is(perr, ErrProviderSourceUnusable) || !errors.Is(cerr, ErrProviderSourceUnusable) {
		t.Fatalf("unusable: predict err = %v, create err = %v, want both provider_source_unusable", perr, cerr)
	}
}

// TestProviderSourceNoOverflowWithoutTxLiveDB: the non-transactional creation branch refuses a
// provider source instead of inserting without the freeze locks.
func TestProviderSourceNoOverflowWithoutTxLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.addProvider(pvDefaultMap, true)
	fx.svc.txBeginner = nil
	_, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
	if !errors.Is(err, errProviderCreateRequiresTx) {
		t.Fatalf("err = %v, want errProviderCreateRequiresTx", err)
	}
	if fx.runCount() != 0 {
		t.Fatal("a non-transactional provider create left a run")
	}
}

// ---------------------------------------------------------------------------------------------
// Freeze versus edit.
// ---------------------------------------------------------------------------------------------

// TestProviderFreezeSurvivesEditAndPruneLiveDB: an edit after the freeze leaves the run bound to
// the old revision, prune keeps it while a run references it, and a new run binds the new one.
func TestProviderFreezeSurvivesEditAndPruneLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.addProvider(pvDefaultMap, true)
	first := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantBound(first, 1, fx.keyRev())

	if rev := fx.editRevision(pvBase); rev != 2 {
		t.Fatalf("edit revision = %d", rev)
	}
	n, err := fx.env.q.PruneModelProviderRevisions(fx.env.ctx, store.PruneModelProviderRevisionsParams{ProviderID: fx.provider, OwnerUserID: fx.userID})
	if err != nil || n != 0 {
		t.Fatalf("prune = %d err=%v, want 0 (revision 1 is still referenced by a run)", n, err)
	}
	if again := mustRun(t, fx.env, first.ID); again.ModelProviderRevision.Int32 != 1 {
		t.Fatalf("the bound run moved to revision %d", again.ModelProviderRevision.Int32)
	}
	second := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantBound(second, 2, fx.keyRev())
}

// TestProviderEditWaitsForTheFreezeLiveDB: while a creation holds its freeze locks (inside the insert
// closure, after the share lock on the provider row), an edit's revision bump blocks, so a run is
// never bound to a revision edited away under it; it proceeds once the creation commits.
func TestProviderEditWaitsForTheFreezeLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.addProvider(pvDefaultMap, true)
	bumped := make(chan error, 1)
	inner := fx.insertAt(nil)
	run, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, func(q *store.Queries, r resolvedHarness) (store.Run, error) {
		// The freeze locks are held here. An edit on another connection must wait.
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		_, berr := fx.aux.Exec(ctx, `UPDATE model_providers SET current_revision = current_revision + 1 WHERE id = $1`, fx.provider)
		bumped <- berr
		return inner(q, r)
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if berr := <-bumped; berr == nil {
		t.Fatal("an edit committed while the creation held the freeze lock")
	}
	fx.wantBound(mustRun(t, fx.env, run.ID), 1, fx.keyRev())
	if rev := fx.editRevision(pvBase); rev != 2 {
		t.Fatalf("edit after the commit = revision %d, want 2 (the timed-out bump rolled back)", rev)
	}
}

// ---------------------------------------------------------------------------------------------
// Successors.
// ---------------------------------------------------------------------------------------------

func (pf *provFix) successor(src store.Run, explicit Harness) (store.Run, error) {
	pf.t.Helper()
	run, _, err := pf.svc.createRunAtomic(pf.env.ctx, pf.userID, &explicit, pf.insertAt(nil), harnessCreationOrigin{InheritFrom: &src})
	return run, err
}

func TestProviderSuccessorCopiesBindingAndSurvivesEditLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.addProvider(pvDefaultMap, true)
	src := mustRun(t, fx.env, mustCreate(t, fx).ID)

	got, err := fx.successor(src, HarnessClaude)
	if err != nil {
		t.Fatalf("successor: %v", err)
	}
	fx.wantBound(mustRun(t, fx.env, got.ID), 1, src.ModelProviderKeyRev.Int64)
	if bindingOfRun(got) != bindingOfRun(src) {
		t.Fatalf("successor binding %+v != source %+v", bindingOfRun(got), bindingOfRun(src))
	}

	// An endpoint edit (revision 2) and a different owner source do not move the successor.
	fx.editRevision("https://gw.example.com/anthropic")
	fx.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID)
	got, err = fx.successor(src, HarnessClaude)
	if err != nil {
		t.Fatalf("successor after edit and with no owner source: %v", err)
	}
	fx.wantBound(mustRun(t, fx.env, got.ID), 1, src.ModelProviderKeyRev.Int64)
}

func TestProviderSuccessorRefusesAStaleOrRevokedBindingLiveDB(t *testing.T) {
	cases := []struct {
		name string
		do   func(fx *provFix)
		want error
	}{
		{"key rotated", func(fx *provFix) {
			fx.env.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, fx.key)
		}, ErrProviderBindingStale},
		{"key disabled", func(fx *provFix) {
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, fx.key)
		}, ErrProviderBindingStale},
		{"key disabled without a rev bump", func(fx *provFix) {
			fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, fx.key)
		}, ErrProviderBindingStale},
		{"key deleted", func(fx *provFix) {
			fx.env.exec(`DELETE FROM user_secrets WHERE id = $1`, fx.key)
		}, ErrProviderBindingStale},
		{"url removed from the allowlist", func(fx *provFix) {
			fx.svc.p.ProviderAllowedBaseURLs = []string{"https://other.example.com/x"}
		}, ErrProviderDestinationNotAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newProvFix(t)
			fx.addProvider(pvDefaultMap, true)
			src := mustRun(t, fx.env, mustCreate(t, fx).ID)
			before := fx.runCount()
			c.do(fx)
			if _, err := fx.successor(src, HarnessClaude); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if fx.runCount() != before {
				t.Fatal("a refused successor left a run")
			}
		})
	}
}

// TestProviderSuccessorOfAVendorRunStaysVendorLiveDB: a vendor source run gives a vendor successor
// even when the owner NOW has a provider source, resolved vendor-only without consulting it; and a
// successor never refuses because the owner's current source is unusable.
func TestProviderSuccessorOfAVendorRunStaysVendorLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	vendor := mustRun(t, fx.env, mustCreate(t, fx).ID) // created before any provider exists
	fx.wantVendor(vendor)
	fx.addProvider(pvDefaultMap, true)
	got, err := fx.successor(vendor, HarnessClaude)
	if err != nil {
		t.Fatalf("vendor successor with a usable owner source: %v", err)
	}
	fx.wantVendor(mustRun(t, fx.env, got.ID))
	// An unusable owner source does not refuse the vendor lineage either.
	fx.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, fx.provider)
	if got, err = fx.successor(vendor, HarnessClaude); err != nil {
		t.Fatalf("vendor successor with an unusable owner source: %v", err)
	}
	fx.wantVendor(mustRun(t, fx.env, got.ID))
	// ... and without a vendor token it is refused as today (no_credential_for_harness), not as a provider error.
	fx.env.exec(`DELETE FROM user_secrets WHERE user_id = $1 AND kind = 'anthropic_token'`, fx.userID)
	if _, err = fx.successor(vendor, HarnessClaude); !errors.Is(err, ErrNoCredentialForHarness) || errors.Is(err, ErrProviderSourceUnusable) {
		t.Fatalf("err = %v, want the vendor refusal ErrNoCredentialForHarness", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Chat.
// ---------------------------------------------------------------------------------------------

func TestProviderChatCreateFreezesAndContinueCopiesLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()

	// Null source: today's non-transactional insert, no binding.
	vchat, err := fx.svc.CreateChatRun(fx.env.ctx, fx.userID, "hello vendor")
	if err != nil {
		t.Fatalf("vendor chat: %v", err)
	}
	fx.wantVendor(mustRun(t, fx.env, vchat.ID))

	fx.addProvider(pvDefaultMap, true)
	chat, err := fx.svc.CreateChatRun(fx.env.ctx, fx.userID, "hello provider")
	if err != nil {
		t.Fatalf("provider chat: %v", err)
	}
	got := mustRun(t, fx.env, chat.ID)
	fx.wantBound(got, 1, fx.keyRev())
	if got.Kind != "chat" {
		t.Fatalf("kind = %q", got.Kind)
	}

	// Continue copies the ended chat's binding; the owner's source changing does not move it.
	fx.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, chat.ID)
	fx.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID)
	cont, err := fx.svc.ContinueChat(fx.env.ctx, fx.userID, chat.ID)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	fx.wantBound(mustRun(t, fx.env, cont.ID), 1, got.ModelProviderKeyRev.Int64)

	// Key rotation stales the next continuation; a vendor chat continues as a vendor chat.
	fx.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, cont.ID)
	fx.env.exec(`UPDATE user_secrets SET enablement_rev = enablement_rev + 1 WHERE id = $1`, fx.key)
	if _, err := fx.svc.ContinueChat(fx.env.ctx, fx.userID, cont.ID); !errors.Is(err, ErrProviderBindingStale) {
		t.Fatalf("continue after rotation err = %v, want ErrProviderBindingStale", err)
	}
	fx.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, vchat.ID)
	fx.selectSource() // the owner now has a provider; the vendor chat must still continue as vendor
	vcont, err := fx.svc.ContinueChat(fx.env.ctx, fx.userID, vchat.ID)
	if err != nil {
		t.Fatalf("vendor continue: %v", err)
	}
	fx.wantVendor(mustRun(t, fx.env, vcont.ID))
}

func TestProviderChatCreateRefusesAnUnusableSourceLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	fx.addProvider(pvDefaultMap, true)
	fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, fx.key)
	before := fx.runCount()
	if _, err := fx.svc.CreateChatRun(fx.env.ctx, fx.userID, "hello"); !isUnusable(err, SourceUnusableKeyDisabled) {
		t.Fatalf("err = %v, want provider_source_unusable (key_disabled), never a vendor chat", err)
	}
	if fx.runCount() != before {
		t.Fatal("a refused chat left a run")
	}
}

// ---------------------------------------------------------------------------------------------
// Lanes.
// ---------------------------------------------------------------------------------------------

func TestProviderLanesRefuseAProviderSourcedOwnerLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	fx.codexDefault()
	lane := func() error {
		_, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, h(HarnessCodex), fx.insertAt(nil), harnessCreationOrigin{RefuseProviderOwner: true})
		return err
	}
	if err := lane(); err != nil {
		t.Fatalf("a null-source owner's lane create: %v", err)
	}
	fx.addProvider(pvDefaultMap, true)
	before := fx.runCount()
	// Usable, then unusable: both are lane refusals, whatever harness the lane resolves to.
	if err := lane(); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Fatalf("usable source: err = %v, want provider_lane_unsupported", err)
	}
	fx.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, fx.provider)
	if err := lane(); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Fatalf("unusable source: err = %v, want provider_lane_unsupported", err)
	}
	if fx.runCount() != before {
		t.Fatal("a refused lane left a run")
	}
}

// TestProviderCrossCheckFailsClosedToTheHumanGateLiveDB: a required plan cross-check of a
// provider-bound lead cannot get a checker (the lane is refused), and the lead's "checker
// unavailable" report is the non-pass that forces the human gate: the run parks at
// awaiting_approval with auto_approve cleared, never an auto-approval. The same report from a
// vendor lead whose checker family is usable stays refused (the attestation is the provider rule's).
func TestProviderCrossCheckFailsClosedToTheHumanGateLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	fx.codexDefault()
	fx.addProvider(pvDefaultMap, true)
	lead := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantBound(lead, 1, fx.keyRev())
	// The checker is the lead's opposite family (codex), usable, yet the lane is refused.
	_, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, h(HarnessCodex), fx.insertAt(nil), harnessCreationOrigin{RefuseProviderOwner: true})
	if !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Fatalf("checker creation err = %v, want provider_lane_unsupported", err)
	}

	report := func(runID uuid.UUID) (applied bool, got store.Run, err error) {
		fx.env.exec(`UPDATE runs SET status='running', worker_id=$2, auto_approve=true, plan_source='agent',
			plan_cross_check_required=true, claim_generation=1 WHERE id=$1`, runID, fx.workerID)
		gen, reason := int64(1), "checker_unavailable"
		plan, size := "A plan that needed a checker", "s"
		caps, tools, ms := []string{}, []string{}, []Milestone{}
		req := StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
			RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, PlanCrossCheckGateReason: &reason}
		_, applied, err = fx.svc.SetState(fx.env.ctx, store.Worker{ID: fx.workerID, UserID: fx.userID}, runID, req)
		return applied, mustRun(t, fx.env, runID), err
	}
	applied, got, err := report(lead.ID)
	if err != nil || !applied || got.Status != "awaiting_approval" || got.AutoApprove ||
		got.PlanCrossCheckGateReason.String != "checker_unavailable" {
		t.Fatalf("provider lead report: applied=%v err=%v status=%s auto_approve=%v reason=%+v, want the human gate (awaiting_approval, auto_approve cleared)",
			applied, err, got.Status, got.AutoApprove, got.PlanCrossCheckGateReason)
	}

	// A vendor lead of a null-source owner with a usable checker family: the report is refused.
	fx.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID)
	vendor := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantVendor(vendor)
	if applied, got, err = report(vendor.ID); applied || !errors.Is(err, ErrInvalidState) || got.Status != "running" {
		t.Fatalf("vendor lead report: applied=%v err=%v status=%s, want ErrInvalidState and no change", applied, err, got.Status)
	}
	// A provider-sourced OWNER (lead not bound): also attested.
	fx.selectSource()
	if applied, got, err = report(vendor.ID); err != nil || !applied || got.AutoApprove {
		t.Fatalf("provider-owner report: applied=%v err=%v auto_approve=%v, want the human gate", applied, err, got.AutoApprove)
	}
}

// ---------------------------------------------------------------------------------------------
// Overrides and switching.
// ---------------------------------------------------------------------------------------------

func TestProviderOwnerRefusesCredentialOverridesLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	fx.addProvider(pvDefaultMap, true)

	// create --token (run create), every non-inherit mode.
	for _, mode := range []string{CredentialOverrideModeAuto, CredentialOverrideModeDefault, CredentialOverrideModePinned} {
		id := fx.token
		if _, err := fx.create(nil, &RawCredentialOverride{Mode: mode, SecretID: &id}); !errors.Is(err, ErrCredentialOverrideProviderSource) {
			t.Errorf("create --token %s: err = %v, want the provider-source override refusal", mode, err)
		}
	}
	if fx.runCount() != 0 {
		t.Fatalf("refused overrides left %d run(s)", fx.runCount())
	}
	// inherit clears and stays legal.
	if _, err := fx.create(nil, &RawCredentialOverride{Mode: CredentialOverrideModeInherit}); err != nil {
		t.Errorf("create with inherit: %v", err)
	}
	// Stored (schedule) override, the effective-settings and schedule validators.
	if err := checkStoredOverrideEnabled(fx.env.ctx, fx.env.q, fx.userID, &CredentialOverride{Mode: CredentialOverrideModeAuto}); !errors.Is(err, ErrCredentialOverrideProviderSource) {
		t.Errorf("stored override: err = %v", err)
	}
	if _, err := fx.svc.ResolveCredentialOverride(fx.env.ctx, fx.userID, "issue", "claude", CredentialOverrideModeAuto, nil); !errors.Is(err, ErrCredentialOverrideProviderSource) {
		t.Errorf("effective-settings/schedule validator: err = %v", err)
	}
	// An unusable source refuses overrides too (the owner still has a provider selected).
	fx.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, fx.provider)
	if _, err := fx.svc.ResolveCredentialOverride(fx.env.ctx, fx.userID, "issue", "claude", CredentialOverrideModeAuto, nil); !errors.Is(err, ErrCredentialOverrideProviderSource) {
		t.Errorf("validator with an unusable source: err = %v", err)
	}
	// No provider source: today's behaviour, byte for byte.
	fx.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID)
	if o, err := fx.svc.ResolveCredentialOverride(fx.env.ctx, fx.userID, "issue", "claude", CredentialOverrideModeAuto, nil); err != nil || o == nil || o.Mode != CredentialOverrideModeAuto {
		t.Errorf("null-source validator = %+v err=%v, want the auto override", o, err)
	}
}

func TestProviderSetTokenRefusalsLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	vendorRun := mustRun(t, fx.env, mustCreate(t, fx).ID) // a queued vendor run
	fx.addProvider(pvDefaultMap, true)
	boundRun := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantBound(boundRun, 1, fx.keyRev())

	// A provider-bound run refuses every mode and writes nothing.
	for _, mode := range []string{CredentialOverrideModeAuto, CredentialOverrideModeDefault, CredentialOverrideModeInherit} {
		if _, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, boundRun.ID, mode, nil); !errors.Is(err, ErrCredentialOverrideProviderSource) {
			t.Errorf("provider-bound set-token %s: err = %v", mode, err)
		}
	}
	if r := mustRun(t, fx.env, boundRun.ID); r.CredentialOverrideMode.Valid {
		t.Errorf("a refused switch wrote override mode %q", r.CredentialOverrideMode.String)
	}
	// A vendor run switched to `default` while the owner's source is a provider is refused...
	if _, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, vendorRun.ID, CredentialOverrideModeDefault, nil); !errors.Is(err, ErrCredentialOverrideProviderSource) {
		t.Errorf("vendor run to default with a provider source: err = %v", err)
	}
	if r := mustRun(t, fx.env, vendorRun.ID); r.CredentialOverrideMode.Valid {
		t.Errorf("a refused default switch wrote override mode %q", r.CredentialOverrideMode.String)
	}
	// ... but re-pointing it at another vendor token (pinned) is still a vendor switch and works.
	id := fx.token
	if _, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, vendorRun.ID, CredentialOverrideModePinned, &id); err != nil {
		t.Errorf("vendor run pinned switch: %v", err)
	}
	// Without a provider source `default` is accepted as today.
	fx.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID)
	if _, err := fx.svc.SetRunCredential(fx.env.ctx, fx.userID, vendorRun.ID, CredentialOverrideModeDefault, nil); err != nil {
		t.Errorf("null-source default switch: %v", err)
	}
}

// ---------------------------------------------------------------------------------------------
// Models.
// ---------------------------------------------------------------------------------------------

func TestProviderModelMapRefusesUnmappedModelsLiveDB(t *testing.T) {
	// A map without sonnet and fable: opus + haiku only.
	const sparse = `{"opus":{"model":"corp-opus"},"haiku":{"model":"corp-haiku"}}`
	fx := newProvFix(t)
	fx.addProvider(sparse, true)

	unmapped := func(err error, model string) {
		t.Helper()
		var u *ProviderModelUnmappedError
		if !errors.Is(err, ErrProviderModelUnmapped) || !errors.As(err, &u) || u.Model != model {
			t.Fatalf("err = %v, want provider_model_unmapped naming %q", err, model)
		}
	}
	before := fx.runCount()
	// No model set: the CLI default is the sonnet alias, which this map lacks.
	_, err := fx.create(nil, nil)
	unmapped(err, "sonnet")
	// A user lane default and a run override that are not mapped.
	fx.lane("default_claude_model", "fable")
	_, err = fx.create(nil, nil)
	unmapped(err, "fable")
	fx.lane("default_claude_model", "claude-opus-4-1") // a full vendor model id
	_, err = fx.create(nil, nil)
	unmapped(err, "claude-opus-4-1")
	if fx.runCount() != before {
		t.Fatal("a refused creation left a run")
	}
	// Mapped values pass: an alias present in the map, and exactly a mapped target.
	fx.lane("default_claude_model", "opus")
	if _, err := fx.create(nil, nil); err != nil {
		t.Fatalf("mapped alias: %v", err)
	}
	fx.lane("default_claude_model", "corp-haiku")
	if _, err := fx.create(nil, nil); err != nil {
		t.Fatalf("mapped target: %v", err)
	}
	// An enabled template whose own model is not mapped refuses creation, naming it.
	fx.lane("default_claude_model", "opus")
	fx.template("reviewer-x", "fable", "")
	_, err = fx.create(nil, nil)
	unmapped(err, "fable")
	if !strings.Contains(err.Error(), "reviewer-x") {
		t.Fatalf("err %q does not name the template", err)
	}
	// ... while a template pinned to a mapped alias is fine.
	fx.env.exec(`UPDATE agent_templates SET model = 'haiku' WHERE name = 'reviewer-x'`)
	if _, err := fx.create(nil, nil); err != nil {
		t.Fatalf("mapped template: %v", err)
	}
}

// TestProviderModelCheckMirrorsClaimTimeLeadLiveDB: the creation check maps the lead the claim would
// run, not the raw stored values. A Codex role token in the Claude lane and an incompatible frozen
// model are dropped at the claim (the CLI default, the sonnet alias, runs), so they must not refuse
// creation; the prediction names the same lead; a template the run excludes is not checked.
func TestProviderModelCheckMirrorsClaimTimeLeadLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.addProvider(pvDefaultMap, true)

	// A Codex role token in the Claude lane: dropped at the claim, so creation succeeds and the
	// prediction (which runs the same resolver) ships no lane model either.
	fx.lane("default_claude_model", "gpt-6-sol")
	if pred := fx.predictLead(HarnessClaude, false); pred.ClaimModel != nil {
		t.Fatalf("prediction ships lead %q, want none (the Codex token is dropped at the claim)", *pred.ClaimModel)
	}
	if _, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil), harnessCreationOrigin{}); err != nil {
		t.Fatalf("a Codex role token in the Claude lane refused creation: %v", err)
	}

	// An incompatible frozen model (a schedule pin the Claude harness cannot run) is dropped too.
	pin := "gpt-6-sol"
	if _, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(&pin), harnessCreationOrigin{}); err != nil {
		t.Fatalf("an incompatible frozen model refused creation: %v", err)
	}
	// ... while a compatible frozen model that the map lacks still refuses.
	unmapped := "claude-opus-4-1"
	if _, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(&unmapped), harnessCreationOrigin{}); !errors.Is(err, ErrProviderModelUnmapped) {
		t.Fatalf("an unmapped compatible frozen model: err = %v, want provider_model_unmapped", err)
	}

	// A template whose model is unmapped refuses, unless the run excludes it.
	fx.lane("default_claude_model", "")
	fx.template("reviewer-x", "fable", "")
	if _, err := fx.create(nil, nil); !errors.Is(err, ErrProviderModelUnmapped) {
		t.Fatalf("an unmapped template: err = %v, want provider_model_unmapped", err)
	}
	fx.iid++
	iid := fx.iid
	excluding := func(q *store.Queries, r resolvedHarness) (store.Run, error) {
		b := bindingOf(r)
		return q.CreateRun(fx.env.ctx, store.CreateRunParams{
			UserID: fx.userID, RepoID: fx.repoID, IssueIid: pgtype.Int8{Int64: iid, Valid: true},
			IssueTitle: "provider", IssueDescription: "d", PlanSource: planSourceAgent, TriggerSource: "manual",
			Harness: string(r.Harness), AgentExclusions: []byte(`["reviewer-x"]`),
			ModelProviderID: b.ProviderID, ModelProviderRevision: b.Revision, ModelProviderKeyMode: b.KeyMode,
			ModelProviderKeySecretID: b.KeySecret, ModelProviderKeyRev: b.KeyRev,
		})
	}
	if _, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, excluding, harnessCreationOrigin{}); err != nil {
		t.Fatalf("an excluded unmapped template refused creation: %v", err)
	}
}

// TestProviderModelCheckAppliesToSuccessorsAndChatLiveDB: the frozen map decides at a successor's
// and a chat's creation too.
func TestProviderModelCheckAppliesToSuccessorsAndChatLiveDB(t *testing.T) {
	const noSonnet = `{"opus":{"model":"corp-opus"},"haiku":{"model":"corp-haiku"}}`
	fx := newProvFix(t)
	fx.addProvider(noSonnet, true)
	fx.lane("default_claude_model", "opus")
	src := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.lane("default_claude_model", "sonnet") // unmapped now
	if _, err := fx.successor(src, HarnessClaude); !errors.Is(err, ErrProviderModelUnmapped) {
		t.Fatalf("successor err = %v, want provider_model_unmapped", err)
	}
	if _, err := fx.svc.CreateChatRun(fx.env.ctx, fx.userID, "hello"); !errors.Is(err, ErrProviderModelUnmapped) {
		t.Fatalf("chat err = %v, want provider_model_unmapped", err)
	}
}

// ---------------------------------------------------------------------------------------------
// The in-place switch apply, the judge and the job lanes.
// ---------------------------------------------------------------------------------------------

// TestProviderBoundRunNeverAppliesAnInPlaceSwitchLiveDB: even with a switch stamp pending (written by
// some other path), applying it to a provider-bound run is refused at step 0, writing nothing.
func TestProviderBoundRunNeverAppliesAnInPlaceSwitchLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, op := f.run(t, "running")
	provider, key := seedProviderFor(t, f.env, f.o.userID, pvDefaultMap)
	// The exclusivity CHECK needs no vendor credential on a bound run.
	f.env.exec(`UPDATE runs SET anthropic_secret_id = NULL, credential_override_mode = NULL, credential_override_secret_id = NULL,
	               model_provider_id = $2, model_provider_revision = 1, model_provider_key_mode = 'user_key',
	               model_provider_key_secret_id = $3, model_provider_key_rev = 0 WHERE id = $1`, run, provider, key)
	f.noWrite(t, run, func() {
		f.want(t, f.apply(t, run, f.req(op)), ApplyOutcomeNotApplied, applyReasonProviderBound)
	})
}

// TestProviderOwnerGetsProviderBoundJudgeLiveDB: with a Claude source selected (and no judge pin) the
// judge of a completed run, bound or vendor, is itself bound to the owner's source (PRD #4322 M1a),
// never a vendor judge; with the source cleared the vendor judge is created as before.
func TestProviderOwnerGetsProviderBoundJudgeLiveDB(t *testing.T) {
	fx := newProvFix(t)
	seedDefaultAnthropicToken(t, fx.env, fx.userID)
	fx.addProvider(pvDefaultMap, false)
	vendor := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantVendor(vendor)
	fx.selectSource()
	bound := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantBound(bound, 1, fx.keyRev())

	judge := judgeEnqueueSvc(fx.env, fx.userID)
	judge.p.ProviderAllowedBaseURLs = fx.svc.p.ProviderAllowedBaseURLs
	for _, id := range []uuid.UUID{vendor.ID, bound.ID} {
		fx.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, id)
		judge.maybeEnqueueJudge(fx.env.ctx, mustRun(t, fx.env, id))
		jid, _, ok := judgeForTarget(t, fx.env, id)
		if !ok {
			t.Fatalf("run %s: the owner's source is usable, so a provider-bound judge must be created", id)
		}
		fx.wantBound(mustRun(t, fx.env, jid), 1, fx.keyRev())
	}
	fx.env.exec(`UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID)
	control := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, control.ID)
	judge.maybeEnqueueJudge(fx.env.ctx, mustRun(t, fx.env, control.ID))
	jid, _, ok := judgeForTarget(t, fx.env, control.ID)
	if !ok {
		t.Fatal("control: with no provider source the vendor run is judged as before")
	}
	fx.wantVendor(mustRun(t, fx.env, jid))
}

// TestProviderOwnerJudgeSkipIsVisibleLiveDB (PRD #1429 D4: a derived run that is not created needs a
// visible reason): a provider-only owner (no Anthropic token) gets a provider-bound judge while the
// source is usable and no vendor-credential skip, and the automatic judge refused for an unusable
// source records the provider_source_unusable skip.
func TestProviderOwnerJudgeSkipIsVisibleLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.addProvider(pvDefaultMap, true) // a provider-only owner: no Anthropic token
	judge := judgeEnqueueSvc(fx.env, fx.userID)
	judge.p.ProviderAllowedBaseURLs = fx.svc.p.ProviderAllowedBaseURLs

	usable := seedJudgeTarget(t, fx.env, fx.userID, fx.repoID, "claude", 9701)
	judge.maybeEnqueueJudge(fx.env.ctx, usable)
	jid, _, ok := judgeForTarget(t, fx.env, usable.ID)
	if !ok {
		t.Fatal("a provider-only owner with a usable source must get a provider-bound judge, not a missing-token skip")
	}
	fx.wantBound(mustRun(t, fx.env, jid), 1, fx.keyRev())

	fx.env.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, fx.provider)
	unusable := seedJudgeTarget(t, fx.env, fx.userID, fx.repoID, "claude", 9702)
	judge.maybeEnqueueJudge(fx.env.ctx, unusable)
	noJudgeAnywhere(t, fx.env, unusable.ID, "provider-sourced owner with a retired source")
	wantSkip(t, fx.env, unusable.ID, "claude", JudgeSkipProviderSourceUnusable)
}

// TestProviderOwnerGetsNoJobLiveDB: the entry-point proof of the job lane (PRD #4322 M1b; the name is
// kept, the contract flipped). A job is created as before for an owner with no source; with a usable
// Claude source it is provider-bound (never refused); an unusable source is the typed
// provider_source_unusable refusal; and a site-list job under a selected source (usable or not) keeps
// provider_lane_unsupported. Refusals leave no run.
func TestProviderOwnerGetsNoJobLiveDB(t *testing.T) {
	e := setupJobLiveDB(t, 0)
	e.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	u := e.seedJobUser(t)
	e.ownJobsCleanup(t, u)
	control, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatalf("control job: %v", err)
	}
	if r := mustRun(t, e.codexTestEnv, control.ID); providerBoundRun(r) || r.ModelProviderKeyMode.Valid {
		t.Fatalf("control job carries a binding: %+v", bindingOfRun(r))
	}
	_, siteList := e.seedProfile(t, "docs.example.com")
	provider, key := seedProviderFor(t, e.codexTestEnv, u, pvDefaultMap)
	t.Cleanup(func() {
		if _, err := e.pool.Exec(context.Background(), `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, u); err != nil {
			t.Errorf("clear the Claude source in cleanup: %v", err)
		}
	})
	e.exec(`UPDATE users SET claude_provider_id = $2, claude_provider_scope_id = id WHERE id = $1`, u, provider)

	v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatalf("usable source: %v, want a provider-bound job", err)
	}
	bound := mustRun(t, e.codexTestEnv, v.ID)
	if !bound.ModelProviderID.Valid || uuid.UUID(bound.ModelProviderID.Bytes) != provider || uuid.UUID(bound.ModelProviderKeySecretID.Bytes) != key ||
		bound.ModelProviderKeyMode.String != "user_key" || bound.AnthropicSecretID.Valid || bound.Harness != "claude" {
		t.Fatalf("provider job = %+v, want a Claude job frozen to provider %s key %s with no vendor credential", bindingOfRun(bound), provider, key)
	}

	before := e.jobCount(t, u)
	if _, err := e.svc.CreateJobRun(e.ctx, profileReq(cliCaller(u), siteList)); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Fatalf("site-list job under a usable source: err = %v, want provider_lane_unsupported", err)
	}
	e.exec(`UPDATE model_providers SET retired_at = now() WHERE id = $1`, provider)
	if _, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u))); !isUnusable(err, SourceUnusableRetired) {
		t.Fatalf("unusable source: err = %v, want provider_source_unusable (retired)", err)
	}
	if _, err := e.svc.CreateJobRun(e.ctx, profileReq(cliCaller(u), siteList)); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Fatalf("site-list job under an unusable source: err = %v, want provider_lane_unsupported", err)
	}
	if e.jobCount(t, u) != before {
		t.Fatal("a refused job left a run")
	}
}

// TestProviderScheduledPromptResolvesTheSourceAtFireTimeLiveDB: a schedule's harness pin resolves the
// owner's source when the run is created (the fire), not when the schedule was saved, so a pinned
// Claude schedule binds the CURRENT provider, a stored override is refused, and an unusable source
// refuses the fire.
func TestProviderScheduledPromptResolvesTheSourceAtFireTimeLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	claude := HarnessClaude
	fire := func(override *CredentialOverride) (store.Run, error) {
		sched := seedPromptScheduleRow(t, fx.env, fx.userID, fx.repoID)
		return fx.svc.CreatePromptRun(fx.env.ctx, fx.userID, fx.repoID, sched, "t", "p", false, false, nil, nil, false, override, &claude)
	}
	// Saved before any provider: a vendor fire.
	run, err := fire(nil)
	if err != nil {
		t.Fatalf("vendor fire: %v", err)
	}
	fx.wantVendor(mustRun(t, fx.env, run.ID))
	// The owner selects a provider afterwards: the next fire binds it.
	fx.addProvider(pvDefaultMap, true)
	if run, err = fire(nil); err != nil {
		t.Fatalf("provider fire: %v", err)
	}
	fx.wantBound(mustRun(t, fx.env, run.ID), 1, fx.keyRev())
	if _, err = fire(&CredentialOverride{Mode: CredentialOverrideModeAuto}); !errors.Is(err, ErrCredentialOverrideProviderSource) {
		t.Fatalf("stored override on a provider fire: err = %v", err)
	}
	fx.env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, fx.key)
	if _, err = fire(nil); !isUnusable(err, SourceUnusableKeyDisabled) {
		t.Fatalf("fire with a disabled key: err = %v, want provider_source_unusable", err)
	}
}
