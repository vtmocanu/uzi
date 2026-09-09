// Command codexm3btestserver is a MINIMAL, throwaway real API test server for
// PRD #1171 milestone m5 (item 7, Go half). It exists ONLY to let a later
// milestone point a REAL TypeScript WorkerClient at a REAL listening loopback
// socket and exercise
//
//	POST /api/worker/runs/{id}/codex/release
//	POST /api/worker/runs/{id}/codex/refresh
//
// over real HTTP against a migrated throwaway Postgres — the coverage the
// in-process handler/worker_codex_livedb_test.go proves in-process, promoted to a
// real socket.
//
// It ships NO production behavior: it is a cmd/ main package that reuses the same
// seeding/mint sequence as worker_codex_livedb_test.go (WorkerRoutes, store.Migrate,
// the fixture graph, SetRunCodexClaimCapability) and a fixed test secretbox key.
// Every credential-shaped value it seeds is a CANARY string, never a real
// credential, and the Codex refresh client is an in-process fake (no provider
// network call), exactly as the live-DB test uses.
//
// It reads a DSN from -dsn or UZI_TEST_DATABASE_URL, seeds a subscription-mode run
// and an api_key-mode run under one owner/worker, prints ONE machine-readable JSON
// line describing everything a WorkerClient needs, then stays up until SIGTERM/
// SIGINT and shuts the server down cleanly.
//
// It binds 127.0.0.1:0 by default (the host loopback smoke) but takes a bind address
// from -listen / CODEX_M3B_LISTEN so it can run as a CONTAINER on an internal network
// (e.g. 0.0.0.0:8080) reachable by other containers. When it binds a wildcard/container
// address the loopback the socket reports is not what a peer container dials, so the
// base_url advertised in the JSON contract is taken from -advertise / CODEX_M3B_ADVERTISE_URL
// (e.g. http://cdr-m3b-api-1234:8080); absent, base_url is derived from the bound address.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/handler"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// fakeCodexRefresh is the in-process CodexRefreshClient the server wires via
// wsvc.SetCodexRefresh, mirroring worker_codex_livedb_test.go's routedCodexRefreshFake.
// It performs NO provider network call: Refresh returns a canary rotated token pair and
// DiscoverIdentity returns the frozen subscription tuple so a coordinated refresh commits
// (advanced) rather than quarantining on a tuple mismatch.
type fakeCodexRefresh struct {
	newAccessToken string
	newRefresh     string
	identity       codexauth.Identity
}

func (f *fakeCodexRefresh) Refresh(_ context.Context, _ string) (codexauth.RefreshResult, error) {
	rotated := f.newRefresh
	return codexauth.RefreshResult{AccessToken: f.newAccessToken, RefreshToken: &rotated}, nil
}

func (f *fakeCodexRefresh) DiscoverIdentity(_ context.Context, _ string) (codexauth.Identity, error) {
	return f.identity, nil
}

// subscriptionContract is the subscription-mode facts a WorkerClient needs to call
// release/refresh: the run id, its per-claim capability, the provider-verified account id
// and the committed generation it starts observing at.
type subscriptionContract struct {
	RunID            string `json:"run_id"`
	Capability       string `json:"capability"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	Generation       int64  `json:"generation"`
}

// apiKeyContract is the api_key-mode facts a WorkerClient needs to call release. An
// api_key run carries no subscription generation/account and is not refreshable.
type apiKeyContract struct {
	RunID      string `json:"run_id"`
	Capability string `json:"capability"`
}

// serverContract is the single JSON line printed to stdout. It is the machine-readable
// handshake a later milestone's lifecycle harness consumes to drive the real WorkerClient.
type serverContract struct {
	BaseURL      string               `json:"base_url"`
	WorkerToken  string               `json:"worker_token"`
	Subscription subscriptionContract `json:"subscription"`
	APIKey       apiKeyContract       `json:"api_key"`
}

// seedResult carries the values the seeding step resolves for the printed contract.
type seedResult struct {
	workerToken  string
	subscription subscriptionContract
	apiKey       apiKeyContract
}

func main() {
	dsn := flag.String("dsn", os.Getenv("UZI_TEST_DATABASE_URL"),
		"Postgres DSN for the throwaway, migrated test database (defaults to $UZI_TEST_DATABASE_URL)")
	listenAddr := flag.String("listen", envOr("CODEX_M3B_LISTEN", "127.0.0.1:0"),
		"TCP bind address (defaults to $CODEX_M3B_LISTEN, else 127.0.0.1:0 for the host loopback smoke; use 0.0.0.0:PORT in a container)")
	advertiseURL := flag.String("advertise", os.Getenv("CODEX_M3B_ADVERTISE_URL"),
		"base_url other containers dial this server at, e.g. http://cdr-m3b-api-NNN:8080 (defaults to $CODEX_M3B_ADVERTISE_URL; absent, derived from the bound address)")
	flag.Parse()
	if *dsn == "" {
		log.Fatal("codexm3btestserver: no DSN; set -dsn or UZI_TEST_DATABASE_URL to a throwaway, migrated Postgres")
	}

	ctx := context.Background()
	if err := store.Migrate(ctx, *dsn); err != nil {
		log.Fatalf("codexm3btestserver: migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, *dsn)
	if err != nil {
		log.Fatalf("codexm3btestserver: open pool: %v", err)
	}
	defer pool.Close()

	q := store.New(pool)
	box := newTestBox()
	wsvc := workersvc.New(q, box, workersvc.Params{})

	seed, err := seedFixtures(ctx, pool, q, box, wsvc)
	if err != nil {
		log.Fatalf("codexm3btestserver: seed: %v", err)
	}

	// handler.New is the only exported Handler constructor; the codex worker routes use
	// only h.q (RequireWorker) and h.wsvc, and mountControllerRoutes stays off with a zero
	// config (WorkerHostingEnabled false), so the forge/privcheck/hub/settings collaborators
	// are safely nil for this credential-only surface.
	h := handler.New(pool, q, config.Config{}, box, nil, wsvc, nil, nil, nil)
	router := h.WorkerRoutes(mw.NewLimiter(1000, time.Minute, nil))

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("codexm3btestserver: listen on %q: %v", *listenAddr, err)
	}
	srv := &http.Server{
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	// The advertised base_url is what a WORKER (possibly in another container) dials. In
	// container mode the bound address is a wildcard (0.0.0.0), which no peer can dial, so
	// -advertise/$CODEX_M3B_ADVERTISE_URL supplies the container-name origin; absent, derive
	// it from the bound loopback address (the host smoke).
	baseURL := *advertiseURL
	if baseURL == "" {
		baseURL = "http://" + ln.Addr().String()
	}
	contract := serverContract{
		BaseURL:      baseURL,
		WorkerToken:  seed.workerToken,
		Subscription: seed.subscription,
		APIKey:       seed.apiKey,
	}
	line, err := json.Marshal(contract)
	if err != nil {
		log.Fatalf("codexm3btestserver: marshal contract: %v", err)
	}
	// The contract is the ONLY thing on stdout (all diagnostics go to stderr via log), so a
	// consumer can read exactly one JSON line. Flush before serving so the harness can
	// connect the moment it has the line.
	if _, err := os.Stdout.Write(append(line, '\n')); err != nil {
		log.Fatalf("codexm3btestserver: write contract: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-sigCtx.Done():
		log.Print("codexm3btestserver: signal received, shutting down")
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("codexm3btestserver: serve: %v", err)
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("codexm3btestserver: shutdown: %v", err)
	}
}

// newTestBox builds a secretbox with the same fixed, non-placeholder key
// worker_codex_livedb_test.go's newHandlerTestBox uses. A test server sealing and opening
// canary values needs a stable key, not a real one.
func newTestBox() *secretbox.Box {
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		log.Fatalf("codexm3btestserver: new secretbox: %v", err)
	}
	return box
}

// seedFixtures builds the same fixture graph as worker_codex_livedb_test.go for BOTH a
// subscription-mode run and an api_key-mode run, under ONE owner user and ONE worker (so
// the printed contract carries a single worker token). AuthorizeCodexCredentialOp resolves
// the alias/account by the WORKER's user id, so the worker, both runs and every secret must
// share one owner.
//
// Every credential-shaped value is a CANARY string, never a real credential.
func seedFixtures(ctx context.Context, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, wsvc *workersvc.Service) (seedResult, error) {
	ownerID, connectionID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	if err := exec(ctx, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		ownerID, fmt.Sprintf("codex-m3b-%s@example.test", uuid.NewString())); err != nil {
		return seedResult{}, fmt.Errorf("insert user: %w", err)
	}
	if err := exec(ctx, pool, `INSERT INTO forge_connections
		(id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'gitlab', 'https://forge.example', 'bot', 1, $3)`,
		connectionID, ownerID, []byte("x")); err != nil {
		return seedResult{}, fmt.Errorf("insert forge_connection: %w", err)
	}
	if err := exec(ctx, pool, `INSERT INTO repos
		(id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 1, $3, 'https://forge.example/group/repo', 'main', true)`,
		repoID, connectionID, "group/repo-"+repoID.String()); err != nil {
		return seedResult{}, fmt.Errorf("insert repo: %w", err)
	}

	joinToken, joinHash, err := jointoken.Generate()
	if err != nil {
		return seedResult{}, fmt.Errorf("generate worker token: %w", err)
	}
	if err := exec(ctx, pool, `INSERT INTO workers (id, user_id, name, token_hash, status)
		VALUES ($1, $2, $3, $4, 'online')`,
		workerID, ownerID, "codex-m3b-"+workerID.String(), joinHash); err != nil {
		return seedResult{}, fmt.Errorf("insert worker: %w", err)
	}

	sub, err := seedSubscription(ctx, pool, q, box, wsvc, ownerID, repoID, workerID)
	if err != nil {
		return seedResult{}, fmt.Errorf("subscription fixture: %w", err)
	}
	api, err := seedAPIKey(ctx, pool, q, box, wsvc, ownerID, repoID, workerID)
	if err != nil {
		return seedResult{}, fmt.Errorf("api_key fixture: %w", err)
	}

	return seedResult{workerToken: joinToken, subscription: sub, apiKey: api}, nil
}

// seedSubscription mirrors the subscription arm of worker_codex_livedb_test.go: a codex_auth
// alias sealing a canary login blob, a provider account with a verified canary identity
// tuple, a linked credential state, a claimed run frozen to the binding, and a minted
// per-claim capability. It also wires the in-process refresh fake to return the frozen
// tuple so a coordinated refresh advances.
func seedSubscription(ctx context.Context, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, wsvc *workersvc.Service, ownerID, repoID, workerID uuid.UUID) (subscriptionContract, error) {
	accessToken := "canary-sub-access-" + uuid.NewString()
	refreshToken := "canary-sub-refresh-" + uuid.NewString()
	login := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q}`, accessToken, refreshToken)
	sealedLogin, err := box.Seal([]byte(login))
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("seal login: %w", err)
	}
	secretRow, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
		UserID: ownerID, Kind: store.KindCodexAuth, Label: "m3b-subscription", WantDefault: true,
		Ciphertext: sealedLogin, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("insert codex secret: %w", err)
	}

	providerUserID := "canary-provider-" + uuid.NewString()
	chatGPTAccountID := "canary-account-" + uuid.NewString()
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: ownerID, ProviderUserID: providerUserID, WorkspaceAccountID: chatGPTAccountID,
		SealedLogin: sealedLogin, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("insert provider account: %w", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secretRow.ID, UserID: ownerID, Status: "staging",
	}); err != nil {
		return subscriptionContract{}, fmt.Errorf("insert credential state: %w", err)
	}
	linked, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secretRow.ID,
		UserID: ownerID, MaterialRevision: 0,
	})
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("link credential state: %w", err)
	}
	if linked != 1 {
		return subscriptionContract{}, fmt.Errorf("link credential state affected %d rows, want 1", linked)
	}

	runID := uuid.New()
	if err := exec(ctx, pool, `INSERT INTO runs
		(id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		VALUES ($1, $2, $3, 'issue', 1, 'm3b subscription', 'm3b subscription body', 'claimed', $4)`,
		runID, ownerID, repoID, workerID); err != nil {
		return subscriptionContract{}, fmt.Errorf("insert run: %w", err)
	}
	if err := wsvc.FreezeCodexBinding(ctx, ownerID, runID, secretRow.ID, "subscription"); err != nil {
		return subscriptionContract{}, fmt.Errorf("freeze binding: %w", err)
	}

	capability, err := mintCapability(ctx, q, runID, workerID, "m3b-sub-cap-")
	if err != nil {
		return subscriptionContract{}, err
	}

	// Wire the in-process refresh fake to the frozen tuple so CoordinatedCodexRefresh
	// advances (rather than quarantining on a mismatch) and commits a canary rotated token.
	wsvc.SetCodexRefresh(&fakeCodexRefresh{
		newAccessToken: "canary-sub-access-new-" + uuid.NewString(),
		newRefresh:     "canary-sub-refresh-new-" + uuid.NewString(),
		identity:       codexauth.Identity{ProviderUserID: providerUserID, WorkspaceAccountID: chatGPTAccountID},
	})

	return subscriptionContract{
		RunID:            runID.String(),
		Capability:       capability,
		ChatGPTAccountID: chatGPTAccountID,
		Generation:       account.Generation,
	}, nil
}

// seedAPIKey mirrors an api_key-mode run: an openai_api_key alias sealing a canary static
// key, a 'static' credential state (no provider account), a claimed run frozen to the
// binding as api_key, and a minted per-claim capability. An api_key run is not refreshable.
func seedAPIKey(ctx context.Context, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, wsvc *workersvc.Service, ownerID, repoID, workerID uuid.UUID) (apiKeyContract, error) {
	apiKeyValue := "canary-openai-key-" + uuid.NewString()
	sealedKey, err := box.Seal([]byte(apiKeyValue))
	if err != nil {
		return apiKeyContract{}, fmt.Errorf("seal api key: %w", err)
	}
	secretRow, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
		UserID: ownerID, Kind: store.KindOpenAIAPIKey, Label: "m3b-apikey", WantDefault: false,
		Ciphertext: sealedKey, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		return apiKeyContract{}, fmt.Errorf("insert codex secret: %w", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secretRow.ID, UserID: ownerID, Status: "static",
	}); err != nil {
		return apiKeyContract{}, fmt.Errorf("insert credential state: %w", err)
	}

	runID := uuid.New()
	if err := exec(ctx, pool, `INSERT INTO runs
		(id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		VALUES ($1, $2, $3, 'issue', 2, 'm3b api_key', 'm3b api_key body', 'claimed', $4)`,
		runID, ownerID, repoID, workerID); err != nil {
		return apiKeyContract{}, fmt.Errorf("insert run: %w", err)
	}
	if err := wsvc.FreezeCodexBinding(ctx, ownerID, runID, secretRow.ID, "api_key"); err != nil {
		return apiKeyContract{}, fmt.Errorf("freeze binding: %w", err)
	}

	capability, err := mintCapability(ctx, q, runID, workerID, "m3b-api-cap-")
	if err != nil {
		return apiKeyContract{}, err
	}

	return apiKeyContract{RunID: runID.String(), Capability: capability}, nil
}

// mintCapability mints a run's per-claim Codex capability the same way the live-DB test
// does: a random secret, its sha256 stored via SetRunCodexClaimCapability, and the wire
// form "<epoch>.<secret>" the worker presents.
func mintCapability(ctx context.Context, q *store.Queries, runID, workerID uuid.UUID, prefix string) (string, error) {
	secret := prefix + uuid.NewString()
	hash := sha256.Sum256([]byte(secret))
	epoch, err := q.SetRunCodexClaimCapability(ctx, store.SetRunCodexClaimCapabilityParams{
		Hash: hash[:], ID: runID, WorkerID: pgconv.UUID(workerID),
	})
	if err != nil {
		return "", fmt.Errorf("mint capability: %w", err)
	}
	return fmt.Sprintf("%d.%s", epoch, secret), nil
}

// envOr returns the environment value for key, or fallback when it is unset/empty. Used for the
// -listen/-advertise flag defaults so the container orchestration can drive them via env.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// exec runs one seeding statement, wrapping any error with its call site's context.
func exec(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) error {
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		return err
	}
	return nil
}
