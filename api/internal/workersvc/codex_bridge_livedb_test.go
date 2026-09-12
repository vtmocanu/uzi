package workersvc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1171 M1 (ships DARK): prove the coordinated refresher through the REAL production
// codexauth.Client (built over a fake httpDoer) injected via the REAL SetCodexRefresh
// setter — "prove the route through real server construction rather than direct service
// tests alone". The dark M4 suite already proves the refresh state machine with an
// in-process fake CodexRefreshClient; here the seam under test is the production client
// wiring itself: request assembly, JSON and JWT-claim decode, the single-call advance, and
// that an api_key run drives zero provider calls.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// fakeCodexProviderDoer answers both codexauth provider surfaces but counts each so the
// coordinated callback can prove it touches only POST /oauth/token. GET /wham/usage remains
// implemented as a tripwire: a subscription advance wants one exchange and zero usage reads;
// an api_key run wants zero of both. Safe for concurrent use.
type fakeCodexProviderDoer struct {
	mu         sync.Mutex
	oauthCalls int
	usageCalls int

	newAccess  string
	newRefresh string
	identity   codexauth.Identity // returned by /wham/usage so the re-verify matches the account
}

func (d *fakeCodexProviderDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.HasSuffix(req.URL.Path, "/oauth/token"):
		d.oauthCalls++
		return codexJSONResp(fmt.Sprintf(`{"access_token":%q,"refresh_token":%q}`, d.newAccess, d.newRefresh)), nil
	case strings.HasSuffix(req.URL.Path, "/wham/usage"):
		d.usageCalls++
		return codexJSONResp(fmt.Sprintf(`{"user_id":%q,"account_id":%q}`, d.identity.ProviderUserID, d.identity.WorkspaceAccountID)), nil
	default:
		return nil, fmt.Errorf("fakeCodexProviderDoer: unexpected provider path %q", req.URL.Path)
	}
}

func (d *fakeCodexProviderDoer) calls() (oauth, usage int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.oauthCalls, d.usageCalls
}

func codexJSONResp(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func freshIdentityAccessToken(t *testing.T, accountID, userID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_user_id":    userID,
			"user_id":            userID,
		},
	})
	if err != nil {
		t.Fatalf("marshal fresh access-token claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

// TestCoordinatedRefreshThroughProductionCodexauthClientLiveDB drives a first refresh
// (observedGeneration == current == 0 → ADVANCE) end to end through the production
// codexauth.Client. It proves the account durably advances to generation 1, the freshly
// exchanged access token is released, and the callback makes exactly one oauth exchange
// with zero usage GETs.
func TestCoordinatedRefreshThroughProductionCodexauthClientLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newSubscriptionFixture(t, env)

	// Resolve the account's frozen tuple so the exchanged JWT can carry matching claims.
	st := mustState(t, env, f.userID, f.aliasID)
	if !st.ProviderAccountID.Valid {
		t.Fatal("subscription alias must link a provider account")
	}
	accountID := uuid.UUID(st.ProviderAccountID.Bytes)
	acct := env.mustAccount(t, f.userID, accountID)
	if acct.Generation != 0 {
		t.Fatalf("fixture account generation = %d, want 0 (initial login)", acct.Generation)
	}

	rotatedAccess := freshIdentityAccessToken(t, acct.WorkspaceAccountID, acct.ProviderUserID)
	doer := &fakeCodexProviderDoer{
		newAccess:  rotatedAccess,
		newRefresh: codexToken("refresh-rotated"),
		identity:   codexauth.Identity{ProviderUserID: acct.ProviderUserID, WorkspaceAccountID: acct.WorkspaceAccountID},
	}
	// The REAL production client over a fake transport, injected through the REAL setter.
	f.svc.SetCodexRefresh(codexauth.NewClient(codexauth.WithHTTPDoer(doer)))

	capability := env.mintCap(t, f.runID, f.workerID)
	// observedGeneration 0 == the account's current generation → ADVANCE (the token expired
	// at the current generation), which is the path that actually calls the provider.
	res, err := f.svc.CoordinatedCodexRefresh(env.ctx, f.wkr, f.runID, capability, uuid.New(), 0)
	if err != nil {
		t.Fatalf("CoordinatedCodexRefresh: %v", err)
	}
	if res.Outcome != CodexRefreshAdvanced {
		t.Fatalf("outcome = %v, want advanced", res.Outcome)
	}
	if res.Generation != 1 {
		t.Fatalf("result generation = %d, want 1", res.Generation)
	}
	if res.AccessToken != rotatedAccess {
		t.Fatalf("released token = %q, want the freshly-exchanged %q", res.AccessToken, rotatedAccess)
	}
	if res.AccessToken == f.accessToken {
		t.Fatal("refresh released the pre-rotation (stale) access token")
	}
	if res.ChatGPTAccountID != acct.WorkspaceAccountID {
		t.Fatalf("refresh chatgpt account id = %q, want verified %q", res.ChatGPTAccountID, acct.WorkspaceAccountID)
	}
	if oauth, usage := doer.calls(); oauth != 1 || usage != 0 {
		t.Fatalf("provider calls = (oauth %d, usage %d), want (1, 0) for one advance", oauth, usage)
	}

	// The account durably advanced: generation 1, and its committed login is the new one.
	after := env.mustAccount(t, f.userID, accountID)
	if after.Generation != 1 {
		t.Fatalf("account generation = %d, want 1 after the durable commit", after.Generation)
	}
	if got := env.accountBlob(t, f.userID, accountID).AccessToken; got != rotatedAccess {
		t.Fatalf("committed login access token = %q, want the rotated %q", got, rotatedAccess)
	}
}

// TestApiKeyRefreshIssuesZeroProviderCallsLiveDB proves an api_key run's coordinated
// refresh is refused at the scope check (ScopeStartRefresh does not apply to a static key)
// and NEVER touches the provider — the "api_key mode performs zero refresh calls" invariant,
// asserted through the production client's own transport counter.
func TestApiKeyRefreshIssuesZeroProviderCallsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	staticKey := codexToken("sk")
	aliasID := env.seedStaticAPIKey(t, userID, "codex-key-"+uuid.NewString(), staticKey)
	runID := env.seedCodexRun(t, userID, workerID, repoID)

	svc := &Service{q: env.q, box: env.box}
	if err := svc.FreezeCodexBinding(env.ctx, userID, runID, aliasID, codexAuthModeAPIKey); err != nil {
		t.Fatalf("FreezeCodexBinding (api_key): %v", err)
	}
	doer := &fakeCodexProviderDoer{
		newAccess:  codexToken("should-never-be-issued"),
		newRefresh: codexToken("should-never-rotate"),
		identity:   codexauth.Identity{ProviderUserID: "u", WorkspaceAccountID: "a"},
	}
	svc.SetCodexRefresh(codexauth.NewClient(codexauth.WithHTTPDoer(doer)))
	wkr := store.Worker{ID: workerID, UserID: userID}

	capability := env.mintCap(t, runID, workerID)
	_, err := svc.CoordinatedCodexRefresh(env.ctx, wkr, runID, capability, uuid.New(), 0)
	if !errors.Is(err, ErrCodexScopeNotApplicable) {
		t.Fatalf("api_key refresh err = %v, want ErrCodexScopeNotApplicable", err)
	}
	if oauth, usage := doer.calls(); oauth != 0 || usage != 0 {
		t.Fatalf("api_key refresh made provider calls: (oauth %d, usage %d), want (0, 0)", oauth, usage)
	}
}
