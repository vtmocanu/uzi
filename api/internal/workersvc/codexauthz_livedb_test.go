package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests exercise the Codex per-run credential-operation authority check, the
// binding freeze, and the claim-payload credential open end to end against a REAL
// Postgres (PRD #1147 M2, ships DARK). They prove the authority check on the actual
// schema — the frozen-vs-current comparisons, the epoch/capability binding, the
// worker-ownership + actively-claimed gating, and the per-alias / account-wide
// revocation semantics — which the pure-Go unit tests structurally cannot.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. All fixtures use
// fresh UUIDs so a single-process `-run 'LiveDB$'` sweep never collides.

// codexRunFixture bundles a fully-seeded Codex-bound run and the pieces a test breaks.
type codexRunFixture struct {
	userID   uuid.UUID
	workerID uuid.UUID
	runID    uuid.UUID
	aliasID  uuid.UUID
	svc      *Service
	wkr      store.Worker
}

// seedCodexInfra seeds a user, forge connection, repo and worker, returning the ids.
func (e codexTestEnv) seedCodexInfra(t *testing.T) (userID, workerID, repoID uuid.UUID) {
	t.Helper()
	userID = e.seedUser(t)
	connID, repoID := uuid.New(), uuid.New()
	e.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte("x"))
	e.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	        VALUES ($1, $2, 1, $3, $4, 'main', true)`, repoID, connID, "g/"+repoID.String(), "https://forge.e2e/g/"+repoID.String())
	workerID = uuid.New()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		workerID, userID, "w-"+workerID.String(), workerID[:])
	return userID, workerID, repoID
}

// seedCodexRun inserts a run owned by workerID in status 'claimed'.
func (e codexTestEnv) seedCodexRun(t *testing.T, userID, workerID, repoID uuid.UUID) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	        VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'claimed', $4)`, runID, userID, repoID, workerID)
	return runID
}

// seedLinkedSubscription seeds a staging codex_auth alias and reconciles it to a linked
// provider account via the in-process fake identity client, returning the alias id.
func (e codexTestEnv) seedLinkedSubscription(t *testing.T, userID uuid.UUID, label, accessToken string) uuid.UUID {
	t.Helper()
	blob := codexLoginBlob{AccessToken: accessToken, RefreshToken: codexToken("refresh")}
	aliasID := e.seedStagingAlias(t, userID, label, blob)
	fake := newFakeCodexIdentity()
	fake.idByToken[accessToken] = codexauth.Identity{
		ProviderUserID:     "user-" + uuid.NewString(),
		WorkspaceAccountID: "acct-" + uuid.NewString(),
	}
	r := NewCodexReconciler(e.q, nil, e.box, fake)
	if err := r.ReconcileCodexAuthIdentity(e.ctx, userID, aliasID); err != nil {
		t.Fatalf("reconcile subscription alias: %v", err)
	}
	return aliasID
}

// seedStaticAPIKey seeds a static openai_api_key alias with its 'static' state row.
func (e codexTestEnv) seedStaticAPIKey(t *testing.T, userID uuid.UUID, label, key string) uuid.UUID {
	t.Helper()
	sealed, err := e.box.Seal([]byte(key))
	if err != nil {
		t.Fatalf("seal api key: %v", err)
	}
	secretID := uuid.New()
	e.exec(`INSERT INTO user_secrets (id, user_id, kind, label, ciphertext, sealed_with)
	        VALUES ($1, $2, 'openai_api_key', $3, $4, 'master')`, secretID, userID, label, sealed)
	if _, err := e.q.InsertCodexCredentialState(e.ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secretID,
		UserID:       userID,
		Status:       "static",
	}); err != nil {
		t.Fatalf("insert static state: %v", err)
	}
	return secretID
}

// newSubscriptionFixture seeds a fully-valid, claimed, subscription Codex run whose
// binding is frozen at the account's current identity — the accept baseline every
// reject test breaks exactly one thing from.
func newSubscriptionFixture(t *testing.T, env codexTestEnv) codexRunFixture {
	t.Helper()
	userID, workerID, repoID := env.seedCodexInfra(t)
	aliasID := env.seedLinkedSubscription(t, userID, "codex-sub-"+uuid.NewString(), codexToken("access"))
	runID := env.seedCodexRun(t, userID, workerID, repoID)
	svc := &Service{q: env.q, box: env.box}
	if err := svc.FreezeCodexBinding(env.ctx, userID, runID, aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding: %v", err)
	}
	return codexRunFixture{
		userID:   userID,
		workerID: workerID,
		runID:    runID,
		aliasID:  aliasID,
		svc:      svc,
		wkr:      store.Worker{ID: workerID, UserID: userID},
	}
}

// mintCap mints and installs a fresh capability worker-scoped, returning the wire form
// the worker would present. Used by reject tests that do not run the full claim path.
func (e codexTestEnv) mintCap(t *testing.T, runID, workerID uuid.UUID) string {
	t.Helper()
	var epochBefore int64
	if err := e.pool.QueryRow(e.ctx, `SELECT codex_claim_epoch FROM runs WHERE id = $1`, runID).Scan(&epochBefore); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	plaintext, hash := mintCodexCapability()
	n, err := e.q.SetRunCodexClaimCapability(e.ctx, store.SetRunCodexClaimCapabilityParams{
		Hash:     hash,
		ID:       runID,
		WorkerID: pgconv.UUID(workerID),
	})
	if err != nil {
		t.Fatalf("SetRunCodexClaimCapability: %v", err)
	}
	if n != 1 {
		t.Fatalf("SetRunCodexClaimCapability affected %d rows, want 1", n)
	}
	return formatCodexCapability(epochBefore+1, plaintext)
}

func (e codexTestEnv) runEpoch(t *testing.T, runID uuid.UUID) int64 {
	t.Helper()
	var ep int64
	if err := e.pool.QueryRow(e.ctx, `SELECT codex_claim_epoch FROM runs WHERE id = $1`, runID).Scan(&ep); err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	return ep
}

// TestAuthorizeCodexCredentialOpAcceptsValidLiveDB proves a valid (worker + capability +
// epoch + actively-claimed + matching revisions + matching tuple) request is accepted,
// and that the claim path opens the subscription access token (not the refresh blob).
func TestAuthorizeCodexCredentialOpAcceptsValidLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)

	// The claim path mints the capability and extracts the access token.
	run := mustRun(t, env, f.runID)
	codex, err := f.svc.codexClaimSecrets(env.ctx, f.wkr, run)
	if err != nil {
		t.Fatalf("codexClaimSecrets: %v", err)
	}
	if codex.AccessToken == "" {
		t.Fatal("claim must carry a subscription access token")
	}
	if codex.Capability == "" {
		t.Fatal("claim must carry a capability")
	}

	authCtx, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.runID, codex.Capability, ScopeReleaseAccessToken)
	if err != nil {
		t.Fatalf("authorize valid request: %v", err)
	}
	if authCtx.AuthMode != codexAuthModeSubscription {
		t.Fatalf("authMode = %q, want subscription", authCtx.AuthMode)
	}
	if authCtx.AccountID == uuid.Nil {
		t.Fatal("subscription authCtx must resolve an account id")
	}
	if authCtx.Scope != ScopeReleaseAccessToken {
		t.Fatalf("scope = %v, want release", authCtx.Scope)
	}

	// Scope independence on a subscription run: all three scopes authorize, but each is
	// requested and reported independently (a release authCtx does not carry refresh).
	for _, sc := range []CodexOpScope{ScopePersistRecovery, ScopeStartRefresh} {
		cap2 := env.mintCap(t, f.runID, f.workerID)
		ac, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.runID, cap2, sc)
		if err != nil {
			t.Fatalf("authorize scope %v: %v", sc, err)
		}
		if ac.Scope != sc {
			t.Fatalf("authCtx.Scope = %v, want %v", ac.Scope, sc)
		}
	}
}

// TestAuthorizeCodexCredentialOpRejectsLiveDB drives each rejection and asserts its
// specific sentinel.
func TestAuthorizeCodexCredentialOpRejectsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	t.Run("wrong capability", func(t *testing.T) {
		f := newSubscriptionFixture(t, env)
		env.mintCap(t, f.runID, f.workerID) // install a valid one, then present a bad secret at the right epoch
		bad := formatCodexCapability(env.runEpoch(t, f.runID), "wrong-secret")
		assertAuthErr(t, f, env, bad, ScopeReleaseAccessToken, ErrCodexCapabilityMismatch)
	})

	t.Run("prior epoch", func(t *testing.T) {
		f := newSubscriptionFixture(t, env)
		cap1 := env.mintCap(t, f.runID, f.workerID)
		// Simulate a requeue: bump the epoch and clear the hash (the runtime paths do
		// both on ownership loss). The old capability is now from a prior epoch.
		env.exec(`UPDATE runs SET codex_claim_epoch = codex_claim_epoch + 1, codex_cap_hash = NULL WHERE id = $1`, f.runID)
		assertAuthErr(t, f, env, cap1, ScopeReleaseAccessToken, ErrCodexCapabilityEpoch)
	})

	t.Run("wrong worker", func(t *testing.T) {
		f := newSubscriptionFixture(t, env)
		cap1 := env.mintCap(t, f.runID, f.workerID)
		other := store.Worker{ID: uuid.New(), UserID: f.userID}
		_, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, other, f.runID, cap1, ScopeReleaseAccessToken)
		if !errors.Is(err, ErrCodexWorkerMismatch) {
			t.Fatalf("want ErrCodexWorkerMismatch, got %v", err)
		}
	})

	t.Run("queued requeue gap", func(t *testing.T) {
		f := newSubscriptionFixture(t, env)
		cap1 := env.mintCap(t, f.runID, f.workerID)
		env.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, f.runID)
		assertAuthErr(t, f, env, cap1, ScopeReleaseAccessToken, ErrCodexRunNotActivelyClaimed)
	})

	t.Run("terminal", func(t *testing.T) {
		f := newSubscriptionFixture(t, env)
		cap1 := env.mintCap(t, f.runID, f.workerID)
		env.exec(`UPDATE runs SET status = 'completed' WHERE id = $1`, f.runID)
		assertAuthErr(t, f, env, cap1, ScopeReleaseAccessToken, ErrCodexRunNotActivelyClaimed)
	})

	t.Run("stale material revision", func(t *testing.T) {
		f := newSubscriptionFixture(t, env)
		cap1 := env.mintCap(t, f.runID, f.workerID)
		// A manual alias replace bumps material_revision (and drops the account link).
		if _, err := env.q.BumpCodexMaterialRevision(env.ctx, store.BumpCodexMaterialRevisionParams{
			Status:       "staging",
			UserSecretID: f.aliasID,
			UserID:       f.userID,
		}); err != nil {
			t.Fatalf("bump material: %v", err)
		}
		assertAuthErr(t, f, env, cap1, ScopeReleaseAccessToken, ErrCodexMaterialRevisionStale)
	})

	t.Run("stale account revision", func(t *testing.T) {
		f := newSubscriptionFixture(t, env)
		cap1 := env.mintCap(t, f.runID, f.workerID)
		env.exec(`UPDATE codex_provider_account SET credential_revision = credential_revision + 1
		          WHERE id = (SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1)`, f.aliasID)
		assertAuthErr(t, f, env, cap1, ScopeReleaseAccessToken, ErrCodexAccountRevisionStale)
	})

	t.Run("tuple mismatch", func(t *testing.T) {
		f := newSubscriptionFixture(t, env)
		cap1 := env.mintCap(t, f.runID, f.workerID)
		env.exec(`UPDATE codex_provider_account SET provider_user_id = 'retargeted-' || provider_user_id
		          WHERE id = (SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id = $1)`, f.aliasID)
		assertAuthErr(t, f, env, cap1, ScopeReleaseAccessToken, ErrCodexAccountTupleMismatch)
	})

	t.Run("null-frozen account key not runnable", func(t *testing.T) {
		// A staging (unlinked) subscription alias freezes with a NULL account key.
		userID, workerID, repoID := env.seedCodexInfra(t)
		blob := codexLoginBlob{AccessToken: codexToken("access"), RefreshToken: codexToken("refresh")}
		aliasID := env.seedStagingAlias(t, userID, "codex-staging-"+uuid.NewString(), blob)
		runID := env.seedCodexRun(t, userID, workerID, repoID)
		svc := &Service{q: env.q, box: env.box}
		if err := svc.FreezeCodexBinding(env.ctx, userID, runID, aliasID, codexAuthModeSubscription); err != nil {
			t.Fatalf("FreezeCodexBinding: %v", err)
		}
		f := codexRunFixture{userID: userID, workerID: workerID, runID: runID, aliasID: aliasID, svc: svc, wkr: store.Worker{ID: workerID, UserID: userID}}
		cap1 := env.mintCap(t, runID, workerID)
		assertAuthErr(t, f, env, cap1, ScopeReleaseAccessToken, ErrCodexAccountKeyUnfrozen)
	})

	t.Run("not codex-bound", func(t *testing.T) {
		userID, workerID, repoID := env.seedCodexInfra(t)
		runID := env.seedCodexRun(t, userID, workerID, repoID) // no codex binding frozen
		svc := &Service{q: env.q, box: env.box}
		wkr := store.Worker{ID: workerID, UserID: userID}
		_, err := svc.AuthorizeCodexCredentialOp(env.ctx, wkr, runID, "1.whatever", ScopeReleaseAccessToken)
		if !errors.Is(err, ErrCodexRunNotBound) {
			t.Fatalf("want ErrCodexRunNotBound, got %v", err)
		}
	})
}

// TestAuthorizeCodexScopeNotApplicableAPIKeyLiveDB proves scope independence on the
// wire: an api_key run can RELEASE its token but cannot START-REFRESH (a static key is
// not refreshable), so one scope's authorization never grants another.
func TestAuthorizeCodexScopeNotApplicableAPIKeyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	aliasID := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), codexToken("sk"))
	runID := env.seedCodexRun(t, userID, workerID, repoID)
	svc := &Service{q: env.q, box: env.box}
	if err := svc.FreezeCodexBinding(env.ctx, userID, runID, aliasID, codexAuthModeAPIKey); err != nil {
		t.Fatalf("FreezeCodexBinding: %v", err)
	}
	wkr := store.Worker{ID: workerID, UserID: userID}
	run := mustRun(t, env, runID)

	// The claim path opens the static key.
	codex, err := svc.codexClaimSecrets(env.ctx, wkr, run)
	if err != nil {
		t.Fatalf("codexClaimSecrets (api_key): %v", err)
	}
	if codex.AccessToken == "" {
		t.Fatal("api_key claim must carry the static key")
	}

	// Release is authorized.
	if _, err := svc.AuthorizeCodexCredentialOp(env.ctx, wkr, runID, codex.Capability, ScopeReleaseAccessToken); err != nil {
		t.Fatalf("release on api_key must be authorized, got %v", err)
	}
	// Start-refresh is NOT — release authorization did not grant it.
	cap2 := env.mintCap(t, runID, workerID)
	if _, err := svc.AuthorizeCodexCredentialOp(env.ctx, wkr, runID, cap2, ScopeStartRefresh); !errors.Is(err, ErrCodexScopeNotApplicable) {
		t.Fatalf("start-refresh on api_key: want ErrCodexScopeNotApplicable, got %v", err)
	}
}

// assertAuthErr mints nothing itself; it authorizes with the given capability/scope and
// asserts the specific sentinel.
func assertAuthErr(t *testing.T, f codexRunFixture, env codexTestEnv, capability string, scope CodexOpScope, want error) {
	t.Helper()
	_, err := f.svc.AuthorizeCodexCredentialOp(env.ctx, f.wkr, f.runID, capability, scope)
	if !errors.Is(err, want) {
		t.Fatalf("want %v, got %v", want, err)
	}
}

// mustRun reads a run row by id.
func mustRun(t *testing.T, env codexTestEnv, runID uuid.UUID) store.Run {
	t.Helper()
	run, err := env.q.GetRunByID(env.ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	return run
}
