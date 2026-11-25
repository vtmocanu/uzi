package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// seedCodexProviderKeyFor seeds a Codex provider (revision 1) with a sealed openai_compatible_key for
// userID and returns the provider and key ids. The key is never a default and never auto-eligible.
func seedCodexProviderKeyFor(t *testing.T, env codexTestEnv, userID uuid.UUID) (provider, key uuid.UUID) {
	t.Helper()
	p, err := env.q.InsertModelProvider(env.ctx, store.InsertModelProviderParams{
		OwnerUserID: userID, Name: "vendor-readers-" + uuid.NewString()[:8], Engine: harnessCodex,
	})
	if err != nil {
		t.Fatalf("insert provider: %v", err)
	}
	if err := env.q.InsertModelProviderRevision(env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase,
		AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"gateway"}}`),
	}); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	sealed, err := env.box.Seal([]byte("inert-provider-key-" + uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	k, err := env.q.InsertProviderKeySecret(env.ctx, store.InsertProviderKeySecretParams{
		UserID: userID, Label: p.ID.String(), Ciphertext: sealed, SealedWith: "master",
		ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("insert provider key: %v", err)
	}
	if k.Kind != store.KindOpenAICompatibleKey {
		t.Fatalf("provider key kind = %q, want %q", k.Kind, store.KindOpenAICompatibleKey)
	}
	return p.ID, k.ID
}

// secretRowJSON is the whole user_secrets row as jsonb text, so "nothing else changed" is one equality.
func secretRowJSON(t *testing.T, env codexTestEnv, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := env.pool.QueryRow(env.ctx, `SELECT to_jsonb(u)::text FROM user_secrets u WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read secret row %s: %v", id, err)
	}
	return s
}

// TestCodexProviderVendorReadersExcludeProviderKeyLiveDB: an owner holds a sealed
// openai_compatible_key bound to a Codex provider AND a usable vendor Codex credential (an
// openai_api_key default). The provider key is never returned or counted by any vendor reader: the
// Codex pool/auto resolver, the weekly reserve, the meter's unmetered reader, the switch-target
// usability rule, CountCodexSecrets, and ClearCodexDefaults (which must leave the key's row
// byte-identical). A second phase removes the vendor credential, so the key stands alone and every
// reader reads empty.
func TestCodexProviderVendorReadersExcludeProviderKeyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, _ := env.seedCodexInfra(t)
	_, providerKey := seedCodexProviderKeyFor(t, env, userID)
	vendor := env.seedStaticAPIKey(t, userID, "vendor-"+uuid.NewString(), "inert-vendor-key")
	makeCodexDefault(t, env, vendor)
	svc := harnessSvc(env)

	keyBefore := secretRowJSON(t, env, providerKey)
	if k := env.mustSecretKind(t, providerKey); k != store.KindOpenAICompatibleKey {
		t.Fatalf("fixture key kind = %q", k)
	}

	// The schema refuses to make a provider key a default or auto-eligible (00515, 00389): the kind
	// filters below are the second line, so the first is asserted directly.
	for _, col := range []string{"is_default", "auto_eligible"} {
		_, err := env.pool.Exec(env.ctx, `UPDATE user_secrets SET `+col+` = true WHERE id = $1`, providerKey)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("setting %s on the provider key: err = %v, want SQLSTATE 23514", col, err)
		}
	}
	if got := secretRowJSON(t, env, providerKey); got != keyBefore {
		t.Fatalf("refused updates changed the provider key row:\n before %s\n after  %s", keyBefore, got)
	}

	// A stray state row for the provider key (nothing in the schema forbids one): every vendor
	// reader must still exclude it by the secret's kind.
	if _, err := env.q.InsertCodexCredentialState(env.ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: providerKey, UserID: userID, Status: "static",
	}); err != nil {
		t.Fatalf("seed stray state row for the provider key: %v", err)
	}

	checkResolver := func(t *testing.T, mode string, wantUsable bool) {
		t.Helper()
		if _, err := env.q.SetUserCodexAccountMode(env.ctx, store.SetUserCodexAccountModeParams{Mode: mode, ID: userID}); err != nil {
			t.Fatalf("set account mode %s: %v", mode, err)
		}
		choice, usable, err := svc.resolveUsableCodexCredential(env.ctx, userID, env.q)
		if err != nil {
			t.Fatalf("resolve (%s): %v", mode, err)
		}
		if usable != wantUsable {
			t.Fatalf("resolve (%s): usable = %v, want %v", mode, usable, wantUsable)
		}
		if choice.SecretID == providerKey {
			t.Fatalf("resolve (%s): the resolver returned the provider key", mode)
		}
		if wantUsable && choice.SecretID != vendor {
			t.Fatalf("resolve (%s): secret = %s, want the vendor credential %s", mode, choice.SecretID, vendor)
		}
		rows, err := env.q.ListCodexPoolAliases(env.ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.AliasID == providerKey {
				t.Fatalf("pool (%s) listed the provider key", mode)
			}
		}
		if _, err := env.q.GetCodexDefaultSelectionCandidate(env.ctx, store.GetCodexDefaultSelectionCandidateParams{UserID: userID, AliasID: providerKey}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("selection candidate (%s) for the provider key: err = %v, want no rows", mode, err)
		}
	}

	checkMeter := func(t *testing.T, wantVendor bool) {
		t.Helper()
		// The meter's unmetered reader, read directly.
		aliases, err := env.q.ListReserveCodexAliases(env.ctx, userID)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if wantVendor {
			want = 1
		}
		if len(aliases) != want {
			t.Fatalf("unmetered reader returned %d aliases, want %d", len(aliases), want)
		}
		for _, a := range aliases {
			if a.AliasID == providerKey {
				t.Fatal("the unmetered reader returned the provider key")
			}
			if a.AliasID != vendor || !a.Unmetered {
				t.Fatalf("unmetered reader row = %+v, want the unmetered vendor key", a)
			}
		}
		// The reserve built over it, with a threshold set so the Codex readers actually run.
		if _, err := env.q.SetUserUsageReserve(env.ctx, store.SetUserUsageReserveParams{ID: userID, WeeklyReservePct: pgtype.Int2{Int16: 80, Valid: true}}); err != nil {
			t.Fatal(err)
		}
		snap, err := svc.reserveSnapshot(env.ctx, userID, time.Now())
		if err != nil || !snap.on() {
			t.Fatalf("reserve snapshot on=%v err=%v", snap.on(), err)
		}
		if _, ok := snap.codex[providerKey]; ok {
			t.Fatal("the reserve holds a verdict for the provider key")
		}
		if snap.codexBlocked(providerKey) {
			t.Fatal("the reserve blocks the provider key")
		}
		if len(snap.codex) != want {
			t.Fatalf("reserve codex verdicts = %d, want %d", len(snap.codex), want)
		}
	}

	checkCounts := func(t *testing.T, want int64) {
		t.Helper()
		n, err := env.q.CountCodexSecrets(env.ctx, userID)
		if err != nil || n != want {
			t.Fatalf("CountCodexSecrets = %d, %v; want %d (the provider key is not counted)", n, err, want)
		}
		n, err = env.q.CountEnabledCodexSecrets(env.ctx, userID)
		if err != nil || n != want {
			t.Fatalf("CountEnabledCodexSecrets = %d, %v; want %d", n, err, want)
		}
	}

	t.Run("switch target rule", func(t *testing.T) {
		// codexSwitchTargetUsable is a pure rule over the target's kind: no status/link combination
		// admits the provider kind, while the vendor kinds keep their rows.
		for _, status := range []string{"static", "linked", "staging", "failed", ""} {
			for _, linked := range []bool{false, true} {
				if codexSwitchTargetUsable(store.KindOpenAICompatibleKey, status, linked) {
					t.Fatalf("provider key kind usable as a switch target (status=%q linked=%v)", status, linked)
				}
			}
		}
		if !codexSwitchTargetUsable(store.KindOpenAIAPIKey, "static", false) {
			t.Fatal("control: a static API key must remain a usable switch target")
		}
	})

	t.Run("vendor and provider key together", func(t *testing.T) {
		checkResolver(t, "default", true)
		checkResolver(t, codexAccountModeAuto, true) // auto with an empty pool falls to the default rule
		checkMeter(t, true)
		checkCounts(t, 1)

		cleared, err := env.q.ClearCodexDefaults(env.ctx, userID)
		if err != nil || cleared != 1 {
			t.Fatalf("ClearCodexDefaults = %d, %v; want exactly the one vendor default", cleared, err)
		}
		if got := secretRowJSON(t, env, providerKey); got != keyBefore {
			t.Fatalf("ClearCodexDefaults touched the provider key row:\n before %s\n after  %s", keyBefore, got)
		}
		var isDefault bool
		if err := env.pool.QueryRow(env.ctx, `SELECT is_default FROM user_secrets WHERE id = $1`, providerKey).Scan(&isDefault); err != nil || isDefault {
			t.Fatalf("provider key is_default = %v, %v; want false", isDefault, err)
		}
		if err := env.pool.QueryRow(env.ctx, `SELECT is_default FROM user_secrets WHERE id = $1`, vendor).Scan(&isDefault); err != nil || isDefault {
			t.Fatalf("vendor is_default after clear = %v, %v; want false", isDefault, err)
		}
	})

	t.Run("provider key alone", func(t *testing.T) {
		env.exec(`DELETE FROM codex_credential_state WHERE user_secret_id = $1`, vendor)
		env.exec(`DELETE FROM user_secrets WHERE id = $1`, vendor)
		checkResolver(t, "default", false)
		checkResolver(t, codexAccountModeAuto, false)
		checkMeter(t, false)
		checkCounts(t, 0)
		if cleared, err := env.q.ClearCodexDefaults(env.ctx, userID); err != nil || cleared != 0 {
			t.Fatalf("ClearCodexDefaults = %d, %v; want 0 with only a provider key", cleared, err)
		}
		if got := secretRowJSON(t, env, providerKey); got != keyBefore {
			t.Fatalf("provider key row changed:\n before %s\n after  %s", keyBefore, got)
		}
	})
}

// mustSecretKind reads user_secrets.kind for id.
func (e codexTestEnv) mustSecretKind(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var k string
	if err := e.pool.QueryRow(e.ctx, `SELECT kind FROM user_secrets WHERE id = $1`, id).Scan(&k); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	return k
}

// TestCodexJudgeBindsProviderSourceOwnerLiveDB: an owner whose Codex source is a model provider
// (users.codex_provider_id set) and who also holds a usable vendor Codex credential gets a Codex
// judge bound to that source (PRD #4574 M1), not a vendor judge and not a skip, through the
// production createRunAtomic path. The same owner without the provider source gets the vendor Codex
// judge (control), so the binding is the source's doing.
func TestCodexJudgeBindsProviderSourceOwnerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, repoID := seedCodexOnlyOriginUser(t, env) // usable vendor Codex default
	svc := judgeEnqueueSvc(env, userID)
	svc.p.ProviderAllowedBaseURLs = []string{pvBase}

	control := seedJudgeTarget(t, env, userID, repoID, "codex", 9801)
	svc.maybeEnqueueJudge(env.ctx, control)
	if _, h, ok := judgeForTarget(t, env, control.ID); !ok || h != string(HarnessCodex) {
		t.Fatalf("control: want a Codex judge for the owner without a provider source, got ok=%v harness=%q", ok, h)
	}

	provider, providerKey := seedCodexProviderKeyFor(t, env, userID)
	env.exec(`UPDATE users SET codex_provider_id = $2, codex_provider_scope_id = id WHERE id = $1`, userID, provider)
	t.Cleanup(func() {
		_, _ = env.pool.Exec(env.ctx, `UPDATE users SET codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id = $1`, userID)
	})

	wantBound := func(t *testing.T, target store.Run) {
		t.Helper()
		id, h, ok := judgeForTarget(t, env, target.ID)
		if !ok || h != string(HarnessCodex) {
			t.Fatalf("want a Codex judge bound to the Codex source, got ok=%v harness=%q", ok, h)
		}
		r := mustRun(t, env, id)
		b := bindingOfRun(r)
		if !b.ProviderID.Valid || uuid.UUID(b.ProviderID.Bytes) != provider || !b.KeySecret.Valid || uuid.UUID(b.KeySecret.Bytes) != providerKey || r.CodexSecretID.Valid {
			t.Fatalf("judge binding = %+v codex secret %v, want provider %s key %s and no vendor credential", b, r.CodexSecretID, provider, providerKey)
		}
		if n := judgeSkipCount(t, env, target.ID); n != 0 {
			t.Fatalf("%d judge skip(s) recorded, want none", n)
		}
	}

	t.Run("codex target", func(t *testing.T) {
		target := seedJudgeTarget(t, env, userID, repoID, "codex", 9802)
		svc.maybeEnqueueJudge(env.ctx, target)
		wantBound(t, target)
	})

	t.Run("claude target judged on codex", func(t *testing.T) {
		setJudgeRunsOn(env, userID, "codex")
		target := seedJudgeTarget(t, env, userID, repoID, "claude", 9803)
		svc.maybeEnqueueJudge(env.ctx, target)
		wantBound(t, target)
	})
}
