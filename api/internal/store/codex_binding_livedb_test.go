package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codexRunFixture creates a forge_connection + repo + worker for `user` and returns
// the repo id and worker id, so a Codex-binding test can INSERT a run that satisfies
// the runs FKs. Suffixes are the fresh uuid so a persistent DB never collides across
// a single-process LiveDB sweep.
func codexRunFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user uuid.UUID) (repoID, workerID uuid.UUID) {
	t.Helper()
	connID := uuid.New()
	repoID = uuid.New()
	workerID = uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, user, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/cb', 'main', true)`, repoID, connID, "g/cb-"+uuid.NewString())
	// token_hash carries the worker UUID bytes so a re-run never collides on
	// workers_token_hash_key (Migrate does not truncate).
	mustExec(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, workerID, user, workerID[:])
	return repoID, workerID
}

// insertCodexRun creates a run owned by `user`, on issue `iid` (distinct per call so
// the one-active-run-per-issue index never collides), in `status`, optionally assigned
// to `worker` (pass uuid.Nil for none), and returns its id. It writes issue_description
// = marker so a regression test can prove an unrelated column survived a requeue.
func insertCodexRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user, repo, worker uuid.UUID, iid int64, status, marker string) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	if worker == uuid.Nil {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, 'issue', $4, 'do x', $5, $6)`, runID, user, repo, iid, marker, status)
	} else {
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
			 VALUES ($1, $2, $3, 'issue', $4, 'do x', $5, $6, $7)`, runID, user, repo, iid, marker, status, worker)
	}
	return runID
}

// TestRunCodexBindingSchemaLiveDB pins 00201: the codex_* columns exist with
// codex_claim_epoch defaulting to 0, and the composite-FK SET NULL (codex_secret_id)
// nulls only the binding when a bound secret is deleted — never runs.user_id.
func TestRunCodexBindingSchemaLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, _ := codexRunFixture(ctx, t, pool, user)

	secret, err := insertSecret(ctx, pool, user, store.KindOpenAIAPIKey, "api-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert secret: %v", err)
	}
	runID := insertCodexRun(ctx, t, pool, user, repo, uuid.Nil, 101, "queued", "marker")

	// Fresh run: the epoch defaults to 0 and every other codex column is NULL/empty.
	fresh, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if fresh.CodexClaimEpoch != 0 {
		t.Fatalf("codex_claim_epoch default = %d, want 0", fresh.CodexClaimEpoch)
	}
	if fresh.CodexSecretID.Valid || fresh.CodexCapHash != nil {
		t.Fatalf("fresh run carries a codex binding: secret_valid=%v cap_hash=%v", fresh.CodexSecretID.Valid, fresh.CodexCapHash)
	}

	// Bind the run to the secret, then delete the secret and prove SET NULL nulls
	// ONLY codex_secret_id — runs.user_id (NOT NULL) survives, so the delete does not
	// error and the run keeps its owner.
	if n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secret, AuthMode: "api_key", SecretLabel: "api", MaterialRevision: 0,
		ID: runID, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("FreezeRunCodexBinding = (%d,%v), want (1,nil)", n, err)
	}
	mustExec(ctx, t, pool, `DELETE FROM user_secrets WHERE id = $1`, secret)

	after, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID after delete: %v", err)
	}
	if after.CodexSecretID.Valid {
		t.Fatal("codex_secret_id is still set after the bound secret was deleted — SET NULL did not fire")
	}
	if after.UserID != user {
		t.Fatalf("runs.user_id = %s after secret delete, want it untouched (%s) — the composite SET NULL nulled the owner", after.UserID, user)
	}
}

// TestGetRunCodexAuthContextLiveDB pins the authority-check read: it returns the run's
// FROZEN binding alongside the CURRENT alias/account state, the provider-account join is
// LEFT (an api_key alias has no account, so current account fields come back NULL), and
// a subscription alias linked to an account resolves the current generation + tuple.
func TestGetRunCodexAuthContextLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, _ := codexRunFixture(ctx, t, pool, user)

	// --- api_key path: a static openai_api_key alias, no provider account behind it. ---
	apiSecret, err := insertSecret(ctx, pool, user, store.KindOpenAIAPIKey, "api-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert api secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: apiSecret, UserID: user, Status: "static",
	}); err != nil {
		t.Fatalf("insert static state: %v", err)
	}
	apiRun := insertCodexRun(ctx, t, pool, user, repo, uuid.Nil, 201, "queued", "marker")
	if _, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: apiSecret, AuthMode: "api_key", SecretLabel: "api", MaterialRevision: 2,
		ID: apiRun, UserID: user,
	}); err != nil {
		t.Fatalf("freeze api binding: %v", err)
	}
	apiCtx, err := q.GetRunCodexAuthContext(ctx, apiRun)
	if err != nil {
		t.Fatalf("GetRunCodexAuthContext(api): %v", err)
	}
	if apiCtx.CodexAuthMode.String != "api_key" || apiCtx.CodexMaterialRevision.Int64 != 2 {
		t.Fatalf("api ctx frozen = (mode=%q mat=%+v), want (api_key, 2)", apiCtx.CodexAuthMode.String, apiCtx.CodexMaterialRevision)
	}
	if apiCtx.CurrentMaterialRevision != 0 {
		t.Fatalf("api ctx current_material_revision = %d, want 0 (the state default)", apiCtx.CurrentMaterialRevision)
	}
	if apiCtx.CurrentGeneration.Valid || apiCtx.ProviderUserID.Valid {
		t.Fatalf("api ctx account fields = (gen_valid=%v provider_valid=%v), want NULL — the LEFT join has no account for an api_key alias",
			apiCtx.CurrentGeneration.Valid, apiCtx.ProviderUserID.Valid)
	}

	// --- subscription path: a codex_auth alias linked to a provider account. ---
	subSecret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "codex-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert codex secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: subSecret, UserID: user, Status: "staging",
	}); err != nil {
		t.Fatalf("insert staging state: %v", err)
	}
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "provider-x", WorkspaceAccountID: "workspace-y",
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: subSecret, UserID: user, ProviderAccountID: pgtype.UUID{Bytes: account.ID, Valid: true},
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	subRun := insertCodexRun(ctx, t, pool, user, repo, uuid.Nil, 202, "queued", "marker")
	// Freeze at a DIFFERENT material revision than the alias's current (0) so the read
	// visibly separates run-frozen from current.
	if _, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: subSecret, AuthMode: "subscription", SecretLabel: "codex", MaterialRevision: 5,
		ID: subRun, UserID: user,
	}); err != nil {
		t.Fatalf("freeze sub binding: %v", err)
	}
	subCtx, err := q.GetRunCodexAuthContext(ctx, subRun)
	if err != nil {
		t.Fatalf("GetRunCodexAuthContext(sub): %v", err)
	}
	if subCtx.CodexMaterialRevision.Int64 != 5 || subCtx.CurrentMaterialRevision != 0 {
		t.Fatalf("sub ctx = (frozen_mat=%+v current_mat=%d), want (5, 0)", subCtx.CodexMaterialRevision, subCtx.CurrentMaterialRevision)
	}
	if !subCtx.CurrentGeneration.Valid || subCtx.CurrentGeneration.Int64 != account.Generation ||
		subCtx.ProviderUserID.String != "provider-x" || subCtx.WorkspaceAccountID.String != "workspace-y" {
		t.Fatalf("sub ctx account = (gen=%+v provider=%q workspace=%q), want (gen=%d, provider-x, workspace-y)",
			subCtx.CurrentGeneration, subCtx.ProviderUserID.String, subCtx.WorkspaceAccountID.String, account.Generation)
	}
}

// TestGetRunCodexAuthContextHardeningLiveDB pins the two additive columns the security
// audit added (PRD #1147): current_coord_state and bound_kind. An api_key alias has no
// account, so current_coord_state comes back NULL and bound_kind is 'openai_api_key'; a
// subscription alias linked to an account reports the account's live coord_state
// ('idle', then 'quarantined' after the account is parked) and bound_kind 'codex_auth'.
func TestGetRunCodexAuthContextHardeningLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, _ := codexRunFixture(ctx, t, pool, user)

	// --- api_key: no provider account, so current_coord_state is NULL. ---
	apiSecret, err := insertSecret(ctx, pool, user, store.KindOpenAIAPIKey, "api-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert api secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: apiSecret, UserID: user, Status: "static",
	}); err != nil {
		t.Fatalf("insert static state: %v", err)
	}
	apiRun := insertCodexRun(ctx, t, pool, user, repo, uuid.Nil, 211, "queued", "marker")
	if _, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: apiSecret, AuthMode: "api_key", SecretLabel: "api", MaterialRevision: 0,
		ID: apiRun, UserID: user,
	}); err != nil {
		t.Fatalf("freeze api binding: %v", err)
	}
	apiCtx, err := q.GetRunCodexAuthContext(ctx, apiRun)
	if err != nil {
		t.Fatalf("GetRunCodexAuthContext(api): %v", err)
	}
	if apiCtx.BoundKind != store.KindOpenAIAPIKey {
		t.Fatalf("api bound_kind = %q, want %q", apiCtx.BoundKind, store.KindOpenAIAPIKey)
	}
	if apiCtx.CurrentCoordState.Valid {
		t.Fatalf("api current_coord_state = %+v, want NULL — an api_key alias has no account", apiCtx.CurrentCoordState)
	}

	// --- subscription: linked to an account, so current_coord_state tracks it. ---
	subSecret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "codex-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert codex secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: subSecret, UserID: user, Status: "staging",
	}); err != nil {
		t.Fatalf("insert staging state: %v", err)
	}
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		UserSecretID: subSecret, UserID: user,
		ProviderAccountID: pgtype.UUID{Bytes: account.ID, Valid: true}, MaterialRevision: 0,
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	subRun := insertCodexRun(ctx, t, pool, user, repo, uuid.Nil, 212, "queued", "marker")
	if _, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: subSecret, AuthMode: "subscription", SecretLabel: "codex", MaterialRevision: 0,
		ID: subRun, UserID: user,
	}); err != nil {
		t.Fatalf("freeze sub binding: %v", err)
	}
	subCtx, err := q.GetRunCodexAuthContext(ctx, subRun)
	if err != nil {
		t.Fatalf("GetRunCodexAuthContext(sub): %v", err)
	}
	if subCtx.BoundKind != store.KindCodexAuth {
		t.Fatalf("sub bound_kind = %q, want %q", subCtx.BoundKind, store.KindCodexAuth)
	}
	if !subCtx.CurrentCoordState.Valid || subCtx.CurrentCoordState.String != "idle" {
		t.Fatalf("sub current_coord_state = %+v, want a valid \"idle\"", subCtx.CurrentCoordState)
	}

	// Park the account: the read now reports 'quarantined', which the release predicate
	// keys off to refuse a run authority over an ambiguous account.
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account SET coord_state = 'quarantined' WHERE id = $1 AND user_id = $2`,
		account.ID, user)
	quarCtx, err := q.GetRunCodexAuthContext(ctx, subRun)
	if err != nil {
		t.Fatalf("GetRunCodexAuthContext(sub, quarantined): %v", err)
	}
	if !quarCtx.CurrentCoordState.Valid || quarCtx.CurrentCoordState.String != "quarantined" {
		t.Fatalf("sub current_coord_state = %+v after quarantine, want \"quarantined\"", quarCtx.CurrentCoordState)
	}
}

// TestFreezeRunCodexBindingLiveDB pins the two freeze primitives: they write the
// binding/identity fields and are owner-scoped, so a foreign user_id moves 0 rows.
func TestFreezeRunCodexBindingLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, _ := codexRunFixture(ctx, t, pool, user)
	secret, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "codex-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert secret: %v", err)
	}
	runID := insertCodexRun(ctx, t, pool, user, repo, uuid.Nil, 102, "queued", "marker")

	// Freeze the binding for a subscription run WITHOUT the account identity yet
	// (nullable args left unset), exactly as an at-creation freeze does.
	n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secret, AuthMode: "subscription", SecretLabel: "codex-label", MaterialRevision: 7,
		ID: runID, UserID: user,
	})
	if err != nil || n != 1 {
		t.Fatalf("FreezeRunCodexBinding = (%d,%v), want (1,nil)", n, err)
	}
	bound, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if !bound.CodexSecretID.Valid || uuid.UUID(bound.CodexSecretID.Bytes) != secret ||
		bound.CodexAuthMode.String != "subscription" || bound.CodexSecretLabel.String != "codex-label" ||
		!bound.CodexMaterialRevision.Valid || bound.CodexMaterialRevision.Int64 != 7 {
		t.Fatalf("bound run = (secret_valid=%v mode=%q label=%q mat=%+v), want the frozen binding",
			bound.CodexSecretID.Valid, bound.CodexAuthMode.String, bound.CodexSecretLabel.String, bound.CodexMaterialRevision)
	}
	if bound.CodexAccountKey.Valid || bound.CodexAccountRevision.Valid {
		t.Fatalf("account identity was set at freeze (key_valid=%v rev_valid=%v), want NULL until first link",
			bound.CodexAccountKey.Valid, bound.CodexAccountRevision.Valid)
	}

	// Owner-scoping: a foreign user_id freezes nothing.
	if n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secret, AuthMode: "subscription", SecretLabel: "x", MaterialRevision: 1,
		ID: runID, UserID: uuid.New(),
	}); err != nil || n != 0 {
		t.Fatalf("FreezeRunCodexBinding(foreign user) = (%d,%v), want (0,nil)", n, err)
	}

	// SetRunCodexFrozenIdentity freezes the identity tuple + account revision at link.
	// The account key is the JSON-array serialization documented in 00201 (Postgres
	// TEXT cannot hold a NUL byte, so a NUL-separated join is not storable).
	key := `["provider-x","workspace-y"]`
	if n, err := q.SetRunCodexFrozenIdentity(ctx, store.SetRunCodexFrozenIdentityParams{
		Key: key, Rev: 3, ID: runID, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("SetRunCodexFrozenIdentity = (%d,%v), want (1,nil)", n, err)
	}
	frozen, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if frozen.CodexAccountKey.String != key || !frozen.CodexAccountRevision.Valid || frozen.CodexAccountRevision.Int64 != 3 {
		t.Fatalf("frozen identity = (key=%q rev=%+v), want (%q, 3)", frozen.CodexAccountKey.String, frozen.CodexAccountRevision, key)
	}
	// Owner-scoping on the identity freeze too.
	if n, err := q.SetRunCodexFrozenIdentity(ctx, store.SetRunCodexFrozenIdentityParams{
		Key: "other", Rev: 9, ID: runID, UserID: uuid.New(),
	}); err != nil || n != 0 {
		t.Fatalf("SetRunCodexFrozenIdentity(foreign user) = (%d,%v), want (0,nil)", n, err)
	}
}

// TestSetRunCodexClaimCapabilityLiveDB pins the capability mint (owning-worker only,
// bumps the epoch) and the requeue revocation across all THREE requeue queries,
// including the regression that a non-codex run's other columns survive a requeue.
func TestSetRunCodexClaimCapabilityLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, worker := codexRunFixture(ctx, t, pool, user)
	pgUUID := func(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

	// A worker-owned, claimed run.
	runID := insertCodexRun(ctx, t, pool, user, repo, worker, 103, "claimed", "marker")

	// A non-owning worker cannot mint. The query is now :one RETURNING codex_claim_epoch,
	// so a non-matching WHERE (foreign worker) returns pgx.ErrNoRows rather than 0 rows.
	if epoch, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash: []byte("cap"), ID: runID, WorkerID: pgUUID(uuid.New()),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("SetRunCodexClaimCapability(non-owner) = (%d,%v), want (_, pgx.ErrNoRows)", epoch, err)
	}
	// The owning worker mints: hash set, epoch 0 -> 1. The RETURNING value is the
	// PERSISTED post-bump epoch, so it must come back as 1.
	if epoch, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash: []byte("cap"), ID: runID, WorkerID: pgUUID(worker),
	}); err != nil || epoch != 1 {
		t.Fatalf("SetRunCodexClaimCapability(owner) = (%d,%v), want (1,nil)", epoch, err)
	}
	minted, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if string(minted.CodexCapHash) != "cap" || minted.CodexClaimEpoch != 1 {
		t.Fatalf("after mint: cap_hash=%q epoch=%d, want (\"cap\", 1)", minted.CodexCapHash, minted.CodexClaimEpoch)
	}

	// --- RequeueClaimedRunToQueued revokes: cap_hash NULL, epoch bumped. ---
	if n, err := q.RequeueClaimedRunToQueued(ctx, runID); err != nil || n != 1 {
		t.Fatalf("RequeueClaimedRunToQueued = (%d,%v), want (1,nil)", n, err)
	}
	revoked, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if revoked.CodexCapHash != nil {
		t.Fatalf("RequeueClaimedRunToQueued left cap_hash=%q, want NULL", revoked.CodexCapHash)
	}
	if revoked.CodexClaimEpoch != 2 {
		t.Fatalf("RequeueClaimedRunToQueued left epoch=%d, want 2", revoked.CodexClaimEpoch)
	}

	// --- Status guard: minting on a requeued ('queued') run is refused even by the
	// still-recorded owner. RequeueClaimedRunToQueued left worker_id intact (resume
	// affinity), so the worker_id predicate alone would still match; the status IN (...)
	// guard is what now rejects the re-mint, surfacing as pgx.ErrNoRows. This closes the
	// requeue TOCTOU where a departed worker could re-mint a fresh capability. ---
	if epoch, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash: []byte("cap-after-requeue"), ID: runID, WorkerID: pgUUID(worker),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("SetRunCodexClaimCapability(queued run) = (%d,%v), want (_, pgx.ErrNoRows)", epoch, err)
	}

	// --- RequeueWorkerRuns revokes a codex run AND leaves a non-codex run intact. ---
	// Re-mint on a running codex run, and add a non-codex control run with a marker.
	mustExec(ctx, t, pool, `UPDATE runs SET status='running' WHERE id=$1`, runID)
	if _, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash: []byte("cap2"), ID: runID, WorkerID: pgUUID(worker),
	}); err != nil {
		t.Fatalf("re-mint: %v", err)
	}
	ctrlID := insertCodexRun(ctx, t, pool, user, repo, worker, 104, "running", "control-marker")
	ctrlBefore, err := q.GetRunByID(ctx, ctrlID)
	if err != nil {
		t.Fatalf("GetRunByID(ctrl): %v", err)
	}

	if n, err := q.RequeueWorkerRuns(ctx, store.RequeueWorkerRunsParams{
		WorkerID: pgUUID(worker), MaxRequeues: 100,
	}); err != nil || n != 2 {
		t.Fatalf("RequeueWorkerRuns = (%d,%v), want (2,nil)", n, err)
	}
	codexAfter, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID(codex): %v", err)
	}
	// Epoch progression: 0 -> 1 (mint) -> 2 (claimed requeue) -> 3 (re-mint) -> 4 (this requeue).
	if codexAfter.CodexCapHash != nil || codexAfter.CodexClaimEpoch != 4 {
		t.Fatalf("after RequeueWorkerRuns codex run: cap_hash=%q epoch=%d, want (NULL, 4)", codexAfter.CodexCapHash, codexAfter.CodexClaimEpoch)
	}
	ctrlAfter, err := q.GetRunByID(ctx, ctrlID)
	if err != nil {
		t.Fatalf("GetRunByID(ctrl after): %v", err)
	}
	// Regression: the non-codex run's unrelated columns are untouched; its cap_hash
	// stays NULL and its epoch bumps harmlessly 0 -> 1 (the no-op revocation).
	if ctrlAfter.IssueDescription != ctrlBefore.IssueDescription || ctrlAfter.CodexSecretID.Valid {
		t.Fatalf("non-codex run mutated by requeue: desc=%q secret_valid=%v", ctrlAfter.IssueDescription, ctrlAfter.CodexSecretID.Valid)
	}
	if ctrlAfter.CodexCapHash != nil || ctrlAfter.CodexClaimEpoch != 1 {
		t.Fatalf("non-codex run after requeue: cap_hash=%q epoch=%d, want (NULL, 1)", ctrlAfter.CodexCapHash, ctrlAfter.CodexClaimEpoch)
	}

	// --- RequeueRunsOfStaleWorkers revokes too. ---
	// Make the worker stale (old heartbeat) and put the codex run back to running.
	mustExec(ctx, t, pool, `UPDATE runs SET status='running', requeue_count=0 WHERE id=$1`, runID)
	if _, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash: []byte("cap3"), ID: runID, WorkerID: pgUUID(worker),
	}); err != nil {
		t.Fatalf("re-mint before stale requeue: %v", err)
	}
	mustExec(ctx, t, pool, `UPDATE workers SET last_heartbeat_at = now() - interval '1 hour' WHERE id=$1`, worker)
	epochBefore, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID before stale: %v", err)
	}
	if _, err := q.RequeueRunsOfStaleWorkers(ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 100, Cutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	staleAfter, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID after stale requeue: %v", err)
	}
	if staleAfter.CodexCapHash != nil || staleAfter.CodexClaimEpoch != epochBefore.CodexClaimEpoch+1 {
		t.Fatalf("after RequeueRunsOfStaleWorkers: cap_hash=%q epoch=%d, want (NULL, %d)",
			staleAfter.CodexCapHash, staleAfter.CodexClaimEpoch, epochBefore.CodexClaimEpoch+1)
	}
}

// TestCodexRefreshCoordinationLiveDB pins the CAS coordination primitives on
// codex_provider_account: lease acquire/re-acquire, expired-lease quarantine, the
// generation-CAS commit (win and stale-loss), and the committed->idle reset.
func TestCodexRefreshCoordinationLiveDB(t *testing.T) {
	ctx, _, q, user := codexLiveDB(t)

	mkAccount := func() store.CodexProviderAccount {
		acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
			UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
			SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert account: %v", err)
		}
		return acc
	}
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	past := pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}

	// --- Lease acquire from idle, then a re-acquire is blocked by the live lease. ---
	acc := mkAccount()
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(idle) = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 0 {
		t.Fatalf("AcquireCodexRefreshLease(in_progress) = (%d,%v), want (0,nil) — a live lease must block", n, err)
	}

	// --- Generation guard (PRD #1147 M4): an acquire whose presented from_generation does
	//     NOT match the account's current generation fails, even from an acquirable state.
	//     A fresh idle account is at gen 0, so presenting gen 1 wins 0 rows; gen 0 wins. ---
	genAcc := mkAccount()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: future, ID: genAcc.ID, UserID: user, FromGeneration: 1,
	}); err != nil || n != 0 {
		t.Fatalf("AcquireCodexRefreshLease(stale from_generation) = (%d,%v), want (0,nil) — a moved generation must block the acquire", n, err)
	}
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: future, ID: genAcc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(matching from_generation) = (%d,%v), want (1,nil)", n, err)
	}

	// --- QuarantineExpiredCodexLease: not-expired is a no-op; expired flips it. ---
	if n, err := q.QuarantineExpiredCodexLease(ctx, store.QuarantineExpiredCodexLeaseParams{
		ID: acc.ID, UserID: user, Now: now,
	}); err != nil || n != 0 {
		t.Fatalf("QuarantineExpiredCodexLease(not expired) = (%d,%v), want (0,nil)", n, err)
	}
	expAcc := mkAccount()
	if _, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: past, ID: expAcc.ID, UserID: user, FromGeneration: 0,
	}); err != nil {
		t.Fatalf("acquire expired-lease account: %v", err)
	}
	if n, err := q.QuarantineExpiredCodexLease(ctx, store.QuarantineExpiredCodexLeaseParams{
		ID: expAcc.ID, UserID: user, Now: now,
	}); err != nil || n != 1 {
		t.Fatalf("QuarantineExpiredCodexLease(expired) = (%d,%v), want (1,nil)", n, err)
	}
	if got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: expAcc.ID}); err != nil {
		t.Fatalf("read quarantined: %v", err)
	} else if got.CoordState != "quarantined" {
		t.Fatalf("coord_state = %q after quarantine, want \"quarantined\"", got.CoordState)
	}

	// --- CommitCodexRefresh CAS: wins at the started-from generation (0), advancing
	//     to 1; a second commit from 0 loses (ErrNoRows) because the generation moved. ---
	committed, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed: []byte("new-sealed"), SealedWith: store.SealedWithMaster, Op: op,
		ID: acc.ID, UserID: user, FromGeneration: 0,
	})
	if err != nil {
		t.Fatalf("CommitCodexRefresh(gen 0): %v", err)
	}
	if committed.Generation != 1 || committed.CoordState != "committed" ||
		!committed.CommittedGeneration.Valid || committed.CommittedGeneration.Int64 != 1 {
		t.Fatalf("commit result = (gen=%d state=%q committed_gen=%+v), want (1, committed, 1)",
			committed.Generation, committed.CoordState, committed.CommittedGeneration)
	}
	if _, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed: []byte("stale"), SealedWith: store.SealedWithMaster, Op: op,
		ID: acc.ID, UserID: user, FromGeneration: 0,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale CommitCodexRefresh returned %v, want pgx.ErrNoRows (generation moved)", err)
	}

	// --- ResetCodexCoordIdle returns a committed account to idle. ---
	if n, err := q.ResetCodexCoordIdle(ctx, store.ResetCodexCoordIdleParams{ID: acc.ID, UserID: user}); err != nil || n != 1 {
		t.Fatalf("ResetCodexCoordIdle = (%d,%v), want (1,nil)", n, err)
	}
	if got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: acc.ID}); err != nil {
		t.Fatalf("read reset: %v", err)
	} else if got.CoordState != "idle" || got.CoordOperationID.Valid || got.LeaseDeadline.Valid {
		t.Fatalf("after reset = (state=%q op_valid=%v deadline_valid=%v), want (idle, false, false)",
			got.CoordState, got.CoordOperationID.Valid, got.LeaseDeadline.Valid)
	}
}

// TestCodexRefreshIntentLiveDB pins 00200 + the intent primitives: insert (born
// rotating), get, set-state, a duplicate operation_id insert fails (23505), and the
// unresolved scan returns only 'rotating' intents.
func TestCodexRefreshIntentLiveDB(t *testing.T) {
	ctx, _, q, user := codexLiveDB(t)

	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}

	op1 := uuid.New()
	intent, err := q.InsertCodexRefreshIntent(ctx, store.InsertCodexRefreshIntentParams{
		OperationID: op1, UserID: user, ProviderAccountID: acc.ID, FromGeneration: 0,
	})
	if err != nil {
		t.Fatalf("InsertCodexRefreshIntent: %v", err)
	}
	if intent.State != "rotating" {
		t.Fatalf("new intent state = %q, want \"rotating\"", intent.State)
	}

	// Get, owner-scoped.
	if got, err := q.GetCodexRefreshIntent(ctx, store.GetCodexRefreshIntentParams{OperationID: op1, UserID: user}); err != nil {
		t.Fatalf("GetCodexRefreshIntent: %v", err)
	} else if got.OperationID != op1 {
		t.Fatalf("got operation %s, want %s", got.OperationID, op1)
	}

	// Duplicate operation_id insert fails on the PK (idempotence guard).
	if _, err := q.InsertCodexRefreshIntent(ctx, store.InsertCodexRefreshIntentParams{
		OperationID: op1, UserID: user, ProviderAccountID: acc.ID, FromGeneration: 0,
	}); pgCode(err) != "23505" {
		t.Fatalf("duplicate operation_id returned %v (code %q), want a unique_violation (23505)", err, pgCode(err))
	}

	// A second intent, then move op1 out of 'rotating': the unresolved scan returns
	// only the still-rotating one.
	op2 := uuid.New()
	if _, err := q.InsertCodexRefreshIntent(ctx, store.InsertCodexRefreshIntentParams{
		OperationID: op2, UserID: user, ProviderAccountID: acc.ID, FromGeneration: 1,
	}); err != nil {
		t.Fatalf("InsertCodexRefreshIntent(op2): %v", err)
	}
	if n, err := q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{
		State: "committed", OperationID: op1, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRefreshIntentState = (%d,%v), want (1,nil)", n, err)
	}
	unresolved, err := q.ListUnresolvedCodexRefreshIntents(ctx, store.ListUnresolvedCodexRefreshIntentsParams{
		UserID: user, ProviderAccountID: acc.ID,
	})
	if err != nil {
		t.Fatalf("ListUnresolvedCodexRefreshIntents: %v", err)
	}
	if len(unresolved) != 1 || unresolved[0].OperationID != op2 {
		got := make([]uuid.UUID, len(unresolved))
		for i, u := range unresolved {
			got[i] = u.OperationID
		}
		t.Fatalf("unresolved intents = %v, want exactly [%s] (only the rotating one)", got, op2)
	}

	// Owner-scoping on set-state: a foreign user moves 0 rows.
	if n, err := q.SetCodexRefreshIntentState(ctx, store.SetCodexRefreshIntentStateParams{
		State: "reconciled", OperationID: op2, UserID: uuid.New(),
	}); err != nil || n != 0 {
		t.Fatalf("SetCodexRefreshIntentState(foreign user) = (%d,%v), want (0,nil)", n, err)
	}
}

// TestSweepClaimedNeverStartedCodexRevocationLiveDB pins the sweeper's claimed→queued
// path (PRD #1147 M2): sweeping a claimed run back to queued must ALSO revoke its
// per-claim Codex capability — clear codex_cap_hash and bump codex_claim_epoch — so a
// run swept off a lost claim cannot replay a live cap on its next claim. A non-codex
// claimed run swept in the same pass keeps its unrelated columns (regression) and its
// already-NULL cap_hash, with only the harmless epoch bump.
func TestSweepClaimedNeverStartedCodexRevocationLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, worker := codexRunFixture(ctx, t, pool, user)

	// The sweep is global (status='claimed' AND claimed_at < cutoff, not owner-scoped),
	// so key everything off a claimed_at far enough in the past that only our rows fall
	// under our cutoff regardless of what else lives in a persistent DB.
	old := pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Hour), Valid: true}
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true}

	// A codex-bound claimed run: non-null cap_hash, a known epoch, claimed old.
	codexRun := insertCodexRun(ctx, t, pool, user, repo, worker, 301, "claimed", "codex-marker")
	mustExec(ctx, t, pool,
		`UPDATE runs SET codex_cap_hash = $1, codex_claim_epoch = 5, claimed_at = $2 WHERE id = $3`,
		[]byte("live-cap"), old, codexRun)

	// A non-codex claimed run in the SAME sweep: no cap_hash, a marker to prove its
	// unrelated columns survive.
	plainRun := insertCodexRun(ctx, t, pool, user, repo, worker, 302, "claimed", "plain-marker")
	mustExec(ctx, t, pool, `UPDATE runs SET claimed_at = $1 WHERE id = $2`, old, plainRun)
	plainBefore, err := q.GetRunByID(ctx, plainRun)
	if err != nil {
		t.Fatalf("GetRunByID(plain before): %v", err)
	}

	if _, err := q.SweepClaimedNeverStarted(ctx, cutoff); err != nil {
		t.Fatalf("SweepClaimedNeverStarted: %v", err)
	}

	// Codex run: queued, cap_hash revoked (NULL), epoch bumped 5 -> 6.
	codexAfter, err := q.GetRunByID(ctx, codexRun)
	if err != nil {
		t.Fatalf("GetRunByID(codex after): %v", err)
	}
	if codexAfter.Status != "queued" {
		t.Fatalf("codex run status = %q after sweep, want \"queued\"", codexAfter.Status)
	}
	if codexAfter.CodexCapHash != nil {
		t.Fatalf("codex run cap_hash = %q after sweep, want NULL (revoked)", codexAfter.CodexCapHash)
	}
	if codexAfter.CodexClaimEpoch != 6 {
		t.Fatalf("codex run epoch = %d after sweep, want 6 (5 -> 6)", codexAfter.CodexClaimEpoch)
	}

	// Non-codex run: queued, unrelated columns untouched, cap_hash stays NULL, epoch
	// bumps harmlessly 0 -> 1 (the no-op revocation).
	plainAfter, err := q.GetRunByID(ctx, plainRun)
	if err != nil {
		t.Fatalf("GetRunByID(plain after): %v", err)
	}
	if plainAfter.Status != "queued" {
		t.Fatalf("plain run status = %q after sweep, want \"queued\"", plainAfter.Status)
	}
	if plainAfter.IssueDescription != plainBefore.IssueDescription || plainAfter.CodexSecretID.Valid {
		t.Fatalf("non-codex run mutated by sweep: desc=%q secret_valid=%v", plainAfter.IssueDescription, plainAfter.CodexSecretID.Valid)
	}
	if plainAfter.CodexCapHash != nil || plainAfter.CodexClaimEpoch != 1 {
		t.Fatalf("non-codex run after sweep: cap_hash=%q epoch=%d, want (NULL, 1)", plainAfter.CodexCapHash, plainAfter.CodexClaimEpoch)
	}
}

// TestCommitCodexRefreshLeaseGuardLiveDB pins the lease guard the M2 review added to
// CommitCodexRefresh (coord_state='in_progress' AND coord_operation_id=@op): a commit
// at the unchanged generation is REFUSED unless the presenting operation still owns a
// live in_progress lease. It covers (b) the quarantine-bypass block — a quarantined
// lease-holder cannot commit at generation N, and the quarantine + recovery slot stay
// intact — and (c) the wrong-op block, while confirming a genuine in_progress owner
// still commits (advancing the generation, flipping to 'committed', clearing recovery).
func TestCommitCodexRefreshLeaseGuardLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	mkAccount := func() store.CodexProviderAccount {
		acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
			UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
			SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
		})
		if err != nil {
			t.Fatalf("insert account: %v", err)
		}
		return acc
	}
	readAcc := func(id uuid.UUID) store.CodexProviderAccount {
		got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: id})
		if err != nil {
			t.Fatalf("GetCodexProviderAccountByID: %v", err)
		}
		return got
	}

	// --- (b) quarantine-bypass: a quarantined lease-holder cannot commit at gen N. ---
	// Acquire the lease as op (in_progress at gen 0), then quarantine via
	// SetCodexRecoverySlot, which also parks a recovery slot. A stale/presumed-dead
	// refresher then tries to commit at the unchanged generation with the original op.
	qAcc := mkAccount()
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: qAcc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(quarantine case) = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		Sealed: []byte("recovery-login"), Gen: 0, ID: qAcc.ID, UserID: user,
		Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRecoverySlot = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed: []byte("bypass"), SealedWith: store.SealedWithMaster, Op: op,
		ID: qAcc.ID, UserID: user, FromGeneration: 0,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CommitCodexRefresh(quarantined, gen 0) returned %v, want pgx.ErrNoRows — quarantine must not be bypassable", err)
	}
	// The account is untouched: still quarantined at gen 0, recovery slot intact.
	if q1 := readAcc(qAcc.ID); q1.CoordState != "quarantined" || q1.Generation != 0 ||
		string(q1.RecoverySealed) != "recovery-login" || !q1.RecoveryGeneration.Valid || q1.RecoveryGeneration.Int64 != 0 {
		t.Fatalf("after refused commit: state=%q gen=%d recovery=%q recovery_gen=%+v, want (quarantined, 0, recovery-login, 0)",
			q1.CoordState, q1.Generation, q1.RecoverySealed, q1.RecoveryGeneration)
	}

	// --- (c) wrong-op: only the lease OWNER commits, even at the right generation. ---
	wAcc := mkAccount()
	opA := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: opA, Deadline: future, ID: wAcc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(wrong-op case) = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed: []byte("intruder"), SealedWith: store.SealedWithMaster, Op: uuid.New(),
		ID: wAcc.ID, UserID: user, FromGeneration: 0,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CommitCodexRefresh(wrong op, gen 0) returned %v, want pgx.ErrNoRows — only the lease owner commits", err)
	}
	// Still owned by opA, in_progress, unadvanced.
	if w1 := readAcc(wAcc.ID); w1.CoordState != "in_progress" || w1.Generation != 0 ||
		!w1.CoordOperationID.Valid || uuid.UUID(w1.CoordOperationID.Bytes) != opA {
		t.Fatalf("after wrong-op commit: state=%q gen=%d op=%+v, want (in_progress, 0, %s)",
			w1.CoordState, w1.Generation, w1.CoordOperationID, opA)
	}

	// --- contrast: the genuine in_progress owner commits and the recovery slot clears. ---
	gAcc := mkAccount()
	opG := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: opG, Deadline: future, ID: gAcc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(genuine case) = (%d,%v), want (1,nil)", n, err)
	}
	// Park a recovery slot directly WITHOUT flipping coord_state (SetCodexRecoverySlot
	// would quarantine), so we can prove a successful commit clears it. recovery_sealed_with
	// is set alongside recovery_sealed to satisfy 00198's CHECK pairing.
	mustExec(ctx, t, pool,
		`UPDATE codex_provider_account SET recovery_sealed = $1, recovery_generation = 0, recovery_sealed_with = 'master' WHERE id = $2 AND user_id = $3`,
		[]byte("stale-recovery"), gAcc.ID, user)
	committed, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed: []byte("fresh-login"), SealedWith: store.SealedWithMaster, Op: opG,
		ID: gAcc.ID, UserID: user, FromGeneration: 0,
	})
	if err != nil {
		t.Fatalf("CommitCodexRefresh(genuine owner, gen 0): %v", err)
	}
	if committed.Generation != 1 || committed.CoordState != "committed" ||
		!committed.CommittedGeneration.Valid || committed.CommittedGeneration.Int64 != 1 {
		t.Fatalf("genuine commit result = (gen=%d state=%q committed_gen=%+v), want (1, committed, 1)",
			committed.Generation, committed.CoordState, committed.CommittedGeneration)
	}
	if g1 := readAcc(gAcc.ID); g1.RecoverySealed != nil || g1.RecoveryGeneration.Valid {
		t.Fatalf("after genuine commit recovery slot = (sealed=%q gen=%+v), want cleared (NULL, NULL)",
			g1.RecoverySealed, g1.RecoveryGeneration)
	}
}

// TestSetCodexRecoverySlotLiveDB pins SetCodexRecoverySlot (PRD #1147 M2): from an
// in_progress account it writes recovery_sealed/recovery_generation and flips
// coord_state to 'quarantined', parking the account for reconciliation.
func TestSetCodexRecoverySlotLiveDB(t *testing.T) {
	ctx, _, q, user := codexLiveDB(t)
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}

	// SetCodexRecoverySlot now requires the live-lease owner's op and the key that sealed
	// the recovery blob (non-empty 'master'|'dek', per 00198's CHECK pairing).
	if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		Sealed: []byte("prior-good-login"), Gen: 3, ID: acc.ID, UserID: user,
		Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRecoverySlot = (%d,%v), want (1,nil)", n, err)
	}
	got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: acc.ID})
	if err != nil {
		t.Fatalf("GetCodexProviderAccountByID: %v", err)
	}
	if string(got.RecoverySealed) != "prior-good-login" || !got.RecoveryGeneration.Valid ||
		got.RecoveryGeneration.Int64 != 3 || got.CoordState != "quarantined" ||
		!got.RecoverySealedWith.Valid || got.RecoverySealedWith.String != store.SealedWithMaster {
		t.Fatalf("after SetCodexRecoverySlot = (sealed=%q gen=%+v state=%q sealed_with=%+v), want (prior-good-login, 3, quarantined, master)",
			got.RecoverySealed, got.RecoveryGeneration, got.CoordState, got.RecoverySealedWith)
	}

	// Owner-scoping: a foreign user_id sets nothing (the op/sealed_with are valid, so this
	// isolates the user_id predicate).
	if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		Sealed: []byte("x"), Gen: 9, ID: acc.ID, UserID: uuid.New(),
		Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 0 {
		t.Fatalf("SetCodexRecoverySlot(foreign user) = (%d,%v), want (0,nil)", n, err)
	}
}

// TestAcquireCodexRefreshLeaseFromCommittedLiveDB pins the committed→in_progress source
// of AcquireCodexRefreshLease (PRD #1147 M2): an account left 'committed' after a prior
// refresh cycle can be re-acquired for the next one, complementing the idle→in_progress
// case already covered in TestCodexRefreshCoordinationLiveDB.
func TestAcquireCodexRefreshLeaseFromCommittedLiveDB(t *testing.T) {
	ctx, _, q, user := codexLiveDB(t)
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	// Drive it to 'committed' via a genuine acquire+commit cycle.
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(idle) = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := q.CommitCodexRefresh(ctx, store.CommitCodexRefreshParams{
		Sealed: []byte("committed-login"), SealedWith: store.SealedWithMaster, Op: op,
		ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil {
		t.Fatalf("CommitCodexRefresh: %v", err)
	}
	if got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: acc.ID}); err != nil {
		t.Fatalf("read committed: %v", err)
	} else if got.CoordState != "committed" {
		t.Fatalf("coord_state = %q before re-acquire, want \"committed\"", got.CoordState)
	}

	// The commit advanced the account to generation 1. The generation guard (PRD #1147 M4)
	// means a re-acquire that still presents the pre-commit from_generation (0) is refused
	// even from the acquirable 'committed' state — the stale loser cannot win.
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 0 {
		t.Fatalf("AcquireCodexRefreshLease(committed, stale from_generation 0) = (%d,%v), want (0,nil) — the moved generation must block", n, err)
	}

	// Re-acquire from 'committed' for the next cycle, presenting the CURRENT generation (1).
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 1,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(committed) = (%d,%v), want (1,nil) — a committed account is re-acquirable at its current generation", n, err)
	}
	if got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: acc.ID}); err != nil {
		t.Fatalf("read re-acquired: %v", err)
	} else if got.CoordState != "in_progress" {
		t.Fatalf("coord_state = %q after re-acquire, want \"in_progress\"", got.CoordState)
	}
}

// TestFreezeRunCodexBindingWriteOnceLiveDB pins the write-once freeze guards the security
// audit added (PRD #1147): once a run's binding/identity is frozen, an identical retry is
// idempotent (1 row) but a CONFLICTING re-freeze with a different secret/identity affects
// 0 rows and cannot silently re-point the run.
func TestFreezeRunCodexBindingWriteOnceLiveDB(t *testing.T) {
	ctx, pool, q, user := codexLiveDB(t)
	repo, _ := codexRunFixture(ctx, t, pool, user)

	secretA, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "a-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert secretA: %v", err)
	}
	secretB, err := insertSecret(ctx, pool, user, store.KindCodexAuth, "b-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert secretB: %v", err)
	}
	runID := insertCodexRun(ctx, t, pool, user, repo, uuid.Nil, 401, "queued", "marker")

	// First freeze (from NULL) binds secretA.
	if n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secretA, AuthMode: "subscription", SecretLabel: "a", MaterialRevision: 1,
		ID: runID, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("FreezeRunCodexBinding(first) = (%d,%v), want (1,nil)", n, err)
	}
	// Identical retry (same secret) is idempotent — still affects the row.
	if n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secretA, AuthMode: "subscription", SecretLabel: "a", MaterialRevision: 1,
		ID: runID, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("FreezeRunCodexBinding(identical retry) = (%d,%v), want (1,nil) — must be idempotent", n, err)
	}
	// Conflicting re-freeze with a DIFFERENT secret affects 0 rows and leaves secretA bound.
	if n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secretB, AuthMode: "subscription", SecretLabel: "b", MaterialRevision: 9,
		ID: runID, UserID: user,
	}); err != nil || n != 0 {
		t.Fatalf("FreezeRunCodexBinding(conflicting secret) = (%d,%v), want (0,nil) — the binding is write-once", n, err)
	}
	bound, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if uuid.UUID(bound.CodexSecretID.Bytes) != secretA || bound.CodexSecretLabel.String != "a" {
		t.Fatalf("after conflicting freeze the binding = (secret=%s label=%q), want it unchanged at secretA/a",
			uuid.UUID(bound.CodexSecretID.Bytes), bound.CodexSecretLabel.String)
	}

	// SetRunCodexFrozenIdentity: same write-once pattern on the identity tuple.
	keyA := `["provider-a","workspace-a"]`
	keyB := `["provider-b","workspace-b"]`
	if n, err := q.SetRunCodexFrozenIdentity(ctx, store.SetRunCodexFrozenIdentityParams{
		Key: keyA, Rev: 1, ID: runID, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("SetRunCodexFrozenIdentity(first) = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.SetRunCodexFrozenIdentity(ctx, store.SetRunCodexFrozenIdentityParams{
		Key: keyA, Rev: 1, ID: runID, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("SetRunCodexFrozenIdentity(identical retry) = (%d,%v), want (1,nil) — must be idempotent", n, err)
	}
	if n, err := q.SetRunCodexFrozenIdentity(ctx, store.SetRunCodexFrozenIdentityParams{
		Key: keyB, Rev: 9, ID: runID, UserID: user,
	}); err != nil || n != 0 {
		t.Fatalf("SetRunCodexFrozenIdentity(conflicting tuple) = (%d,%v), want (0,nil) — the identity is write-once", n, err)
	}
	frozen, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if frozen.CodexAccountKey.String != keyA {
		t.Fatalf("after conflicting identity freeze key = %q, want it unchanged at %q", frozen.CodexAccountKey.String, keyA)
	}

	// --- F5: FreezeRunCodexBinding's COALESCE keeps an already-frozen account identity
	// through a replay that carries NULL account fields (a stale binding re-delivery must
	// not clobber the identity that a later freeze/link pinned). Freeze a SEPARATE run WITH
	// a non-null account_key/revision, then replay the freeze for the same run+secret with
	// NULL account fields and assert the account identity is UNCHANGED.
	idRun := insertCodexRun(ctx, t, pool, user, repo, uuid.Nil, 402, "queued", "marker")
	acctKey := `["provider-id","workspace-id"]`
	if n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secretA, AuthMode: "subscription", SecretLabel: "a", MaterialRevision: 1,
		AccountKey:      pgtype.Text{String: acctKey, Valid: true},
		AccountRevision: pgtype.Int8{Int64: 4, Valid: true},
		ID:              idRun, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("FreezeRunCodexBinding(with identity) = (%d,%v), want (1,nil)", n, err)
	}
	// Replay for the SAME run+secret with NULL account fields (nargs unset): the row is
	// still affected (write-once secret guard is satisfied by the equal secret) but the
	// COALESCE keeps the prior non-null account identity rather than nulling it.
	if n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secretA, AuthMode: "subscription", SecretLabel: "a", MaterialRevision: 1,
		ID: idRun, UserID: user,
	}); err != nil || n != 1 {
		t.Fatalf("FreezeRunCodexBinding(replay NULL account) = (%d,%v), want (1,nil)", n, err)
	}
	afterReplay, err := q.GetRunByID(ctx, idRun)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if afterReplay.CodexAccountKey.String != acctKey ||
		!afterReplay.CodexAccountRevision.Valid || afterReplay.CodexAccountRevision.Int64 != 4 {
		t.Fatalf("after NULL-account replay identity = (key=%q rev=%+v), want it preserved at (%q, 4) via COALESCE",
			afterReplay.CodexAccountKey.String, afterReplay.CodexAccountRevision, acctKey)
	}
	// A replay with a DIFFERENT secret is rejected by the write-once secret guard (0 rows).
	if n, err := q.FreezeRunCodexBinding(ctx, store.FreezeRunCodexBindingParams{
		SecretID: secretB, AuthMode: "subscription", SecretLabel: "b", MaterialRevision: 2,
		ID: idRun, UserID: user,
	}); err != nil || n != 0 {
		t.Fatalf("FreezeRunCodexBinding(different secret) = (%d,%v), want (0,nil) — write-once", n, err)
	}
}

// mkCodexAccount inserts a fresh provider account for `user` (gen 0, idle) and returns it.
func mkCodexAccount(ctx context.Context, t *testing.T, q *store.Queries, user uuid.UUID) store.CodexProviderAccount {
	t.Helper()
	acc, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "p-" + uuid.NewString(), WorkspaceAccountID: "w-" + uuid.NewString(),
		SealedLogin: []byte("sealed"), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return acc
}

// TestPromoteCodexRecoveryLiveDB pins PromoteCodexRecovery (PRD #1147 audit): it installs
// the protected recovery material as the live login on a quarantined account whose
// recovery_generation matches, advancing the generation and returning to idle; a second
// call is a no-op (ErrNoRows, already promoted) and a wrong from_generation is refused.
func TestPromoteCodexRecoveryLiveDB(t *testing.T) {
	ctx, _, q, user := codexLiveDB(t)
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	// Quarantine an account with a recovery slot at generation 0.
	acc := mkCodexAccount(ctx, t, q, user)
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		Sealed: []byte("recovery-bytes"), Gen: 0, ID: acc.ID, UserID: user,
		Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRecoverySlot = (%d,%v), want (1,nil)", n, err)
	}

	gen, err := q.PromoteCodexRecovery(ctx, store.PromoteCodexRecoveryParams{
		ID: acc.ID, UserID: user, FromGeneration: 0,
	})
	if err != nil {
		t.Fatalf("PromoteCodexRecovery: %v", err)
	}
	if gen != 1 {
		t.Fatalf("PromoteCodexRecovery returned generation %d, want 1", gen)
	}
	promoted, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: acc.ID})
	if err != nil {
		t.Fatalf("read promoted: %v", err)
	}
	if string(promoted.SealedLogin) != "recovery-bytes" {
		t.Fatalf("sealed_login = %q after promote, want the recovery bytes", promoted.SealedLogin)
	}
	if promoted.Generation != 1 || promoted.CoordState != "idle" ||
		!promoted.CommittedGeneration.Valid || promoted.CommittedGeneration.Int64 != 1 {
		t.Fatalf("after promote = (gen=%d state=%q committed_gen=%+v), want (1, idle, 1)",
			promoted.Generation, promoted.CoordState, promoted.CommittedGeneration)
	}
	if promoted.RecoverySealed != nil || promoted.RecoveryGeneration.Valid ||
		promoted.CoordOperationID.Valid || promoted.LeaseDeadline.Valid {
		t.Fatalf("after promote the recovery/coord slots = (recovery=%q recovery_gen=%+v op_valid=%v deadline_valid=%v), want all cleared",
			promoted.RecoverySealed, promoted.RecoveryGeneration, promoted.CoordOperationID.Valid, promoted.LeaseDeadline.Valid)
	}

	// Idempotent: a second promote finds coord_state='idle' (not quarantined) → ErrNoRows.
	if _, err := q.PromoteCodexRecovery(ctx, store.PromoteCodexRecoveryParams{
		ID: acc.ID, UserID: user, FromGeneration: 1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second PromoteCodexRecovery returned %v, want pgx.ErrNoRows — the promotion is idempotent", err)
	}

	// Wrong from_generation: a quarantined account whose recovery_generation is 0 refuses
	// a promote presenting a different from_generation.
	wrongAcc := mkCodexAccount(ctx, t, q, user)
	wrongOp := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: wrongOp, Deadline: future, ID: wrongAcc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(wrong-gen) = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		Sealed: []byte("recovery-bytes"), Gen: 0, ID: wrongAcc.ID, UserID: user,
		Op: wrongOp, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRecoverySlot(wrong-gen) = (%d,%v), want (1,nil)", n, err)
	}
	if _, err := q.PromoteCodexRecovery(ctx, store.PromoteCodexRecoveryParams{
		ID: wrongAcc.ID, UserID: user, FromGeneration: 5,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("PromoteCodexRecovery(wrong from_generation) returned %v, want pgx.ErrNoRows", err)
	}
}

// TestRefreshCodexAccountLoginLiveDB pins RefreshCodexAccountLogin (PRD #1147 audit): a
// verified re-login installs a fresh sealed login, advances the generation, returns the
// account to idle, and clears the recovery/quarantine slots.
func TestRefreshCodexAccountLoginLiveDB(t *testing.T) {
	ctx, _, q, user := codexLiveDB(t)
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	// Start from a quarantined account carrying a recovery slot, to prove Refresh clears it.
	acc := mkCodexAccount(ctx, t, q, user)
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		Sealed: []byte("stale-recovery"), Gen: 0, ID: acc.ID, UserID: user,
		Op: op, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRecoverySlot = (%d,%v), want (1,nil)", n, err)
	}

	// Fixture is quarantined at generation 0 (SetCodexRecoverySlot sets coord_state without
	// bumping generation), so the CAS restore must present FromGeneration: 0 to match.
	if n, err := q.RefreshCodexAccountLogin(ctx, store.RefreshCodexAccountLoginParams{
		Sealed: []byte("fresh-login"), SealedWith: store.SealedWithDEK, ID: acc.ID, UserID: user,
		FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("RefreshCodexAccountLogin = (%d,%v), want (1,nil)", n, err)
	}
	got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: acc.ID})
	if err != nil {
		t.Fatalf("read refreshed: %v", err)
	}
	if string(got.SealedLogin) != "fresh-login" || got.SealedWith != store.SealedWithDEK {
		t.Fatalf("after refresh login = (sealed=%q with=%q), want (fresh-login, %q)", got.SealedLogin, got.SealedWith, store.SealedWithDEK)
	}
	if got.Generation != 1 || got.CoordState != "idle" ||
		!got.CommittedGeneration.Valid || got.CommittedGeneration.Int64 != 1 {
		t.Fatalf("after refresh = (gen=%d state=%q committed_gen=%+v), want (1, idle, 1)",
			got.Generation, got.CoordState, got.CommittedGeneration)
	}
	// recovery_sealed_with MUST be cleared alongside recovery_sealed: a populated recovery
	// slot (here sealed under 'master') would otherwise violate 00198's CHECK pairing
	// ((recovery_sealed IS NULL) = (recovery_sealed_with IS NULL)) on this install (23514).
	if got.RecoverySealed != nil || got.RecoveryGeneration.Valid || got.RecoverySealedWith.Valid ||
		got.CoordOperationID.Valid || got.LeaseDeadline.Valid {
		t.Fatalf("after refresh recovery/coord slots not cleared: recovery=%q recovery_gen=%+v recovery_with=%+v op_valid=%v deadline_valid=%v",
			got.RecoverySealed, got.RecoveryGeneration, got.RecoverySealedWith, got.CoordOperationID.Valid, got.LeaseDeadline.Valid)
	}

	// Stale-generation CAS: a fresh quarantined account at generation 0 refuses a restore
	// presenting the WRONG expected generation (the quarantine moved under the caller). This
	// isolates the generation half of the CAS: the account IS still quarantined, so a 0-row
	// result can only be the generation mismatch.
	stale := mkCodexAccount(ctx, t, q, user)
	staleOp := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: staleOp, Deadline: future, ID: stale.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(stale) = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		Sealed: []byte("stale-recovery"), Gen: 0, ID: stale.ID, UserID: user,
		Op: staleOp, RecoverySealedWith: pgtype.Text{String: store.SealedWithMaster, Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("SetCodexRecoverySlot(stale) = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.RefreshCodexAccountLogin(ctx, store.RefreshCodexAccountLoginParams{
		Sealed: []byte("y"), SealedWith: store.SealedWithDEK, ID: stale.ID, UserID: user,
		FromGeneration: 99,
	}); err != nil || n != 0 {
		t.Fatalf("RefreshCodexAccountLogin(stale generation) = (%d,%v), want (0,nil)", n, err)
	}

	// Owner-scoping: a foreign user refreshes nothing. Target `stale`, which is STILL
	// quarantined at generation 0 (the stale-generation call above matched 0 rows and
	// changed nothing), and present the MATCHING FromGeneration: 0 so both the
	// coord_state='quarantined' guard and the generation CAS pass — the foreign user_id is
	// then the SOLE reason the update rejects, isolating the ownership predicate.
	if n, err := q.RefreshCodexAccountLogin(ctx, store.RefreshCodexAccountLoginParams{
		Sealed: []byte("x"), SealedWith: store.SealedWithMaster, ID: stale.ID, UserID: uuid.New(),
		FromGeneration: 0,
	}); err != nil || n != 0 {
		t.Fatalf("RefreshCodexAccountLogin(foreign user) = (%d,%v), want (0,nil)", n, err)
	}
	// The foreign call must leave `stale` untouched: same login, generation, and coord_state.
	staleGot, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: stale.ID})
	if err != nil {
		t.Fatalf("read stale after foreign refresh: %v", err)
	}
	if string(staleGot.SealedLogin) != "sealed" || staleGot.SealedWith != store.SealedWithMaster ||
		staleGot.Generation != 0 || staleGot.CoordState != "quarantined" {
		t.Fatalf("foreign refresh mutated stale account = (sealed=%q with=%q gen=%d state=%q), want (sealed, %q, 0, quarantined)",
			staleGot.SealedLogin, staleGot.SealedWith, staleGot.Generation, staleGot.CoordState, store.SealedWithMaster)
	}
}

// TestQuarantineCodexAccountLiveDB pins QuarantineCodexAccount (PRD #1147 audit): the
// identity-mismatch path quarantines an in_progress account owned by the presenting op
// WITHOUT touching the recovery slot; a wrong op or a non-in_progress account is refused.
func TestQuarantineCodexAccountLiveDB(t *testing.T) {
	ctx, _, q, user := codexLiveDB(t)
	future := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

	// In-progress owned by op: quarantine succeeds, recovery slot stays empty.
	acc := mkCodexAccount(ctx, t, q, user)
	op := uuid.New()
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: op, Deadline: future, ID: acc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{
		ID: acc.ID, UserID: user, Op: op,
	}); err != nil || n != 1 {
		t.Fatalf("QuarantineCodexAccount(owner) = (%d,%v), want (1,nil)", n, err)
	}
	got, err := q.GetCodexProviderAccountByID(ctx, store.GetCodexProviderAccountByIDParams{UserID: user, ID: acc.ID})
	if err != nil {
		t.Fatalf("read quarantined: %v", err)
	}
	if got.CoordState != "quarantined" {
		t.Fatalf("coord_state = %q after quarantine, want \"quarantined\"", got.CoordState)
	}
	if got.RecoverySealed != nil || got.RecoveryGeneration.Valid {
		t.Fatalf("QuarantineCodexAccount wrote recovery material (sealed=%q gen=%+v), want it untouched",
			got.RecoverySealed, got.RecoveryGeneration)
	}

	// Wrong op: an in_progress account is not quarantined by a non-owning operation.
	wrongAcc := mkCodexAccount(ctx, t, q, user)
	if n, err := q.AcquireCodexRefreshLease(ctx, store.AcquireCodexRefreshLeaseParams{
		Op: uuid.New(), Deadline: future, ID: wrongAcc.ID, UserID: user, FromGeneration: 0,
	}); err != nil || n != 1 {
		t.Fatalf("AcquireCodexRefreshLease(wrong-op) = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{
		ID: wrongAcc.ID, UserID: user, Op: uuid.New(),
	}); err != nil || n != 0 {
		t.Fatalf("QuarantineCodexAccount(wrong op) = (%d,%v), want (0,nil) — only the lease owner may quarantine", n, err)
	}

	// Non-in_progress: a fresh idle account is not quarantined even by a matching-looking op.
	idleAcc := mkCodexAccount(ctx, t, q, user)
	if n, err := q.QuarantineCodexAccount(ctx, store.QuarantineCodexAccountParams{
		ID: idleAcc.ID, UserID: user, Op: uuid.New(),
	}); err != nil || n != 0 {
		t.Fatalf("QuarantineCodexAccount(idle) = (%d,%v), want (0,nil) — only an in_progress account is quarantinable", n, err)
	}
}
