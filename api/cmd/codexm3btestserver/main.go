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
//
// It has TWO modes, selected by env:
//
//   - DEFAULT (canary): every credential-shaped value it seeds is a CANARY string,
//     never a real credential, and the Codex refresh client is an in-process fake
//     (no provider network call), exactly as the live-DB test uses. This is the
//     credential-free path every automated run takes.
//   - CODEX_M3B_LIVE=1 (maintainer-only, egress-capable): it seals the REAL
//     env-injected subscription login from CODEX_M3B_LIVE_LOGIN_JSON
//     ({"access_token":...,"refresh_token":...}), auto-discovers that login's
//     canonical identity tuple with a NONROTATING codexauth.DiscoverIdentity (no
//     account id is injected), wires the REAL codexauth client into the coordinated
//     refresher so /codex/refresh performs a real oauth rotation, and seeds
//     SUBSCRIPTION-ONLY (the api_key arm is skipped, its contract fields stay the
//     zero value). The real credential is ENV-ONLY — it is never written to a file.
//
// It reads a DSN from -dsn or UZI_TEST_DATABASE_URL, seeds a subscription-mode run
// (plus, in the default mode, an api_key-mode run) under one owner/worker, prints ONE
// machine-readable JSON line describing everything a WorkerClient needs, then stays up
// until SIGTERM/SIGINT and shuts the server down cleanly.
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
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
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
// It performs no provider network call: Refresh returns a canary token pair plus matching
// fresh-token claims. DiscoverIdentity remains available for recovery promotion.
type fakeCodexRefresh struct {
	newAccessToken string
	newRefresh     string
	identity       codexauth.Identity
}

func (f *fakeCodexRefresh) Refresh(_ context.Context, _ string) (codexauth.RefreshResult, error) {
	rotated := f.newRefresh
	return codexauth.RefreshResult{
		AccessToken:  f.newAccessToken,
		RefreshToken: &rotated,
		IdentityClaims: codexauth.FreshAccessTokenIdentityClaims{
			ChatGPTAccountID: f.identity.WorkspaceAccountID,
			ChatGPTUserID:    f.identity.ProviderUserID,
			AuthUserID:       f.identity.ProviderUserID,
		},
	}, nil
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
	// Live reports whether this server seeded a REAL env-injected subscription login
	// (CODEX_M3B_LIVE=1) rather than the default canary. It is informational for a
	// maintainer reading the JSON line; the lifecycle harness keys its own live behaviour
	// off CODEX_M3B_LIVE in its environment, not off this field. In live mode APIKey is the
	// zero value (subscription-only).
	Live bool `json:"live"`
}

// liveSubscription carries the maintainer-injected REAL Codex subscription login and the
// real codexauth client seedSubscriptionLive uses. A nil *liveSubscription selects the
// default canary seeding; a non-nil one seals the real env-injected login and wires the real
// refresh client. It exists ONLY when CODEX_M3B_LIVE=1 (see the file header). The two tokens
// stay in memory and the sealed DB row — never written to a file.
type liveSubscription struct {
	accessToken  string
	refreshToken string
	client       *codexauth.Client
}

// buildLiveSubscription returns the live subscription seeding inputs when CODEX_M3B_LIVE=1,
// or nil for the default canary path. In live mode it REQUIRES a real login blob in
// CODEX_M3B_LIVE_LOGIN_JSON ({"access_token":...,"refresh_token":...}); an absent or
// incomplete blob is a fatal misconfiguration, so a live run can never silently fall back to
// a canary and read green. codexauth.NewClient's default endpoints hit the REAL
// chatgpt.com/auth.openai.com hosts; codexauth exposes no base-URL override and must not get
// one, so identity discovery and the coordinated refresh both target the real provider.
func buildLiveSubscription() *liveSubscription {
	if os.Getenv("CODEX_M3B_LIVE") != "1" {
		return nil
	}
	raw := os.Getenv("CODEX_M3B_LIVE_LOGIN_JSON")
	if raw == "" {
		log.Fatal("codexm3btestserver: CODEX_M3B_LIVE=1 but CODEX_M3B_LIVE_LOGIN_JSON is empty; inject the real subscription login blob {\"access_token\":...,\"refresh_token\":...}")
	}
	var login struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(raw), &login); err != nil {
		log.Fatalf("codexm3btestserver: parse CODEX_M3B_LIVE_LOGIN_JSON: %v", err)
	}
	if login.AccessToken == "" || login.RefreshToken == "" {
		log.Fatal("codexm3btestserver: CODEX_M3B_LIVE_LOGIN_JSON must carry a non-empty access_token and refresh_token")
	}
	// Production-equivalent per-provider-call cap (mirrors cmd/server's
	// WithPerRequestTimeout(codexProviderRequestTimeout)) so the acceptance measures the real budget.
	// CODEX_M3B_LIVE_RELAX_TIMEOUTS=1 drops the cap as an explicit diagnostic mode only.
	client := codexauth.NewClient(codexauth.WithPerRequestTimeout(2500 * time.Millisecond))
	if os.Getenv("CODEX_M3B_LIVE_RELAX_TIMEOUTS") == "1" {
		client = codexauth.NewClient()
	}
	return &liveSubscription{
		accessToken:  login.AccessToken,
		refreshToken: login.RefreshToken,
		client:       client,
	}
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

	// nil unless CODEX_M3B_LIVE=1, in which case it fatals here on a missing/incomplete
	// real login blob rather than seeding a canary and reading green.
	live := buildLiveSubscription()

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

	seed, err := seedFixtures(ctx, pool, q, box, wsvc, live)
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
	// Plain HTTP is intentional for this test-only socket. Container mode runs only runtime
	// synthetic credentials on the script-owned internal network, and the same disposable
	// Docker controller can already inspect every peer's environment and logs. Ephemeral TLS
	// would add PKI plumbing without adding a trust boundary or testing production transport.

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
		Live:         live != nil,
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

// seedFixtures builds the fixture graph as worker_codex_livedb_test.go does, under ONE owner
// user and ONE worker (so the printed contract carries a single worker token).
// AuthorizeCodexCredentialOp resolves the alias/account by the WORKER's user id, so the
// worker, every run and every secret must share one owner.
//
// In the DEFAULT mode (live == nil) it seeds BOTH a subscription-mode run and an api_key-mode
// run, every credential-shaped value a CANARY string. In LIVE mode (live != nil) it seeds the
// subscription arm from the real injected login and SKIPS seedAPIKey (subscription-only), so
// the contract's APIKey stays the zero value.
func seedFixtures(ctx context.Context, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, wsvc *workersvc.Service, live *liveSubscription) (seedResult, error) {
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

	sub, err := seedSubscription(ctx, pool, q, box, wsvc, ownerID, repoID, workerID, live)
	if err != nil {
		return seedResult{}, fmt.Errorf("subscription fixture: %w", err)
	}
	// Live mode is subscription-only: seedAPIKey is skipped and the contract's APIKey stays
	// the zero value (empty run_id/capability), which the live lifecycle harness tolerates.
	var api apiKeyContract
	if live == nil {
		api, err = seedAPIKey(ctx, pool, q, box, wsvc, ownerID, repoID, workerID)
		if err != nil {
			return seedResult{}, fmt.Errorf("api_key fixture: %w", err)
		}
	}

	return seedResult{workerToken: joinToken, subscription: sub, apiKey: api}, nil
}

// seedSubscription mirrors the subscription arm of worker_codex_livedb_test.go: a codex_auth
// alias sealing a canary login blob, a provider account with a verified canary identity
// tuple, a linked credential state, a claimed run frozen to the binding, and a minted
// per-claim capability. It also wires the in-process refresh fake to return the frozen
// tuple so a coordinated refresh advances.
func seedSubscription(ctx context.Context, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, wsvc *workersvc.Service, ownerID, repoID, workerID uuid.UUID, live *liveSubscription) (subscriptionContract, error) {
	if live != nil {
		return seedSubscriptionLive(ctx, pool, q, box, wsvc, ownerID, repoID, workerID, live)
	}
	providerUserID := "canary-provider-" + uuid.NewString()
	chatGPTAccountID := "canary-account-" + uuid.NewString()
	accessToken, err := fakeChatGPTAccessToken(chatGPTAccountID, uuid.NewString())
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("build access token: %w", err)
	}
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
	rotatedAccessToken, err := fakeChatGPTAccessToken(chatGPTAccountID, uuid.NewString())
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("build rotated access token: %w", err)
	}
	wsvc.SetCodexRefresh(&fakeCodexRefresh{
		newAccessToken: rotatedAccessToken,
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

// resolveLiveIdentity establishes the login's canonical identity tuple with a NONROTATING read,
// refreshing first when the injected access token is already expired. ChatGPT access tokens are
// short-lived, so a 401 on the identity read is expected and recoverable: it mints a fresh access
// token from the durable refresh token (a single codexauth.Refresh), then discovers with it. It
// returns the tuple plus the tokens to seal (rotated when a refresh happened). Only an invalid
// refresh token or an unreachable provider fails here, before any run is seeded.
func resolveLiveIdentity(ctx context.Context, client *codexauth.Client, accessToken, refreshToken string) (codexauth.Identity, string, string, error) {
	id, err := client.DiscoverIdentity(ctx, accessToken)
	if err == nil {
		return id, accessToken, refreshToken, nil
	}
	var authErr *codexauth.AuthError
	if !errors.As(err, &authErr) || !authErr.Unauthorized() {
		// Not a 401: the token authenticated but identity did not resolve (e.g. ErrIdentityIncomplete).
		// A refresh cannot help, so log the usage-response SHAPE (key names + field presence only,
		// never values) to reveal whether the provider moved or omitted the identity fields.
		diagnoseUsageShape(ctx, accessToken)
		return codexauth.Identity{}, "", "", fmt.Errorf("discover live identity: %w", err)
	}
	// The injected access token is expired/invalid (401). Mint a fresh one from the durable refresh
	// token, then discover with it. Refresh MAY rotate the refresh token; seal whichever it returns.
	res, err := client.Refresh(ctx, refreshToken)
	if err != nil {
		return codexauth.Identity{}, "", "", fmt.Errorf("refresh live login after a 401 on identity (invalid refresh token? re-run 'codex login'): %w", err)
	}
	newRefresh := refreshToken
	if res.RefreshToken != nil && *res.RefreshToken != "" {
		newRefresh = *res.RefreshToken
	}
	id, err = client.DiscoverIdentity(ctx, res.AccessToken)
	if err != nil {
		diagnoseUsageShape(ctx, res.AccessToken)
		return codexauth.Identity{}, "", "", fmt.Errorf("discover live identity after refresh: %w", err)
	}
	return id, res.AccessToken, newRefresh, nil
}

// diagnoseUsageShape logs the SHAPE of the /wham/usage response (its top-level key names, the HTTP
// status, and whether account_id/user_id are present and non-empty) when identity discovery cannot
// resolve the tuple. It logs field NAMES and booleans ONLY, never any value, so a maintainer can
// tell a moved/renamed field or an empty usage record from a real outage without exposing the token
// or any account data. It is best-effort: any failure is logged and swallowed.
func diagnoseUsageShape(ctx context.Context, accessToken string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexauth.DefaultUsageBaseURL+"/wham/usage", nil)
	if err != nil {
		log.Printf("codexm3btestserver: diag: build usage request: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		log.Printf("codexm3btestserver: diag: usage request: %v", err)
		return
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort diagnostic
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		log.Printf("codexm3btestserver: diag: /wham/usage status=%d body is not a JSON object (len=%d)", resp.StatusCode, len(body))
		return
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	nonEmpty := func(k string) bool {
		v := string(top[k])
		return len(top[k]) > 0 && v != `""` && v != "null"
	}
	log.Printf("codexm3btestserver: diag: /wham/usage status=%d top-level keys=%v account_id_present=%v user_id_present=%v",
		resp.StatusCode, keys, nonEmpty("account_id"), nonEmpty("user_id"))
}

// seedSubscriptionLive seeds the subscription arm from the maintainer-injected REAL Codex
// login (CODEX_M3B_LIVE=1). It mirrors seedSubscription's credential graph, but every value
// except the two injected tokens comes from the provider, not a canary:
//
//   - resolveLiveIdentity establishes the login's canonical (provider_user_id, workspace_account_id)
//     tuple with a NONROTATING read, refreshing first if the injected access token is already
//     expired, so a stale access token (they are short-lived) is recovered via the durable refresh
//     token before any run is seeded.
//   - the real {access_token,refresh_token} blob is sealed and stored; it lives only in memory
//     and the sealed DB row, never in a file.
//   - the REAL codexauth client is wired into the coordinated refresher, so POST /codex/refresh
//     performs a real oauth rotation (the canary arm wires an in-process fake instead).
func seedSubscriptionLive(ctx context.Context, pool *pgxpool.Pool, q *store.Queries, box *secretbox.Box, wsvc *workersvc.Service, ownerID, repoID, workerID uuid.UUID, live *liveSubscription) (subscriptionContract, error) {
	id, accessToken, refreshToken, err := resolveLiveIdentity(ctx, live.client, live.accessToken, live.refreshToken)
	if err != nil {
		return subscriptionContract{}, err
	}

	login := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q}`, accessToken, refreshToken)
	sealedLogin, err := box.Seal([]byte(login))
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("seal live login: %w", err)
	}
	secretRow, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
		UserID: ownerID, Kind: store.KindCodexAuth, Label: "m3b-subscription-live", WantDefault: true,
		Ciphertext: sealedLogin, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("insert live codex secret: %w", err)
	}

	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: ownerID, ProviderUserID: id.ProviderUserID, WorkspaceAccountID: id.WorkspaceAccountID,
		SealedLogin: sealedLogin, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("insert live provider account: %w", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secretRow.ID, UserID: ownerID, Status: "staging",
	}); err != nil {
		return subscriptionContract{}, fmt.Errorf("insert live credential state: %w", err)
	}
	linked, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secretRow.ID,
		UserID: ownerID, MaterialRevision: 0,
	})
	if err != nil {
		return subscriptionContract{}, fmt.Errorf("link live credential state: %w", err)
	}
	if linked != 1 {
		return subscriptionContract{}, fmt.Errorf("link live credential state affected %d rows, want 1", linked)
	}

	runID := uuid.New()
	if err := exec(ctx, pool, `INSERT INTO runs
		(id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		VALUES ($1, $2, $3, 'issue', 1, 'm3b subscription live', 'm3b subscription live body', 'claimed', $4)`,
		runID, ownerID, repoID, workerID); err != nil {
		return subscriptionContract{}, fmt.Errorf("insert live run: %w", err)
	}
	if err := wsvc.FreezeCodexBinding(ctx, ownerID, runID, secretRow.ID, "subscription"); err != nil {
		return subscriptionContract{}, fmt.Errorf("freeze live binding: %w", err)
	}

	capability, err := mintCapability(ctx, q, runID, workerID, "m3b-sub-live-cap-")
	if err != nil {
		return subscriptionContract{}, err
	}

	// Wire the REAL codexauth client so POST /codex/refresh runs a real coordinated oauth
	// rotation against the provider.
	wsvc.SetCodexRefresh(live.client)

	return subscriptionContract{
		RunID:            runID.String(),
		Capability:       capability,
		ChatGPTAccountID: id.WorkspaceAccountID,
		Generation:       account.Generation,
	}, nil
}

// fakeChatGPTAccessToken builds the syntactically valid unsigned JWT shape pinned Codex
// requires for its external chatgptAuthTokens login. Codex parses these claims but does not
// verify a signature on this caller-supplied external-auth path. The random id keeps refresh
// material distinct, and every value exists only inside this throwaway test server.
func fakeChatGPTAccessToken(accountID, tokenID string) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{
		"email": "m3b@example.test",
		"jti":   "canary-" + tokenID,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "pro",
			"chatgpt_account_id": accountID,
		},
	})
	if err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode(header) + "." + encode(payload) + "." + encode([]byte("test-signature")), nil
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
