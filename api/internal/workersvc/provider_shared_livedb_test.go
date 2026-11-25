package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_shared_livedb_test.go is PRD #4324 (admin-shared Claude provider definitions with per-user
// keys) against a real Postgres: a recipient's run freezes the SHARED revision and the RECIPIENT's own
// key, the claim carries exactly that key in the personal-provider shape, and retire, delete,
// missing-key and inaccessible-reference cases fail closed with no vendor fallback and no decrypt.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via ./e2e/run-store-it.sh.
// Every test name ends LiveDB so the store-IT sweep selects it.

// seedSharedProvider inserts a shared definition of the given engine (revision 1 at pvBase; a Codex
// revision is openai_responses with bearer auth) and registers its cleanup: every selection of either
// engine is cleared first (users_claude_provider_fkey and users_codex_provider_fkey are RESTRICT), then
// the row is deleted (its keys and evidence cascade).
func seedSharedProvider(t *testing.T, env codexTestEnv, engine, modelMap string) (id uuid.UUID, name string) {
	t.Helper()
	p, err := env.q.InsertSharedModelProvider(env.ctx, store.InsertSharedModelProviderParams{Name: "shared-" + uuid.NewString()[:8], Engine: engine})
	if err != nil {
		t.Fatalf("insert shared provider: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := env.pool.Exec(ctx, `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE claude_provider_id = $1`, p.ID); err != nil {
			t.Errorf("clear shared Claude selections in cleanup: %v", err)
		}
		if _, err := env.pool.Exec(ctx, `UPDATE users SET codex_provider_id = NULL, codex_provider_scope_id = NULL WHERE codex_provider_id = $1`, p.ID); err != nil {
			t.Errorf("clear shared Codex selections in cleanup: %v", err)
		}
		if _, err := env.pool.Exec(ctx, `DELETE FROM model_providers WHERE id = $1`, p.ID); err != nil {
			t.Errorf("delete shared provider in cleanup: %v", err)
		}
	})
	protocol := "anthropic_messages"
	if engine == "codex" {
		protocol = "openai_responses"
	}
	if err := env.q.InsertModelProviderRevision(env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: protocol, BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(modelMap),
	}); err != nil {
		t.Fatalf("insert shared revision: %v", err)
	}
	return p.ID, p.Name
}

// seedSealedKey attaches userID's own gateway key (really sealed, so a claim can open it) to a
// definition visible to them, through the handler-equivalent query.
func seedSealedKey(t *testing.T, env codexTestEnv, userID, providerID uuid.UUID, plain string) uuid.UUID {
	t.Helper()
	sealed, err := env.box.Seal([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	k, err := env.q.InsertProviderKeySecret(env.ctx, store.InsertProviderKeySecretParams{
		UserID: userID, Label: providerID.String(), Ciphertext: sealed, SealedWith: "master", ProviderID: pgtype.UUID{Bytes: providerID, Valid: true},
	})
	if err != nil {
		t.Fatalf("insert key: %v", err)
	}
	return k.ID
}

func seedPersonalSealed(t *testing.T, env codexTestEnv, userID uuid.UUID, plain string) (provider, key uuid.UUID) {
	t.Helper()
	p, err := env.q.InsertModelProvider(env.ctx, store.InsertModelProviderParams{OwnerUserID: userID, Name: "p-" + uuid.NewString()[:8], Engine: "claude"})
	if err != nil {
		t.Fatalf("insert personal provider: %v", err)
	}
	if err := env.q.InsertModelProviderRevision(env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "anthropic_messages", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(pvDefaultMap),
	}); err != nil {
		t.Fatalf("insert personal revision: %v", err)
	}
	return p.ID, seedSealedKey(t, env, userID, p.ID, plain)
}

// selectClaudeSource selects (or, with uuid.Nil, clears) userID's Claude source through the same
// statement the handler uses.
func selectClaudeSource(t *testing.T, env codexTestEnv, userID, providerID uuid.UUID) {
	t.Helper()
	arg := store.SetUserProviderSourceParams{ID: userID, Engine: "claude"}
	if providerID != uuid.Nil {
		arg.ProviderID = pgtype.UUID{Bytes: providerID, Valid: true}
	}
	if _, err := env.q.SetUserProviderSource(env.ctx, arg); err != nil {
		t.Fatalf("select source %s for %s: %v", providerID, userID, err)
	}
}

// useShared points the fixture at a fresh shared definition with B's own sealed key, selected.
func (pf *provFix) useShared(plain string) (name string) {
	pf.t.Helper()
	pf.provider, name = seedSharedProvider(pf.t, pf.env, "claude", pvDefaultMap)
	pf.key = seedSealedKey(pf.t, pf.env, pf.userID, pf.provider, plain)
	selectClaudeSource(pf.t, pf.env, pf.userID, pf.provider)
	return name
}

// providerCapableWorker is the fixture worker as a claude_provider_v1 worker.
func (pf *provFix) providerCapableWorker() store.Worker {
	return store.Worker{ID: pf.workerID, UserID: pf.userID, ProtocolCapabilities: []string{capability.ClaudeProviderV1}}
}

// assemble claims run for the fixture worker and returns the payload.
func (pf *provFix) assemble(run store.Run) (*ClaimPayload, error) {
	pf.t.Helper()
	pf.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, run.ID, pf.workerID)
	return pf.svc.assembleClaim(pf.env.ctx, pf.providerCapableWorker(), mustRun(pf.t, pf.env, run.ID))
}

func jsonObject(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func wireKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSharedProviderRunFreezesTheRecipientsKeyLiveDB: B attaches an enabled personal key to a shared
// definition and selects it. B's new Claude run freezes the shared revision and B's key secret id; the
// claim carries B's opened key exactly and no vendor credential; a retry, even after the admin edits
// the definition, keeps the same frozen binding. Publication safety (a): the claim's shape is the
// personal-provider shape, with no owner or scope field.
func TestSharedProviderRunFreezesTheRecipientsKeyLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	plainB := "gw-key-B-" + uuid.NewString()
	fx.useShared(plainB)

	run := mustRun(t, fx.env, mustCreate(t, fx).ID)
	fx.wantBound(run, 1, fx.keyRev())
	payload, err := fx.assemble(run)
	if err != nil || payload == nil {
		t.Fatalf("assembleClaim = (%v, %v)", payload != nil, err)
	}
	p := payload.Secrets.Provider
	if p == nil || p.Credential != plainB || p.ProviderID != fx.provider.String() || p.Revision != 1 || p.BaseURL != pvBase || p.Engine != "claude" {
		t.Fatalf("provider binding = %+v, want B's key %q on the shared revision", p, plainB)
	}
	if payload.Secrets.AnthropicOAuthToken != "" || payload.Secrets.Codex != nil {
		t.Fatalf("a shared-provider claim carries a vendor credential: %+v", payload.Secrets)
	}
	wantNoVendor := func(r store.Run) {
		t.Helper()
		if r.CredentialOverrideSecretID.Valid || r.AnthropicSecretID.Valid {
			t.Fatalf("run recorded a vendor or override credential after the claim: %+v", bindingOfRun(r))
		}
	}
	wantNoVendor(mustRun(t, fx.env, run.ID))

	// Retry: the admin edits the definition (revision 2) and the run is claimed again; the frozen
	// binding is unchanged.
	rev, err := fx.env.q.BumpSharedModelProviderRevision(fx.env.ctx, fx.provider)
	if err != nil {
		t.Fatalf("bump shared: %v", err)
	}
	if err := fx.env.q.InsertModelProviderRevision(fx.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: fx.provider, Revision: rev, Protocol: "anthropic_messages", BaseUrl: pvBase, AuthStyle: "bearer",
		ModelMap: []byte(`{"opus":{"model":"edited-opus"},"sonnet":{"model":"edited-sonnet"},"haiku":{"model":"edited-haiku"}}`),
	}); err != nil {
		t.Fatalf("insert edited revision: %v", err)
	}
	again, err := fx.assemble(run)
	if err != nil || again.Secrets.Provider == nil {
		t.Fatalf("retry assembleClaim = (%+v, %v)", again, err)
	}
	if q := again.Secrets.Provider; q.Revision != 1 || q.Credential != plainB || q.ModelMap["haiku"].Model != "corp-haiku" {
		t.Fatalf("retry binding = %+v, want the frozen revision 1 and B's key", q)
	}
	wantNoVendor(mustRun(t, fx.env, run.ID))
	if again2 := mustRun(t, fx.env, run.ID); bindingOfRun(again2) != bindingOfRun(run) {
		t.Fatalf("the frozen binding moved: %+v -> %+v", bindingOfRun(run), bindingOfRun(again2))
	}

	// Publication safety (a): the same run shape through a PERSONAL provider carries the same JSON keys.
	plainP := "gw-key-personal-" + uuid.NewString()
	personal, _ := seedPersonalSealed(t, fx.env, fx.userID, plainP)
	selectClaudeSource(t, fx.env, fx.userID, personal)
	prun := mustRun(t, fx.env, mustCreate(t, fx).ID)
	ppayload, err := fx.assemble(prun)
	if err != nil || ppayload.Secrets.Provider == nil || ppayload.Secrets.Provider.Credential != plainP {
		t.Fatalf("personal assembleClaim = (%+v, %v)", ppayload, err)
	}
	sharedSecrets, personalSecrets := jsonObject(t, payload.Secrets), jsonObject(t, ppayload.Secrets)
	if got, want := strings.Join(wireKeys(sharedSecrets), ","), strings.Join(wireKeys(personalSecrets), ","); got != want {
		t.Fatalf("secrets keys differ: shared %s, personal %s", got, want)
	}
	sharedProv, personalProv := sharedSecrets["provider"].(map[string]any), personalSecrets["provider"].(map[string]any)
	if got, want := strings.Join(wireKeys(sharedProv), ","), strings.Join(wireKeys(personalProv), ","); got != want {
		t.Fatalf("provider keys differ: shared %s, personal %s", got, want)
	}
	for _, k := range wireKeys(sharedProv) {
		if lk := strings.ToLower(k); strings.Contains(lk, "owner") || strings.Contains(lk, "scope") || strings.Contains(lk, "shared") || strings.HasSuffix(lk, "_mode") {
			t.Fatalf("the provider object exposes %q", k)
		}
	}
	if sharedProv["credential"] != plainB || personalProv["credential"] != plainP {
		t.Fatalf("credentials: shared %v personal %v", sharedProv["credential"], personalProv["credential"])
	}
}

func snapshotProviderRuns(t *testing.T, env codexTestEnv, owner uuid.UUID) string {
	t.Helper()
	var snapshot string
	if err := env.pool.QueryRow(env.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY r.id), '[]')::text
		FROM runs r WHERE r.user_id = $1`, owner).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// TestSharedProviderWithoutAKeyIsRefusedWithoutFallbackLiveDB: C selected the shared definition but
// holds no key: C's run creation (issue prediction and chat) is refused key_missing with no vendor
// fallback although C has an Anthropic default. B's key is never usable by C, and C's key never
// appears in B's claim; B losing their own key is refused even while C has one.
func TestSharedProviderWithoutAKeyIsRefusedWithoutFallbackLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	plainB := "gw-key-B-" + uuid.NewString()
	fx.useShared(plainB)
	c := fx.env.seedUser(t)
	fx.env.seedAnthropicSecret(t, c, "c-default-"+uuid.NewString(), true)
	selectClaudeSource(t, fx.env, c, fx.provider)

	if _, err := fx.svc.PredictHarness(fx.env.ctx, c, nil, IssueStartOrigin()); !isUnusable(err, SourceUnusableKeyMissing) {
		t.Fatalf("C predict err = %v, want provider_source_unusable (key_missing)", err)
	}
	before := snapshotProviderRuns(t, fx.env, c)
	if _, err := fx.svc.CreateChatRun(fx.env.ctx, c, "hello"); !isUnusable(err, SourceUnusableKeyMissing) {
		t.Fatalf("C chat err = %v, want provider_source_unusable (key_missing), never a vendor chat", err)
	}
	if snapshotProviderRuns(t, fx.env, c) != before {
		t.Fatal("a refused creation changed C's persisted runs")
	}

	// C attaches their own key: C's chat now binds C's key, never B's.
	plainC := "gw-key-C-" + uuid.NewString()
	keyC := seedSealedKey(t, fx.env, c, fx.provider, plainC)
	chatC, err := fx.svc.CreateChatRun(fx.env.ctx, c, "hello again")
	if err != nil {
		t.Fatalf("C chat with a key: %v", err)
	}
	if got := mustRun(t, fx.env, chatC.ID); uuid.UUID(got.ModelProviderKeySecretID.Bytes) != keyC {
		t.Fatalf("C's chat froze key %v, want C's %s", got.ModelProviderKeySecretID, keyC)
	}

	// B's claim carries only B's key.
	run := mustRun(t, fx.env, mustCreate(t, fx).ID)
	payload, err := fx.assemble(run)
	if err != nil || payload.Secrets.Provider == nil {
		t.Fatalf("B assembleClaim = (%+v, %v)", payload, err)
	}
	raw, _ := json.Marshal(payload)
	if payload.Secrets.Provider.Credential != plainB || strings.Contains(string(raw), plainC) {
		t.Fatalf("B's claim credential = %q or leaks C's key", payload.Secrets.Provider.Credential)
	}

	// B deletes their own key while C still has one: B is refused key_missing, not served C's key.
	fx.env.exec(`DELETE FROM user_secrets WHERE id = $1`, fx.key)
	if _, err := fx.create(nil, nil); !isUnusable(err, SourceUnusableKeyMissing) {
		t.Fatalf("B without a key err = %v, want key_missing", err)
	}
}

// TestSharedProviderRetireKeepsFrozenRunsAndRefusesFreshOnesLiveDB: after the shared definition is
// retired, a run B already froze still claims with its binding (and still spawns an inherited
// successor), a fresh creation is refused as retired with no fallback, and B's selection stays.
func TestSharedProviderRetireKeepsFrozenRunsAndRefusesFreshOnesLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	plainB := "gw-key-B-" + uuid.NewString()
	fx.useShared(plainB)
	run := mustRun(t, fx.env, mustCreate(t, fx).ID)

	if n, err := fx.env.q.RetireSharedModelProvider(fx.env.ctx, fx.provider); err != nil || n != 1 {
		t.Fatalf("retire shared = (%d, %v)", n, err)
	}
	payload, err := fx.assemble(run)
	if err != nil || payload.Secrets.Provider == nil || payload.Secrets.Provider.Credential != plainB {
		t.Fatalf("a frozen run must still claim after retire: (%+v, %v)", payload, err)
	}
	succ, err := fx.successor(run, HarnessClaude)
	if err != nil {
		t.Fatalf("successor of a frozen run after retire: %v", err)
	}
	fx.wantBound(mustRun(t, fx.env, succ.ID), 1, run.ModelProviderKeyRev.Int64)

	before := snapshotProviderRuns(t, fx.env, fx.userID)
	if _, err := fx.create(nil, nil); !isUnusable(err, SourceUnusableRetired) {
		t.Fatalf("fresh create err = %v, want retired, never a vendor run", err)
	}
	if _, err := fx.svc.CreateChatRun(fx.env.ctx, fx.userID, "hi"); !isUnusable(err, SourceUnusableRetired) {
		t.Fatalf("fresh chat err = %v, want retired", err)
	}
	if snapshotProviderRuns(t, fx.env, fx.userID) != before {
		t.Fatal("a refused creation changed persisted runs")
	}
	var selected uuid.UUID
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT claude_provider_id FROM users WHERE id = $1`, fx.userID).Scan(&selected); err != nil || selected != fx.provider {
		t.Fatalf("selection after retire = %v (%v), want it kept", selected, err)
	}
}

// TestSharedProviderChatBindsTheSharedRevisionAndKeyLiveDB: a chat on a shared source freezes the shared
// revision and B's key, its claim carries B's key, and a continuation copies the binding.
func TestSharedProviderChatBindsTheSharedRevisionAndKeyLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	plainB := "gw-key-B-" + uuid.NewString()
	fx.useShared(plainB)

	chat, err := fx.svc.CreateChatRun(fx.env.ctx, fx.userID, "hello shared")
	if err != nil {
		t.Fatalf("chat on a shared source: %v", err)
	}
	got := mustRun(t, fx.env, chat.ID)
	fx.wantBound(got, 1, fx.keyRev())
	fx.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2, claim_generation = 1 WHERE id = $1`, chat.ID, fx.workerID)
	cp, err := fx.svc.assembleChatClaim(fx.env.ctx, fx.providerCapableWorker(), mustRun(t, fx.env, chat.ID))
	if err != nil || cp == nil || cp.Secrets.Provider == nil || cp.Secrets.Provider.Credential != plainB || cp.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("chat claim = (%+v, %v), want B's key and no Anthropic token", cp, err)
	}
	fx.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, chat.ID)
	cont, err := fx.svc.ContinueChat(fx.env.ctx, fx.userID, chat.ID)
	if err != nil {
		t.Fatalf("continue: %v", err)
	}
	fx.wantBound(mustRun(t, fx.env, cont.ID), 1, got.ModelProviderKeyRev.Int64)
}

// sharedPCFix is a claim fixture (a worker advertising claude_provider_v1) whose provider is a SHARED
// definition with B's own sealed key, selected by B.
func newSharedPCFix(t *testing.T) *pcFix {
	t.Helper()
	pf := newPCFix(t)
	provider, _ := seedSharedProvider(t, pf.env, "claude", pvDefaultMap)
	pf.provider = provider
	pf.key = seedSealedKey(t, pf.env, pf.userID, provider, pf.plain)
	selectClaudeSource(t, pf.env, pf.userID, provider)
	return pf
}

// TestSharedProviderClaimAndHoldPromotionLiveDB drives the claim precheck, the finisher's locked
// re-check and the hold promoter over a shared binding: the claim delivers B's key, a de-listed URL
// parks the run on provider_destination_not_allowed, and re-allowlisting promotes it and the next claim
// delivers the binding again.
func TestSharedProviderClaimAndHoldPromotionLiveDB(t *testing.T) {
	pf := newSharedPCFix(t)
	runID, chatID := pf.run(t), pf.chat(t)
	pf.allow(pcOtherURL)
	if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v), want idle", p != nil, err)
	}
	pf.wantParked(t, runID, "provider_destination_not_allowed")
	if p, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t)); err != nil || p != nil {
		t.Fatalf("ClaimChat = (%v, %v), want idle", p != nil, err)
	}
	pf.wantParked(t, chatID, "provider_destination_not_allowed")
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	pf.wantParked(t, runID, "provider_destination_not_allowed")

	pf.allow(pvBase)
	pf.svc.RequestCredentialDisabledPromotion(pf.userID)
	for _, id := range []uuid.UUID{runID, chatID} {
		if r := mustRun(t, pf.env, id); r.Status != "queued" || r.HoldReason.Valid {
			t.Fatalf("run %s after re-allowlisting: status=%s hold=%v, want queued", id, r.Status, r.HoldReason)
		}
	}
	payload, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || payload == nil || payload.Secrets.Provider == nil || payload.Secrets.Provider.Credential != pf.plain || payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatalf("Claim = (%+v, %v), want B's key", payload, err)
	}
	chat, err := pf.svc.ClaimChat(pf.env.ctx, pf.worker(t))
	if err != nil || chat == nil || chat.Secrets.Provider == nil || chat.Secrets.Provider.Credential != pf.plain {
		t.Fatalf("ClaimChat = (%+v, %v), want B's key", chat, err)
	}
	pf.wantNoVendorWrites(t, mustRun(t, pf.env, runID))
	pf.wantNoVendorWrites(t, mustRun(t, pf.env, chatID))
}

// TestSharedProviderDeleteStalesBoundRunsLiveDB: a shared definition cannot be deleted while selected
// (RESTRICT). Once the selection is cleared it can be deleted with B's non-terminal bound run still
// queued; B's keys cascade, and B's next claim refuses with provider_binding_stale, without ever
// reading the key ciphertext.
func TestSharedProviderDeleteStalesBoundRunsLiveDB(t *testing.T) {
	pf := newSharedPCFix(t)
	spy := &keyOpenSpy{Store: pf.svc.q, rd: pf.svc.q.(providerClaimReader)}
	pf.svc.q = spy
	runID := pf.run(t)

	_, err := pf.env.q.DeleteSharedModelProvider(pf.env.ctx, pf.provider)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("delete while selected = %v, want a foreign-key violation", err)
	}
	selectClaudeSource(t, pf.env, pf.userID, uuid.Nil)
	if n, err := pf.env.q.DeleteSharedModelProvider(pf.env.ctx, pf.provider); err != nil || n != 1 {
		t.Fatalf("delete shared = (%d, %v)", n, err)
	}
	var keys int
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT count(*) FROM user_secrets WHERE id = $1`, pf.key).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("B's key after the delete: %d rows (%v), want it cascaded", keys, err)
	}

	if p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v), want idle", p != nil, err)
	}
	pf.wantParked(t, runID, "provider_binding_stale")
	if c := spy.ciphertextReads.Load(); c != 0 {
		t.Fatalf("%d ciphertext read(s) for a binding whose key was deleted", c)
	}
}

// TestRunProviderNamesForSharedAndInaccessibleBindingsLiveDB: ListRunProviderNames (through
// ProviderNamesForRuns) returns the shared definition's name for B's bound run and omits the name for a
// run whose user cannot see the provider: a run owned by B but bound to A's PRIVATE provider, one bound
// to a provider that does not exist, and a vendor run.
func TestRunProviderNamesForSharedAndInaccessibleBindingsLiveDB(t *testing.T) {
	pf := newSharedPCFix(t)
	var sharedName string
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT name FROM model_providers WHERE id = $1`, pf.provider).Scan(&sharedName); err != nil {
		t.Fatal(err)
	}
	bound := pf.run(t)

	a := pf.env.seedUser(t)
	aProvider, aKey := seedPersonalSealed(t, pf.env, a, "gw-key-A-"+uuid.NewString())
	foreign, orphan, vendor := uuid.New(), uuid.New(), uuid.New()
	insertBound := func(id, provider, key uuid.UUID) {
		pf.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, title, trigger_source, harness, status,
		    model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
		    VALUES ($1, $2, 'chat', 'hi', 'hi', 'hi', 'chat', 'claude', 'completed', $3, 1, 'user_key', $4, 1)`, id, pf.userID, provider, key)
	}
	insertBound(foreign, aProvider, aKey)
	insertBound(orphan, uuid.New(), uuid.New())
	pf.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, title, trigger_source, harness, status)
	    VALUES ($1, $2, 'chat', 'hi', 'hi', 'hi', 'chat', 'claude', 'completed')`, vendor, pf.userID)

	names, err := pf.svc.ProviderNamesForRuns(pf.env.ctx, []uuid.UUID{bound, foreign, orphan, vendor})
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[bound] != sharedName {
		t.Fatalf("names = %v, want only the shared definition's name %q for the bound run", names, sharedName)
	}
	if empty, err := pf.svc.ProviderNamesForRuns(pf.env.ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("no runs: (%v, %v)", empty, err)
	}
}

// TestInaccessibleProviderReferencesFailClosedLiveDB: runs carry no foreign key to the provider, so a
// run owned by B can reference A's PRIVATE provider and key (corrupt, or left by a bug). Every
// workersvc path must read that as missing or stale, never decrypting A's key or delivering it:
// selection, freeze lock, inherit, claim, the finisher's locked re-check and hold promotion.
func TestInaccessibleProviderReferencesFailClosedLiveDB(t *testing.T) {
	pf := newSharedPCFix(t)
	spy := &keyOpenSpy{Store: pf.svc.q, rd: pf.svc.q.(providerClaimReader)}
	pf.svc.q = spy
	a := pf.env.seedUser(t)
	aProvider, aKey := seedPersonalSealed(t, pf.env, a, "gw-key-A-"+uuid.NewString())

	runID := uuid.New()
	pf.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	    status, status_since, worker_id, started_at, budget_wall_seconds, session_id, harness,
	    model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
	    VALUES ($1, $2, $3, 'issue', 4242, 't', 'd', 'claimed', now(), $4, now() - interval '15 minutes', 3600, 'sess-x', 'claude',
	            $5, 1, 'user_key', $6, 1)`, runID, pf.userID, pf.repoID, pf.workerID, aProvider, aKey)
	run := mustRun(t, pf.env, runID)
	ctx := pf.env.ctx

	// select: B cannot select A's private provider.
	if _, err := pf.env.q.SetUserProviderSource(ctx, store.SetUserProviderSourceParams{ID: pf.userID, Engine: "claude", ProviderID: pgtype.UUID{Bytes: aProvider, Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("select A's provider err = %v, want no rows", err)
	}
	// freeze: the share lock does not see it.
	tx, err := pf.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err := q.LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: aProvider, OwnerUserID: pf.userID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("freeze lock on A's provider err = %v, want no rows", err)
	}
	// inherit: stale.
	if _, err := pf.svc.inheritProviderBinding(ctx, tx, q, run); !errors.Is(err, ErrProviderBindingStale) {
		t.Fatalf("inherit err = %v, want provider_binding_stale", err)
	}
	// the finisher's locked re-check: stale.
	if reason, err := pf.svc.lockedProviderRefusal(ctx, q, run); err != nil || reason != holdProviderBindingStale {
		t.Fatalf("locked re-check = (%q, %v), want provider_binding_stale", reason, err)
	}
	// hold promotion: stale (the promoter rewrites the hold, never re-queues).
	if reason, err := pf.svc.providerHoldRefusal(ctx, q, run); err != nil || reason != holdProviderBindingStale {
		t.Fatalf("hold promotion = (%q, %v), want provider_binding_stale", reason, err)
	}
	// claim: stale refusal.
	if _, err := pf.svc.assembleProviderBinding(ctx, pf.worker(t), run); !errors.Is(err, ErrProviderBindingStale) {
		t.Fatalf("claim err = %v, want provider_binding_stale", err)
	}
	if c := spy.ciphertextReads.Load(); c != 0 {
		t.Fatalf("%d ciphertext read(s) for an inaccessible reference", c)
	}
}

// TestSharedBindingToAnotherRecipientsKeyIsStaleLiveDB: B and C each attach their own key to one shared
// definition. A run owned by B but frozen to C's key secret id (planted by raw SQL; runs carry no
// foreign key to the key) is a corrupt reference: the claim, inherit, the finisher's locked re-check
// and hold promotion all read it as stale because the key lookup is scoped to the run's owner, so C's
// ciphertext is never read and C's plaintext never appears in anything the claim path returns.
func TestSharedBindingToAnotherRecipientsKeyIsStaleLiveDB(t *testing.T) {
	pf := newSharedPCFix(t)
	spy := &keyOpenSpy{Store: pf.svc.q, rd: pf.svc.q.(providerClaimReader)}
	pf.svc.q = spy
	c := pf.env.seedUser(t)
	plainC := "gw-key-C-" + uuid.NewString()
	keyC := seedSealedKey(t, pf.env, c, pf.provider, plainC)
	var revC int64
	if err := pf.env.pool.QueryRow(pf.env.ctx, `SELECT enablement_rev FROM user_secrets WHERE id = $1`, keyC).Scan(&revC); err != nil {
		t.Fatal(err)
	}

	runID := uuid.New()
	pf.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	    status, status_since, worker_id, started_at, budget_wall_seconds, session_id, harness,
	    model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
	    VALUES ($1, $2, $3, 'issue', 4244, 't', 'd', 'queued', now(), $4, now() - interval '15 minutes', 3600, 'sess-x', 'claude',
	            $5, 1, 'user_key', $6, $7)`, runID, pf.userID, pf.repoID, pf.workerID, pf.provider, keyC, revC)
	run := mustRun(t, pf.env, runID)
	ctx := pf.env.ctx

	_, claimErr := pf.svc.assembleProviderBinding(ctx, pf.worker(t), run)
	var refusal *providerClaimRefusal
	if !errors.As(claimErr, &refusal) || refusal.Reason != holdProviderBindingStale || !errors.Is(claimErr, ErrProviderBindingStale) {
		t.Fatalf("claim err = %v, want provider_binding_stale", claimErr)
	}
	if strings.Contains(claimErr.Error(), plainC) {
		t.Fatalf("the claim refusal leaks C's key: %v", claimErr)
	}

	tx, err := pf.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if _, err := pf.svc.inheritProviderBinding(ctx, tx, q, run); !errors.Is(err, ErrProviderBindingStale) {
		t.Fatalf("inherit err = %v, want provider_binding_stale", err)
	}
	if reason, err := pf.svc.lockedProviderRefusal(ctx, q, run); err != nil || reason != holdProviderBindingStale {
		t.Fatalf("locked re-check = (%q, %v), want provider_binding_stale", reason, err)
	}
	if reason, err := pf.svc.providerHoldRefusal(ctx, q, run); err != nil || reason != holdProviderBindingStale {
		t.Fatalf("hold promotion = (%q, %v), want provider_binding_stale", reason, err)
	}
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal(err)
	}

	// The real claim path parks the run stale and delivers nothing.
	if p, err := pf.svc.Claim(ctx, pf.worker(t), nil); err != nil || p != nil {
		t.Fatalf("Claim = (%+v, %v), want idle", p, err)
	}
	pf.wantParked(t, runID, "provider_binding_stale")
	if c := spy.ciphertextReads.Load(); c != 0 {
		t.Fatalf("%d ciphertext read(s) for a binding to another recipient's key", c)
	}
}

// TestSharedClaudeDefinitionNeverServesACodexRunLiveDB: cross-engine, a shared definition of one engine
// never serves a run of the other. A codex-harness run bound to a shared CLAUDE definition (with B's
// Claude key) is a corrupt reference and refuses stale on the claim and the finisher's re-check, never
// decrypting the key, and that definition is never selectable as the Codex source.
func TestSharedClaudeDefinitionNeverServesACodexRunLiveDB(t *testing.T) {
	pf := newSharedPCFix(t)
	spy := &keyOpenSpy{Store: pf.svc.q, rd: pf.svc.q.(providerClaimReader)}
	pf.svc.q = spy
	runID := uuid.New()
	pf.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	    status, status_since, worker_id, started_at, budget_wall_seconds, session_id, harness,
	    model_provider_id, model_provider_revision, model_provider_key_mode, model_provider_key_secret_id, model_provider_key_rev)
	    VALUES ($1, $2, $3, 'issue', 4243, 't', 'd', 'claimed', now(), $4, now() - interval '15 minutes', 3600, 'sess-x', 'codex',
	            $5, 1, 'user_key', $6, $7)`, runID, pf.userID, pf.repoID, pf.workerID, pf.provider, pf.key, pf.keyRev(t))
	run := mustRun(t, pf.env, runID)
	wkr := pf.worker(t)
	wkr.ProtocolCapabilities = append(wkr.ProtocolCapabilities, capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3)

	var refusal *providerClaimRefusal
	if _, err := pf.svc.assembleProviderBinding(pf.env.ctx, wkr, run); !errors.As(err, &refusal) || refusal.Reason != holdProviderBindingStale {
		t.Fatalf("codex claim over a shared definition err = %v, want provider_binding_stale", err)
	}
	tx, err := pf.env.pool.Begin(pf.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(pf.env.ctx) }()
	if reason, err := pf.svc.lockedProviderRefusal(pf.env.ctx, store.New(tx), run); err != nil || reason != holdProviderBindingStale {
		t.Fatalf("locked re-check = (%q, %v), want provider_binding_stale", reason, err)
	}
	if reason, err := pf.svc.providerHoldRefusal(pf.env.ctx, store.New(tx), run); err != nil || reason != holdProviderBindingStale {
		t.Fatalf("hold promotion = (%q, %v), want provider_binding_stale", reason, err)
	}
	if _, err := pf.svc.inheritProviderBinding(pf.env.ctx, tx, store.New(tx), run); !errors.Is(err, ErrProviderBindingStale) {
		t.Fatalf("inherit err = %v, want provider_binding_stale", err)
	}
	if c := spy.ciphertextReads.Load(); c != 0 {
		t.Fatalf("%d ciphertext read(s) for a codex run on a shared Claude definition", c)
	}
	// A shared Claude definition is never a Codex source (the cross-engine guard).
	if _, err := pf.env.q.SetUserProviderSource(pf.env.ctx, store.SetUserProviderSourceParams{ID: pf.userID, Engine: "codex", ProviderID: pgtype.UUID{Bytes: pf.provider, Valid: true}}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("select a shared definition as the Codex source err = %v, want no rows", err)
	}
}
