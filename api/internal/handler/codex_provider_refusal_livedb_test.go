package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Observe credential-input SQL only, not calls to vault.Open or query arguments.
type providerRouteInputReads struct {
	ciphertext, snapshots atomic.Int64
}

func (q *providerRouteInputReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	switch {
	case strings.HasPrefix(data.SQL, "-- name: GetUserSecretCiphertextByID "):
		q.ciphertext.Add(1)
	case strings.HasPrefix(data.SQL, "-- name: GetCodexProviderAccountSnapshot "):
		q.snapshots.Add(1)
	}
	return ctx
}

func (*providerRouteInputReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type providerRouteFixture struct {
	ctx                                  context.Context
	pool                                 *pgxpool.Pool
	q                                    *store.Queries
	vlt                                  *vault.Vault
	reads                                *providerRouteInputReads
	owner, repo, worker, native, secret  uuid.UUID
	capability, access, refresh, account string
	provider                             *routedCodexRefreshFake
	post                                 func(string, string) *httptest.ResponseRecorder
}

// Keep this setup local: the existing route control remains independently executable.
func newProviderRouteFixture(t *testing.T) *providerRouteFixture {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reads := &providerRouteInputReads{}
	pool, err := store.OpenPool(ctx, dsn, store.WithQueryTracer(reads))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	wsvc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	vlt := vault.NewPostgres(box, pool)
	wsvc.SetVault(vlt)
	wsvc.SetTxBeginner(pool)
	h := &Handler{q: q, wsvc: wsvc}
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))

	ownerID, connectionID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash, vault_mode) VALUES ($1, $2, 'x', 'unprotected')`,
		ownerID, fmt.Sprintf("codex-route-%s@example.test", uuid.NewString()))
	mustExecT(ctx, t, pool, `INSERT INTO forge_connections
		(id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'gitlab', 'https://forge.example', 'bot', 1, $3)`, connectionID, ownerID, []byte("x"))
	mustExecT(ctx, t, pool, `INSERT INTO repos
		(id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 1, $3, 'https://forge.example/group/repo', 'main', true)`,
		repoID, connectionID, "group/repo-"+repoID.String())

	joinToken, joinHash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("generate worker token: %v", err)
	}
	mustExecT(ctx, t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status)
		VALUES ($1, $2, $3, $4, 'online')`, workerID, ownerID, "codex-route-"+workerID.String(), joinHash)

	accessToken := "route-access-" + uuid.NewString()
	refreshToken := "route-refresh-" + uuid.NewString()
	login := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q}`, accessToken, refreshToken)
	sealedLogin, err := box.Seal([]byte(login))
	if err != nil {
		t.Fatalf("seal login: %v", err)
	}
	secretRow, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
		UserID: ownerID, Kind: store.KindCodexAuth, Label: "route-subscription", WantDefault: true,
		Ciphertext: sealedLogin, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert codex secret: %v", err)
	}
	providerUserID := "provider-" + uuid.NewString()
	chatGPTAccountID := "account-" + uuid.NewString()
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: ownerID, ProviderUserID: providerUserID, WorkspaceAccountID: chatGPTAccountID,
		SealedLogin: sealedLogin, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatalf("insert provider account: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secretRow.ID, UserID: ownerID, Status: "staging",
	}); err != nil {
		t.Fatalf("insert credential state: %v", err)
	}
	linked, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secretRow.ID,
		UserID: ownerID, MaterialRevision: 0,
	})
	if err != nil || linked != 1 {
		t.Fatalf("link credential state = (%d, %v), want (1, nil)", linked, err)
	}

	mustExecT(ctx, t, pool, `INSERT INTO runs
		(id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		VALUES ($1, $2, $3, 'issue', 1, 'route test', 'route body', 'claimed', $4)`,
		runID, ownerID, repoID, workerID)
	if err := wsvc.FreezeCodexBinding(ctx, ownerID, runID, secretRow.ID, "subscription"); err != nil {
		t.Fatalf("freeze subscription binding: %v", err)
	}
	capabilitySecret := "route-capability-" + uuid.NewString()
	capabilityHash := sha256.Sum256([]byte(capabilitySecret))
	seeded, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatalf("read seeded run: %v", err)
	}
	epoch, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash: capabilityHash[:], ID: runID, WorkerID: pgconv.UUID(workerID), ClaimGeneration: seeded.ClaimGeneration,
	})
	if err != nil {
		t.Fatalf("mint capability: %v", err)
	}
	capability := fmt.Sprintf("%d.%s", epoch, capabilitySecret)

	newAccessToken := "route-access-new-" + uuid.NewString()
	newRefreshToken := "route-refresh-new-" + uuid.NewString()
	provider := &routedCodexRefreshFake{
		newAccessToken: newAccessToken,
		newRefresh:     newRefreshToken,
		identity: codexauth.Identity{
			ProviderUserID: providerUserID, WorkspaceAccountID: chatGPTAccountID,
		},
	}
	wsvc.SetCodexRefresh(provider)

	doPost := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+joinToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	return &providerRouteFixture{ctx: ctx, pool: pool, q: q, vlt: vlt, reads: reads, owner: ownerID, repo: repoID, worker: workerID, native: runID, secret: secretRow.ID, capability: capability, access: accessToken, refresh: refreshToken, account: chatGPTAccountID, provider: provider, post: doPost}
}

// A legal provider tuple has no native authority to join. Usable credentials for
// the same owner must not become a fallback on either native endpoint.
func TestWorkerCodexProviderRoutesRefuseLiveDB(t *testing.T) {
	f := newProviderRouteFixture(t)
	ctx := f.ctx
	p, err := f.q.InsertModelProvider(ctx, store.InsertModelProviderParams{
		OwnerUserID: f.owner, Name: "route-provider-" + uuid.NewString(), Engine: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.InsertModelProviderRevision(ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: "https://gateway.example.test", AuthStyle: "bearer",
		ModelMap: []byte(`{"default":{"model":"gateway"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	canary := "inert-" + "provider-route-canary-" + uuid.NewString()
	sealed, err := f.vlt.Seal(ctx, f.owner, store.KindOpenAICompatibleKey, []byte(canary), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	key, err := f.q.InsertProviderKeySecret(ctx, store.InsertProviderKeySecretParams{
		UserID: f.owner, Label: "route-key-" + uuid.NewString(), Ciphertext: sealed.Ciphertext, SealedWith: sealed.SealedWith, ProviderID: pgconv.UUID(p.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	providerRun := uuid.New()
	mustExecT(ctx, t, f.pool, `INSERT INTO runs
 (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,harness,
 model_provider_id,model_provider_revision,model_provider_key_mode,model_provider_key_secret_id,model_provider_key_rev)
 SELECT $1,$2,$3,'issue',2,'provider refusal','body','claimed',$4,'codex',$5,1,'user_key',$6,enablement_rev
 FROM user_secrets WHERE id=$6 AND user_id=$2`,
		providerRun, f.owner, f.repo, f.worker, p.ID, key.ID)
	frozen, err := f.q.GetRunByID(ctx, providerRun)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.CodexSecretID.Valid || frozen.CodexAuthMode.Valid || frozen.CredentialOverrideSecretID.Valid ||
		frozen.CredentialOverrideMode.Valid || !frozen.ModelProviderID.Valid || frozen.ModelProviderID.Bytes != p.ID ||
		!frozen.ModelProviderKeySecretID.Valid || frozen.ModelProviderKeySecretID.Bytes != key.ID {
		t.Fatal("fixture is not a provider-only frozen run")
	}
	refuse := func() {
		t.Helper()
		f.reads.ciphertext.Store(0)
		f.reads.snapshots.Store(0)
		beforeCalls, beforeDiscover := f.provider.refreshCalls, f.provider.discoverCalls
		for _, op := range []string{"release", "refresh"} {
			body := fmt.Sprintf(`{"capability":%q}`, f.capability)
			if op == "refresh" {
				body = fmt.Sprintf(`{"capability":%q,"operation_id":%q,"observed_generation":0}`, f.capability, uuid.NewString())
			}
			rec := f.post("/api/worker/runs/"+providerRun.String()+"/codex/"+op, body)
			var envelope struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusNotFound || envelope.Error != codexErrRunNotFound {
				t.Fatalf("%s refusal = %d %s", op, rec.Code, rec.Body.String())
			}
			for _, sensitive := range []string{canary, f.access, f.refresh, f.provider.newAccessToken, f.provider.newRefresh, "access_token", "refresh_token"} {
				if strings.Contains(rec.Body.String(), sensitive) {
					t.Fatalf("%s refusal disclosed credential material or token field", op)
				}
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s refusal lacks no-store", op)
			}
		}
		if f.reads.ciphertext.Load() != 0 || f.reads.snapshots.Load() != 0 || f.provider.refreshCalls != beforeCalls || f.provider.discoverCalls != beforeDiscover {
			t.Fatalf("refusal fetched credential inputs or called provider: ciphertext=%d snapshots=%d refresh=%d discover=%d", f.reads.ciphertext.Load(), f.reads.snapshots.Load(), f.provider.refreshCalls-beforeCalls, f.provider.discoverCalls-beforeDiscover)
		}
	}
	nativeControl := func(generation int64, wantAccess, wantRefresh string) {
		t.Helper()
		f.reads.ciphertext.Store(0)
		f.reads.snapshots.Store(0)
		calls := f.provider.refreshCalls
		rec := f.post("/api/worker/runs/"+f.native.String()+"/codex/release", fmt.Sprintf(`{"capability":%q}`, f.capability))
		var released codexSubscriptionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &released); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || released.Generation != generation || released.AccessToken != wantAccess || released.ChatGPTAccountID != f.account {
			t.Fatalf("native release generation %d: status=%d", generation, rec.Code)
		}
		if f.reads.ciphertext.Load() != 0 || f.reads.snapshots.Load() != 1 || f.provider.refreshCalls != calls || f.provider.discoverCalls != 0 {
			t.Fatalf("native release inputs: ciphertext=%d snapshots=%d refresh=%d discover=%d", f.reads.ciphertext.Load(), f.reads.snapshots.Load(), f.provider.refreshCalls-calls, f.provider.discoverCalls)
		}
		f.reads.snapshots.Store(0)
		rec = f.post("/api/worker/runs/"+f.native.String()+"/codex/refresh", fmt.Sprintf(`{"capability":%q,"operation_id":%q,"observed_generation":%d}`, f.capability, uuid.NewString(), generation))
		var refreshed codexRefreshResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &refreshed); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || refreshed.Generation != generation+1 || refreshed.AccessToken != f.provider.newAccessToken || refreshed.ChatGPTAccountID != f.account {
			t.Fatalf("native refresh generation %d: status=%d", generation, rec.Code)
		}
		if f.reads.ciphertext.Load() != 0 || f.reads.snapshots.Load() == 0 || f.provider.refreshCalls != calls+1 || f.provider.discoverCalls != 0 || f.provider.refreshToken != wantRefresh {
			t.Fatalf("native refresh inputs/calls differ: ciphertext=%d snapshots=%d refresh=%d discover=%d", f.reads.ciphertext.Load(), f.reads.snapshots.Load(), f.provider.refreshCalls-calls, f.provider.discoverCalls)
		}
	}
	refuse()
	nativeControl(0, f.access, f.refresh)
	nativeBefore, err := f.q.GetRunByID(ctx, f.native)
	if err != nil {
		t.Fatal(err)
	}
	if nativeBefore.Harness != "codex" {
		t.Fatal("native control is not frozen to the Codex harness")
	}
	// Edit only the current source, then exercise the very same native capability.
	mustExecT(ctx, t, f.pool, `UPDATE users SET codex_provider_id=$2,codex_provider_scope_id=$1 WHERE id=$1`, f.owner, p.ID)
	t.Cleanup(func() {
		if _, err := f.pool.Exec(context.Background(), `UPDATE users SET codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id=$1`, f.owner); err != nil {
			t.Errorf("clear owned source: %v", err)
		}
	})
	nativeAfter, err := f.q.GetRunByID(ctx, f.native)
	if err != nil {
		t.Fatal(err)
	}
	if nativeAfter.ModelProviderID.Valid || nativeAfter.ModelProviderRevision.Valid || nativeAfter.ModelProviderKeyMode.Valid ||
		nativeAfter.ModelProviderKeySecretID.Valid || nativeAfter.ModelProviderKeyRev.Valid ||
		nativeAfter.Harness != nativeBefore.Harness ||
		nativeAfter.CodexSecretID != nativeBefore.CodexSecretID || nativeAfter.CodexAuthMode != nativeBefore.CodexAuthMode ||
		nativeAfter.CodexSecretLabel != nativeBefore.CodexSecretLabel || nativeAfter.CodexMaterialRevision != nativeBefore.CodexMaterialRevision ||
		nativeAfter.CodexAccountKey != nativeBefore.CodexAccountKey || nativeAfter.CodexAccountRevision != nativeBefore.CodexAccountRevision ||
		nativeAfter.CodexClaimEpoch != nativeBefore.CodexClaimEpoch ||
		!bytes.Equal(nativeAfter.CodexCapHash, nativeBefore.CodexCapHash) || nativeAfter.ClaimGeneration != nativeBefore.ClaimGeneration {
		t.Fatal("current source edit changed frozen native binding or capability")
	}
	refuse()
	oldAccess, oldRefresh := f.provider.newAccessToken, f.provider.newRefresh
	f.provider.newAccessToken = "route-access-second-" + uuid.NewString()
	f.provider.newRefresh = "route-refresh-second-" + uuid.NewString()
	nativeControl(1, oldAccess, oldRefresh)
}
