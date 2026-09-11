package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type routedCodexRefreshFake struct {
	refreshToken   string
	newAccessToken string
	newRefresh     string
	identity       codexauth.Identity
	refreshCalls   int
	discoverCalls  int
}

type failingCodexResponseWriter struct {
	header http.Header
	status int
}

func (w *failingCodexResponseWriter) Header() http.Header { return w.header }
func (w *failingCodexResponseWriter) WriteHeader(status int) {
	w.status = status
}
func (*failingCodexResponseWriter) Write(_ []byte) (int, error) {
	return 0, fmt.Errorf("response write failed")
}

func (f *routedCodexRefreshFake) Refresh(_ context.Context, refreshToken string) (codexauth.RefreshResult, error) {
	f.refreshCalls++
	f.refreshToken = refreshToken
	return codexauth.RefreshResult{
		AccessToken:  f.newAccessToken,
		RefreshToken: &f.newRefresh,
		IdentityClaims: codexauth.FreshAccessTokenIdentityClaims{
			ChatGPTAccountID: f.identity.WorkspaceAccountID,
			ChatGPTUserID:    f.identity.ProviderUserID,
			AuthUserID:       f.identity.ProviderUserID,
		},
	}, nil
}

func (f *routedCodexRefreshFake) DiscoverIdentity(_ context.Context, _ string) (codexauth.Identity, error) {
	f.discoverCalls++
	return f.identity, nil
}

// Drive both operations through the real WorkerRoutes tree, Bearer middleware, handlers,
// workersvc authorization and live SQL. A direct handler call cannot prove the paths are
// mounted to the correct methods; a service-only test cannot prove RequireWorker or JSON
// routing. Distinct request shapes make a release/refresh handler swap fail with 400.
func TestWorkerCodexRoutesReleaseAndRefreshLiveDB(t *testing.T) {
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
	epoch, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash: capabilityHash[:], ID: runID, WorkerID: pgconv.UUID(workerID),
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
	timings := installTimingCapture(t)

	doPost := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+joinToken)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	release := doPost("/api/worker/runs/"+runID.String()+"/codex/release",
		fmt.Sprintf(`{"capability":%q}`, capability))
	if release.Code != http.StatusOK {
		t.Fatalf("routed release = %d, want 200: %s", release.Code, release.Body.String())
	}
	var released codexSubscriptionResponse
	if err := json.Unmarshal(release.Body.Bytes(), &released); err != nil {
		t.Fatalf("decode release: %v", err)
	}
	if released.AccessToken != accessToken || released.Generation != 0 || released.ChatGPTAccountID != chatGPTAccountID {
		t.Fatalf("release = %+v, want original token, generation 0 and verified account", released)
	}
	if provider.refreshCalls != 0 || provider.discoverCalls != 0 {
		t.Fatalf("release called refresh provider: refresh=%d discover=%d", provider.refreshCalls, provider.discoverCalls)
	}

	refreshOpID := uuid.New()
	refresh := doPost("/api/worker/runs/"+runID.String()+"/codex/refresh",
		fmt.Sprintf(`{"capability":%q,"operation_id":%q,"observed_generation":0}`, capability, refreshOpID.String()))
	if refresh.Code != http.StatusOK {
		t.Fatalf("routed refresh = %d, want 200: %s", refresh.Code, refresh.Body.String())
	}
	var refreshed codexRefreshResponse
	if err := json.Unmarshal(refresh.Body.Bytes(), &refreshed); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if refreshed.AccessToken != newAccessToken || refreshed.Generation != 1 || refreshed.ChatGPTAccountID != chatGPTAccountID {
		t.Fatalf("refresh = %+v, want rotated token, generation 1 and verified account", refreshed)
	}
	if provider.refreshCalls != 1 || provider.discoverCalls != 0 || provider.refreshToken != refreshToken {
		t.Fatalf("provider calls/token = (%d,%d,%q), want (1,0,original refresh token)",
			provider.refreshCalls, provider.discoverCalls, provider.refreshToken)
	}

	writeFailureOpID := uuid.New()
	writeFailureReq := httptest.NewRequest(http.MethodPost,
		"/api/worker/runs/"+runID.String()+"/codex/refresh",
		strings.NewReader(fmt.Sprintf(`{"capability":%q,"operation_id":%q,"observed_generation":0}`,
			capability, writeFailureOpID.String())))
	writeFailureReq.Header.Set("Authorization", "Bearer "+joinToken)
	writeFailureReq.Header.Set("Content-Type", "application/json")
	writeFailure := &failingCodexResponseWriter{header: make(http.Header)}
	router.ServeHTTP(writeFailure, writeFailureReq)
	if writeFailure.status != http.StatusOK {
		t.Fatalf("write-failure refresh status = %d, want 200 headers before the failed body write", writeFailure.status)
	}
	if provider.refreshCalls != 1 || provider.discoverCalls != 0 {
		t.Fatalf("reconciled write-failure request repeated provider calls: refresh=%d discover=%d",
			provider.refreshCalls, provider.discoverCalls)
	}

	assertResults := func(operationID uuid.UUID, wantService, wantRoute string) {
		t.Helper()
		serviceCount, routeCount := 0, 0
		for _, rec := range timings.snapshot() {
			opAttr, ok := rec.attrs["operation_id"]
			if !ok || opAttr.String() != operationID.String() {
				continue
			}
			switch rec.msg {
			case "codex refresh service":
				serviceCount++
				assertTimingKeySet(t, rec, "operation_id", "service_total_ms", "result")
				if got := rec.attrs["result"].String(); got != wantService {
					t.Fatalf("service result for %s = %q, want %q", operationID, got, wantService)
				}
			case codexTimingMsgRoute:
				routeCount++
				assertTimingKeySet(t, rec, "operation_id", "route_total_ms", "result")
				if got := rec.attrs["result"].String(); got != wantRoute {
					t.Fatalf("route result for %s = %q, want %q", operationID, got, wantRoute)
				}
			}
		}
		if serviceCount != 1 || routeCount != 1 {
			t.Fatalf("timing records for %s: service=%d route=%d, want 1 each",
				operationID, serviceCount, routeCount)
		}
	}
	assertResults(refreshOpID, "ok", "ok")
	assertResults(writeFailureOpID, "ok", "error")
}
