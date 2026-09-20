package workersvc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1209 M4, DELIVERABLE A: the REAL-HTTP end-to-end proof. Unlike the M2 usage tests
// (codexusage_livedb_test.go), which inject an in-process fakeUsageClient and never touch
// the wire, this drives CollectCodexAccountUsage through the PRODUCTION *codexauth.Client
// against a real net/http/httptest server serving /wham/usage. The point is the one thing
// no lower layer shows: a row written by the REAL client shape reaches the REAL store,
// AND the production redirect-refusal + body-limit path is exercised rather than bypassed.
//
// The client is pointed at the test server through the cleanest EXISTING seam — no
// production change to codexauth: WithHTTPDoer injects a real *http.Client that (a) carries
// the SAME CheckRedirect: http.ErrUseLastResponse the production NewClient sets on its
// default doer (so a 3xx surfaces as a refused non-2xx, header un-leaked), and (b) uses a
// tiny RoundTripper that rewrites only the request's scheme+host to the test server while
// leaving ReadUsage's own io.LimitReader body bound and json decode entirely intact. Every
// bound under test (redirect, body limit, decode-fail-closed) is the production code's.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// rewriteToTestServer is a RoundTripper that sends every request to the test server's
// scheme+host, keeping the path (so the handler still matches /backend-api/wham/usage).
// It wraps http.DefaultTransport and changes nothing else, so the *http.Client above it —
// its redirect policy in particular — behaves exactly as production's does.
type rewriteToTestServer struct {
	scheme string
	host   string
	inner  http.RoundTripper
}

func (rt rewriteToTestServer) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = rt.scheme
	r2.URL.Host = rt.host
	r2.Host = rt.host
	return rt.inner.RoundTrip(r2)
}

// realCodexUsageClient builds a production *codexauth.Client whose network seam is a real
// *http.Client carrying the SAME CheckRedirect the production NewClient sets, pointed at the
// given test server via the URL-rewriting RoundTripper.
func realCodexUsageClient(serverURL string) (*codexauth.Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{
		Transport: rewriteToTestServer{scheme: u.Scheme, host: u.Host, inner: http.DefaultTransport},
		// The SAME policy the production NewClient sets on its default doer: refuse EVERY
		// redirect so ReadUsage's custom ChatGPT-Account-Id header is never forwarded across
		// a cross-host 3xx. WithHTTPDoer replaces the whole doer, so this must be set here.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return codexauth.NewClient(codexauth.WithHTTPDoer(httpClient)), nil
}

// codexStoredBuckets reads back the persisted snapshot for one account through the REAL
// owner read query and decodes its buckets JSONB, so an assertion is against exactly what a
// client would receive.
func codexStoredBuckets(t *testing.T, env codexTestEnv, userID, accountID uuid.UUID) ([]apitypes.CodexRateLimitBucketDTO, store.GetCodexAccountRateLimitsForUserRow, bool) {
	t.Helper()
	rows, err := env.q.GetCodexAccountRateLimitsForUser(env.ctx, userID)
	if err != nil {
		t.Fatalf("read back snapshot: %v", err)
	}
	for _, row := range rows {
		if row.ProviderAccountID != accountID {
			continue
		}
		var buckets []apitypes.CodexRateLimitBucketDTO
		if len(row.Buckets) > 0 {
			if uerr := json.Unmarshal(row.Buckets, &buckets); uerr != nil {
				t.Fatalf("decode stored buckets: %v", uerr)
			}
		}
		return buckets, row, true
	}
	return nil, store.GetCodexAccountRateLimitsForUserRow{}, false
}

// codexPersistReading mirrors the poller's writeReading (codexusagepoller/engine.go): it
// marshals the collected buckets and installs them under the observed (generation,
// credential_revision) fence with the SAME success upsert the production poller uses. It
// returns the upsert's affected-row count so a caller can assert authority held.
func codexPersistReading(t *testing.T, env codexTestEnv, userID, accountID uuid.UUID, reading CodexUsageReading) int64 {
	t.Helper()
	raw, err := json.Marshal(reading.Buckets)
	if err != nil {
		t.Fatalf("marshal buckets: %v", err)
	}
	n, err := env.q.UpsertCodexAccountRateLimits(env.ctx, store.UpsertCodexAccountRateLimitsParams{
		UserID:                     userID,
		ProviderAccountID:          accountID,
		Buckets:                    raw,
		ObservedGeneration:         reading.ObservedGeneration,
		ObservedCredentialRevision: reading.ObservedCredentialRevision,
		AttemptStatus:              "ok",
	})
	if err != nil {
		t.Fatalf("upsert reading: %v", err)
	}
	return n
}

// mustBucketPrimaryPercent extracts the codex bucket's primary window used_percent from a
// bucket set, failing the test if the shape is not the single-codex-bucket-with-primary one.
func mustBucketPrimaryPercent(t *testing.T, buckets []apitypes.CodexRateLimitBucketDTO) float64 {
	t.Helper()
	if len(buckets) != 1 || buckets[0].ID != "codex" {
		t.Fatalf("buckets = %+v, want a single %q bucket", buckets, "codex")
	}
	if buckets[0].Primary == nil || buckets[0].Primary.UsedPercent == nil {
		t.Fatalf("bucket has no primary used_percent: %+v", buckets[0])
	}
	return *buckets[0].Primary.UsedPercent
}

func TestCollectCodexAccountUsageRealHTTPEndToEndLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	// newUsageFixture seeds a linked account with a real sealed committed login; the fake it
	// wires is only for reconcile-time and is immediately replaced by the REAL client below.
	fx := newUsageFixture(t, env, &fakeUsageClient{}, true)

	// A programmable handler the phases below swap. It also records the credentials the real
	// client presented, so the happy phase proves openCodexAccountLogin's token + the
	// persisted workspace id reached the wire.
	var (
		handler       http.HandlerFunc
		gotAuthHeader string
		gotAccountID  string
		gotPath       string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("ChatGPT-Account-Id")
		gotPath = r.URL.Path
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	client, err := realCodexUsageClient(srv.URL)
	if err != nil {
		t.Fatalf("build real client: %v", err)
	}
	// Swap the fixture's fake for the production client: openCodexAccountLogin still yields
	// fx.access (the committed login), which the real client sends as the bearer to srv.
	fx.svc.codexRefresh = client

	validPrimary := func(pct string) string {
		return `{"user_id":"` + fx.providerUserID + `","rate_limit":{"allowed":true,"limit_reached":false,` +
			`"primary_window":{"used_percent":` + pct + `,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1893456000}}}`
	}

	// (a) A valid rate_limit payload → normalized buckets, persisted and read back.
	t.Run("valid_payload_persists_and_reads_back", func(t *testing.T) {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, validPrimary("42"))
		}
		reading, cerr := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
		if cerr != nil {
			t.Fatalf("collect through the real client: %v", cerr)
		}
		if got := mustBucketPrimaryPercent(t, reading.Buckets); got != 42 {
			t.Fatalf("returned reading used_percent = %v, want 42", got)
		}
		if reading.ObservedGeneration != 0 || reading.ObservedCredentialRevision != 0 {
			t.Fatalf("observed = (%d,%d), want (0,0)", reading.ObservedGeneration, reading.ObservedCredentialRevision)
		}
		// The real client presented the committed access token and the PERSISTED workspace id.
		if gotAuthHeader != "Bearer "+fx.access {
			t.Fatalf("server saw Authorization %q, want the committed access token", gotAuthHeader)
		}
		if gotAccountID != fx.workspaceID {
			t.Fatalf("server saw ChatGPT-Account-Id %q, want the persisted workspace id %q", gotAccountID, fx.workspaceID)
		}
		if gotPath != "/backend-api/wham/usage" {
			t.Fatalf("server saw path %q, want /backend-api/wham/usage", gotPath)
		}

		if n := codexPersistReading(t, env, fx.userID, fx.accountID, reading); n != 1 {
			t.Fatalf("upsert affected %d rows, want 1", n)
		}
		stored, _, ok := codexStoredBuckets(t, env, fx.userID, fx.accountID)
		if !ok {
			t.Fatal("no snapshot row was persisted for the account")
		}
		if got := mustBucketPrimaryPercent(t, stored); got != 42 {
			t.Fatalf("stored used_percent = %v, want 42", got)
		}
	})

	// (b) A 3xx from the server is REFUSED: the client does not follow it (header un-leaked),
	// ReadUsage sees the non-2xx and CollectCodexAccountUsage returns a failure — no reading,
	// no upsert, last-good (42) preserved.
	t.Run("redirect_refused", func(t *testing.T) {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://evil.example.invalid/wham/usage")
			w.WriteHeader(http.StatusFound)
		}
		reading, cerr := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
		if got := mustUsageFailure(t, cerr).Kind; got != CodexUsageFailTransient {
			t.Fatalf("kind = %v, want transient (a refused 3xx)", got)
		}
		if len(reading.Buckets) != 0 {
			t.Fatalf("a refused redirect must return no reading, got %+v", reading.Buckets)
		}
		stored, _, ok := codexStoredBuckets(t, env, fx.userID, fx.accountID)
		if !ok || mustBucketPrimaryPercent(t, stored) != 42 {
			t.Fatalf("last-good reading (42) must survive a refused redirect, stored=%+v", stored)
		}
	})

	// (c) An oversized body is rejected: ReadUsage's io.LimitReader truncates it, the decode
	// fails, and the reading is refused — last-good (42) preserved.
	t.Run("oversized_body_rejected", func(t *testing.T) {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// A valid JSON prefix followed by ~1.5MB of padding, well past the 1MB body cap:
			// the LimitReader cuts the padding mid-string, so the decode hits an unexpected EOF.
			_, _ = io.WriteString(w, `{"user_id":"`+fx.providerUserID+`","rate_limit":{"primary_window":{"used_percent":7}},"_pad":"`)
			_, _ = io.WriteString(w, strings.Repeat("A", 1_500_000))
			_, _ = io.WriteString(w, `"}`)
		}
		reading, cerr := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
		if got := mustUsageFailure(t, cerr).Kind; got != CodexUsageFailTransient {
			t.Fatalf("kind = %v, want transient (oversized body)", got)
		}
		if len(reading.Buckets) != 0 {
			t.Fatalf("an oversized body must return no reading, got %+v", reading.Buckets)
		}
		stored, _, ok := codexStoredBuckets(t, env, fx.userID, fx.accountID)
		if !ok || mustBucketPrimaryPercent(t, stored) != 42 {
			t.Fatalf("last-good reading (42) must survive an oversized body, stored=%+v", stored)
		}
	})

	// (d1) Additive unknown fields (top-level and inside a window) are IGNORED: the reading
	// still normalizes to the codex bucket, and the new value persists.
	t.Run("unknown_fields_ignored", func(t *testing.T) {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"user_id":"`+fx.providerUserID+`","future_top_level":{"x":1},`+
				`"rate_limit":{"allowed":true,"limit_reached":false,"future_flag":"ignore me",`+
				`"primary_window":{"used_percent":55,"limit_window_seconds":18000,"future_field":123}}}`)
		}
		reading, cerr := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
		if cerr != nil {
			t.Fatalf("additive unknown fields must be ignored, got: %v", cerr)
		}
		if got := mustBucketPrimaryPercent(t, reading.Buckets); got != 55 {
			t.Fatalf("used_percent = %v, want 55 (unknown fields ignored)", got)
		}
		if n := codexPersistReading(t, env, fx.userID, fx.accountID, reading); n != 1 {
			t.Fatalf("upsert affected %d rows, want 1", n)
		}
		stored, _, ok := codexStoredBuckets(t, env, fx.userID, fx.accountID)
		if !ok || mustBucketPrimaryPercent(t, stored) != 55 {
			t.Fatalf("stored used_percent = %v, want 55", stored)
		}
	})

	// (d2) An invalid KNOWN field fails closed: a type mismatch on used_percent makes the
	// decode error, the reading is refused, and — mirroring the poller's health-only write —
	// the last-good reading (55) is preserved while the attempt is recorded failed.
	t.Run("invalid_known_field_fails_closed", func(t *testing.T) {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"user_id":"`+fx.providerUserID+`","rate_limit":{"primary_window":{"used_percent":"not-a-number"}}}`)
		}
		reading, cerr := fx.svc.CollectCodexAccountUsage(env.ctx, fx.userID, fx.accountID)
		if got := mustUsageFailure(t, cerr).Kind; got != CodexUsageFailTransient {
			t.Fatalf("kind = %v, want transient (invalid known field)", got)
		}
		if len(reading.Buckets) != 0 {
			t.Fatalf("an invalid known field must return no reading, got %+v", reading.Buckets)
		}
		// The poller records the health-only failure (which PRESERVES buckets + last-success).
		n, ferr := env.q.RecordCodexAccountPollFailure(env.ctx, store.RecordCodexAccountPollFailureParams{
			UserID:                     fx.userID,
			ProviderAccountID:          fx.accountID,
			AttemptStatus:              CodexUsageFailTransient.String(),
			AttemptError:               "transient usage read failure",
			ObservedGeneration:         0,
			ObservedCredentialRevision: 0,
		})
		if ferr != nil || n != 1 {
			t.Fatalf("record failure: n=%d err=%v", n, ferr)
		}
		stored, row, ok := codexStoredBuckets(t, env, fx.userID, fx.accountID)
		if !ok || mustBucketPrimaryPercent(t, stored) != 55 {
			t.Fatalf("last-good reading (55) must survive an invalid known field, stored=%+v", stored)
		}
		if !row.AttemptStatus.Valid || row.AttemptStatus.String != CodexUsageFailTransient.String() {
			t.Fatalf("attempt_status = %v, want the recorded transient failure", row.AttemptStatus)
		}
		if !row.LastSuccessAt.Valid {
			t.Fatal("last_success_at must be preserved (the reading is stale, not gone)")
		}
	})
}
