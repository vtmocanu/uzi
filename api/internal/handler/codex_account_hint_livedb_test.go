package handler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	protocol "github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerCodexAccountHintRepliesAndTimingLiveDB(t *testing.T) {
	logs := installTimingCapture(t)
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	q := store.New(pool)
	box := newHandlerTestBox(t)
	wsvc := workersvc.New(q, box, workersvc.Params{})
	h := &Handler{q: q, wsvc: wsvc}
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))

	ownerID, connectionID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM users WHERE id=$1", ownerID); err != nil {
			t.Errorf("delete recovery fixture user %s: %v", ownerID, err)
		}
	})
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
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

	path := "/api/worker/runs/" + runID.String() + "/codex/refresh"
	request := func(cap string) *httptest.ResponseRecorder {
		return doPost(path, fmt.Sprintf(`{"capability":%q,"operation_id":%q,"observed_generation":0}`, cap, uuid.NewString()))
	}
	n, err := q.BumpCodexMaterialRevision(ctx, store.BumpCodexMaterialRevisionParams{UserSecretID: secretRow.ID, UserID: ownerID, Status: "staging"})
	if err != nil || n != 1 {
		t.Fatalf("PATCH: %d %v", n, err)
	}
	for _, tc := range []struct {
		name    string
		capable bool
		want    string
	}{
		{"old-worker", false, "{\"error\":\"codex credential is not available\"}\n"},
		{"capable", true, "{\"error\":\"codex credential is not available\",\"reason\":\"codex_account_unavailable\"}\n"},
	} {
		caps := []string{}
		if tc.capable {
			caps = append(caps, protocol.CodexAccountParkV1)
		}
		mustExecT(ctx, t, pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", workerID, caps)
		rec := request(capability)
		if rec.Code != 409 || rec.Body.String() != tc.want {
			t.Fatalf("%s: %d %q", tc.name, rec.Code, rec.Body.String())
		}
	}
	// A newer alias linked to a revoked credential revision is terminal, not a hold.
	mustExecT(ctx, t, pool, "UPDATE codex_provider_account SET credential_revision=credential_revision+1 WHERE id=$1", account.ID)
	mustExecT(ctx, t, pool, "UPDATE codex_credential_state SET status='linked', provider_account_id=$2 WHERE user_secret_id=$1", secretRow.ID, account.ID)
	rec := request(capability)
	if rec.Code != 409 || rec.Body.String() != "{\"error\":\"codex credential is not available\"}\n" {
		t.Fatalf("nonhold: %d %q", rec.Code, rec.Body.String())
	}
	rec = request(capability + "-wrong")
	if rec.Code != 403 {
		t.Fatalf("authority: %d %q", rec.Code, rec.Body.String())
	}
	var routes []timingRecord
	for _, rec := range logs.snapshot() {
		if rec.msg == codexTimingMsgRoute {
			routes = append(routes, rec)
		}
	}
	if len(routes) != 4 {
		t.Fatalf("routes=%d", len(routes))
	}
	for i, rec := range routes {
		wantClass := "material_revision_stale"
		if i == 3 {
			wantClass = "capability"
		}
		if rec.attrs["error_class"].String() != wantClass {
			t.Fatalf("record %d: %v", i, rec.attrs)
		}
		value, computed := rec.attrs["account_hold"]
		if computed != (i != 3) || (computed && value.Bool() != (i < 2)) {
			t.Fatalf("record %d: hold=%v computed=%v", i, value, computed)
		}
		for _, key := range []string{"error", "secret_id", "account_id", "user_id"} {
			if _, ok := rec.attrs[key]; ok {
				t.Fatalf("diagnostic leaked %s", key)
			}
		}
	}
	if provider.refreshCalls != 0 {
		t.Fatalf("provider calls=%d", provider.refreshCalls)
	}
}
