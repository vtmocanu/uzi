package uzicli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// --- FakeClient ---

func TestFakeGetRunNotFound(t *testing.T) {
	f := &FakeClient{RunByID: map[string]apitypes.RunDTO{}}
	_, err := f.GetRun(context.Background(), "missing")
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != ExitNotFound {
		t.Fatalf("GetRun err = %v, want ExitError{ExitNotFound}", err)
	}
}

// The fake's RunReview is tri-state: a present-but-nil entry is a
// visible-but-unjudged run (nil, nil) — NOT the 404 path an absent key takes.
func TestFakeReviewTriState(t *testing.T) {
	f := &FakeClient{Reviews: map[string]*apitypes.ReviewDTO{
		"judged":   {Verdict: "good"},
		"unjudged": nil,
	}}
	if rv, _, err := f.RunReview(context.Background(), "judged"); err != nil || rv == nil {
		t.Fatalf("judged: rv=%v err=%v", rv, err)
	}
	if rv, _, err := f.RunReview(context.Background(), "unjudged"); err != nil || rv != nil {
		t.Fatalf("unjudged: want (nil,nil), got rv=%v err=%v", rv, err)
	}
	if _, _, err := f.RunReview(context.Background(), "absent"); ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("absent: exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

// PendingJudges is a SEPARATE axis from the tri-state above (PRD #119): it decides
// whether a verdict is on its way, never whether the run is found. Both nil-review
// cases — with and without a pending judge — stay exit-0 reads, and a pending judge
// rides alongside a PRESENT review too (a re-judge in flight).
func TestFakePendingJudgeIsIndependentOfReview(t *testing.T) {
	f := &FakeClient{
		Reviews: map[string]*apitypes.ReviewDTO{
			"pending":  nil,
			"unjudged": nil,
			"rejudge":  {Verdict: "good"},
		},
		PendingJudges: map[string]*apitypes.PendingJudgeDTO{
			"pending": {State: "scheduled"},
			"rejudge": {State: "running"},
		},
	}
	rv, pj, err := f.RunReview(context.Background(), "pending")
	if err != nil || rv != nil || pj == nil || pj.State != "scheduled" {
		t.Fatalf("pending: want (nil, scheduled, nil), got rv=%v pj=%+v err=%v", rv, pj, err)
	}
	if rv, pj, err := f.RunReview(context.Background(), "unjudged"); err != nil || rv != nil || pj != nil {
		t.Fatalf("unjudged: want (nil,nil,nil), got rv=%v pj=%+v err=%v", rv, pj, err)
	}
	rv, pj, err = f.RunReview(context.Background(), "rejudge")
	if err != nil || rv == nil || pj == nil || pj.State != "running" {
		t.Fatalf("rejudge: want (review, running, nil), got rv=%v pj=%+v err=%v", rv, pj, err)
	}
	// A pending judge does NOT rescue an absent Reviews key: only that map decides
	// found-vs-404, exactly as before this field existed.
	f.PendingJudges["absent"] = &apitypes.PendingJudgeDTO{State: "running"}
	if _, _, err := f.RunReview(context.Background(), "absent"); ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("absent: exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

// The fake's RunInputs mirrors the owner-only endpoint: a present key returns the
// queue (empty allowed), an absent key is the non-owner/unknown 404 path.
func TestFakeRunInputs(t *testing.T) {
	body := "steer me"
	f := &FakeClient{InputsByID: map[string][]apitypes.SteerInputDTO{
		"mine":  {{ID: 1, Body: &body}},
		"empty": {},
	}}
	if in, err := f.RunInputs(context.Background(), "mine"); err != nil || len(in) != 1 {
		t.Errorf("mine: in=%v err=%v", in, err)
	}
	if in, err := f.RunInputs(context.Background(), "empty"); err != nil || len(in) != 0 {
		t.Errorf("empty: in=%v err=%v", in, err)
	}
	if _, err := f.RunInputs(context.Background(), "foreign"); ExitCodeFor(err) != ExitNotFound {
		t.Errorf("foreign: exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

func TestFakeErrPropagates(t *testing.T) {
	sentinel := Exitf(ExitAuth, "nope")
	f := &FakeClient{Err: sentinel}
	if _, err := f.ListRuns(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("ListRuns err = %v, want sentinel", err)
	}
	if _, err := f.Whoami(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("Whoami err = %v, want sentinel", err)
	}
}

func TestFakeHappyPath(t *testing.T) {
	f := &FakeClient{
		Runs:    []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "r1"}}},
		Workers: []apitypes.WorkerDTO{{ID: "w1"}},
		Repos:   []apitypes.RepoDTO{{ID: "p1"}},
	}
	if runs, err := f.ListRuns(context.Background()); err != nil || len(runs) != 1 {
		t.Errorf("ListRuns = %v, %v", runs, err)
	}
	if ws, err := f.ListWorkers(context.Background()); err != nil || len(ws) != 1 {
		t.Errorf("ListWorkers = %v, %v", ws, err)
	}
	if ps, err := f.ListRepos(context.Background()); err != nil || len(ps) != 1 {
		t.Errorf("ListRepos = %v, %v", ps, err)
	}
}

// --- HTTPClient (against an httptest server) ---

// newTestClient points an HTTPClient at srv. httptest listens on 127.0.0.1, so
// its http URL is loopback and passes the https-only guard (the compose-laptop
// exception is exactly this shape).
func newTestClient(srv *httptest.Server) *HTTPClient {
	return &HTTPClient{BaseURL: srv.URL, Token: "uzc_test", HTTP: srv.Client()}
}

func TestHTTPClientWhoami(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer uzc_test" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"a@b.c","is_admin":true},"prd_label":"PRD"}`))
	}))
	defer srv.Close()
	u, err := newTestClient(srv).Whoami(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "u1" || u.Email != "a@b.c" || !u.IsAdmin {
		t.Errorf("user = %+v", u)
	}
}

// A 200 {"review": null} is a visible-but-unjudged run: (nil, nil), exit 0. It
// must NOT be turned into a 404.
func TestHTTPClientReviewNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"review":null}`))
	}))
	defer srv.Close()
	rv, pj, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if err != nil || rv != nil || pj != nil {
		t.Fatalf("null review: want (nil,nil,nil), got rv=%v pj=%v err=%v", rv, pj, err)
	}
}

func TestHTTPClientReviewPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"review":{"id":"rv1","verdict":"needs_work","status":"failed"}}`))
	}))
	defer srv.Close()
	rv, _, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if err != nil || rv == nil || rv.Verdict != "needs_work" || rv.Status != "failed" {
		t.Fatalf("present review: rv=%+v err=%v", rv, err)
	}
}

// The response is a TWO-key envelope since PRD #119: {"review", "pending_judge"},
// each independently nullable. This pins all three shapes the server can send —
// pending over no review (the auto-judge case), pending over a present review (a
// re-judge in flight), and an explicit `"pending_judge": null` — because the field
// exists precisely to tell the first apart from the third, and a decode that dropped
// it would leave the CLI printing "not judged" at both.
func TestHTTPClientReviewPendingJudge(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRv    bool
		wantState string // "" = want no pending judge
	}{
		{"pending over unjudged", `{"review":null,"pending_judge":{"state":"scheduled","enqueued_at":"2026-07-20T10:00:00Z"}}`, false, "scheduled"},
		{"pending over a review", `{"review":{"id":"rv1","verdict":"good"},"pending_judge":{"state":"running","enqueued_at":"2026-07-20T10:00:00Z"}}`, true, "running"},
		{"explicit null pending", `{"review":{"id":"rv1","verdict":"good"},"pending_judge":null}`, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			rv, pj, err := newTestClient(srv).RunReview(context.Background(), "r1")
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if (rv != nil) != tc.wantRv {
				t.Errorf("review present = %v, want %v", rv != nil, tc.wantRv)
			}
			if tc.wantState == "" {
				if pj != nil {
					t.Fatalf("pending_judge = %+v, want nil", pj)
				}
				return
			}
			if pj == nil {
				t.Fatalf("pending_judge = nil, want state %q", tc.wantState)
			}
			if pj.State != tc.wantState {
				t.Errorf("state = %q, want %q", pj.State, tc.wantState)
			}
			if pj.EnqueuedAt.IsZero() {
				t.Errorf("enqueued_at did not decode: %+v", pj)
			}
		})
	}
}

// A real 404 (run absent or not visible) is exit 4 — reserved, distinct from the
// null-review exit-0 path above.
func TestHTTPClientReview404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"run not found"}`))
	}))
	defer srv.Close()
	_, _, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

// RunInputs decodes the {"inputs": [...]} envelope into the DTO list, carrying
// consumed_at through as a pointer (null → nil = Queued, set → Delivered).
func TestHTTPClientRunInputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"inputs":[{"id":2,"body":"b2","created_at":"2026-07-20T10:00:00Z","consumed_at":"2026-07-20T10:01:00Z"},{"id":1,"body":"b1","created_at":"2026-07-20T09:00:00Z","consumed_at":null}]}`))
	}))
	defer srv.Close()
	in, err := newTestClient(srv).RunInputs(context.Background(), "r1")
	if err != nil || len(in) != 2 {
		t.Fatalf("RunInputs: in=%+v err=%v", in, err)
	}
	if in[0].ID != 2 || in[0].ConsumedAt == nil {
		t.Errorf("first row should be consumed (Delivered): %+v", in[0])
	}
	if in[1].ID != 1 || in[1].ConsumedAt != nil {
		t.Errorf("second row should be unconsumed (Queued): %+v", in[1])
	}
}

// A malformed 200 body must fail cleanly with the decode exit code (1), never
// panic.
func TestHTTPClientMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user": not-json`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).Whoami(context.Background())
	if ExitCodeFor(err) != ExitGeneric {
		t.Fatalf("malformed: exit = %d, want %d (err=%v)", ExitCodeFor(err), ExitGeneric, err)
	}
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("malformed err is not *ExitError: %v", err)
	}
}

// TestHTTPClientStatusMapping pins the HTTP-status → exit-code contract.
func TestHTTPClientStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusBadRequest, ExitUsage},
		{http.StatusUnauthorized, ExitAuth},
		{http.StatusForbidden, ExitAuth},
		{http.StatusNotFound, ExitNotFound},
		{http.StatusConflict, ExitConflict},
		{http.StatusInternalServerError, ExitUnreachable},
		{http.StatusBadGateway, ExitUnreachable},
		{http.StatusTeapot, ExitGeneric}, // an unenumerated 4xx falls to generic
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"x"}`))
		}))
		got := ExitCodeFor(func() error { _, e := newTestClient(srv).Whoami(context.Background()); return e }())
		srv.Close()
		if got != tc.want {
			t.Errorf("status %d: exit = %d, want %d", tc.status, got, tc.want)
		}
	}
}

// A 403 carries an actionable admin-scope hint (M7b).
func TestHTTPClient403AdminHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"admin required"}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).AdminListUsers(context.Background())
	if ExitCodeFor(err) != ExitAuth {
		t.Fatalf("exit = %d, want %d", ExitCodeFor(err), ExitAuth)
	}
	if !strings.Contains(err.Error(), "admin-scoped token") {
		t.Errorf("403 message = %q, want an admin-scope hint", err.Error())
	}
}

// Reject a non-https, non-loopback base URL BEFORE any request is sent — the
// credential-leak guard. The token must never travel in cleartext to a foreign
// host.
func TestHTTPClientRejectsNonHTTPS(t *testing.T) {
	c := &HTTPClient{BaseURL: "http://uzi.example.com", Token: "uzc_secret", HTTP: http.DefaultClient}
	_, err := c.Whoami(context.Background())
	if ExitCodeFor(err) != ExitUsage {
		t.Fatalf("exit = %d, want %d", ExitCodeFor(err), ExitUsage)
	}
	if !strings.Contains(err.Error(), "https") {
		t.Errorf("message = %q, want an https guard message", err.Error())
	}
}

// A TLS endpoint that 302-redirects to plain http must NOT cause the client to
// replay the bearer token in cleartext. The shipped CheckRedirect refuses every
// redirect, so the http target is never contacted and the Authorization header
// can never leak. Mirrors the auditor's proof of the https→http scheme-downgrade
// token leak: without CheckRedirect, Go follows the hop and forwards Bearer over
// http. The https guard in newRequest only vets the INITIAL URL, not redirect
// hops — this is the layer that closes the gap.
func TestHTTPClientRefusesRedirect(t *testing.T) {
	var httpSawAuth atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			httpSawAuth.Store(true)
		}
		_, _ = w.Write([]byte(`{"user":{"id":"leaked"}}`))
	}))
	defer plain.Close()

	// A TLS server that 302s every request down to the plain-http endpoint (same
	// loopback host, scheme downgraded https→http).
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/api/auth/me", http.StatusFound)
	}))
	defer tlsSrv.Close()

	// The SHIPPED client (its CheckRedirect), but trusting the test's TLS cert.
	c := NewHTTPClient(Settings{URL: tlsSrv.URL, Token: "uzc_secret"})
	c.HTTP.Transport = tlsSrv.Client().Transport

	_, err := c.Whoami(context.Background())
	if err == nil {
		t.Fatal("want an error when the API redirects (the hop must not be followed), got nil")
	}
	if ExitCodeFor(err) != ExitUnreachable {
		t.Errorf("redirect refusal exit = %d, want %d (transport failure)", ExitCodeFor(err), ExitUnreachable)
	}
	if httpSawAuth.Load() {
		t.Fatal("Authorization: Bearer leaked to the http redirect target in cleartext")
	}
}

func TestHTTPClientEmptyURL(t *testing.T) {
	c := &HTTPClient{BaseURL: "", Token: "uzc_x", HTTP: http.DefaultClient}
	_, err := c.Whoami(context.Background())
	if ExitCodeFor(err) != ExitUsage {
		t.Fatalf("empty URL: exit = %d, want %d", ExitCodeFor(err), ExitUsage)
	}
}

// TestHTTPClientOnlyReturnsExitError is the reviewer-mandated invariant: no
// method of the live client ever returns a bare error — every failure path
// (unreachable, 5xx, malformed body) is an *ExitError, so a raw error can never
// leak to ExitCodeFor and be misclassified.
func TestHTTPClientOnlyReturnsExitError(t *testing.T) {
	// Every method as a uniform func(*HTTPClient) error closure.
	calls := []struct {
		name string
		fn   func(*HTTPClient) error
	}{
		{"whoami", func(c *HTTPClient) error { _, e := c.Whoami(context.Background()); return e }},
		{"list-runs", func(c *HTTPClient) error { _, e := c.ListRuns(context.Background()); return e }},
		{"get-run", func(c *HTTPClient) error { _, e := c.GetRun(context.Background(), "r1"); return e }},
		{"run-logs", func(c *HTTPClient) error { _, e := c.RunLogs(context.Background(), "r1", 0); return e }},
		{"run-review", func(c *HTTPClient) error { _, _, e := c.RunReview(context.Background(), "r1"); return e }},
		{"run-inputs", func(c *HTTPClient) error { _, e := c.RunInputs(context.Background(), "r1"); return e }},
		{"list-workers", func(c *HTTPClient) error { _, e := c.ListWorkers(context.Background()); return e }},
		{"list-repos", func(c *HTTPClient) error { _, e := c.ListRepos(context.Background()); return e }},
		{"admin-users", func(c *HTTPClient) error { _, e := c.AdminListUsers(context.Background()); return e }},
		{"admin-runs", func(c *HTTPClient) error { _, e := c.AdminListRuns(context.Background()); return e }},
		{"admin-workers", func(c *HTTPClient) error { _, e := c.AdminListWorkers(context.Background()); return e }},
		{"admin-cli-tokens", func(c *HTTPClient) error { _, e := c.AdminListCLITokens(context.Background()); return e }},
		{"admin-usage", func(c *HTTPClient) error { _, e := c.AdminUsage(context.Background()); return e }},
		{"admin-rate-limits", func(c *HTTPClient) error { _, e := c.AdminRateLimits(context.Background()); return e }},
		{"start-cli-auth", func(c *HTTPClient) error { _, e := c.StartCLIAuth(context.Background(), "ch", "desc"); return e }},
		{"poll-cli-auth", func(c *HTTPClient) error { _, e := c.PollCLIAuth(context.Background(), "req", "ver"); return e }},
		{"create-run", func(c *HTTPClient) error { _, e := c.CreateRun(context.Background(), "p1", 7, nil, nil); return e }},
		{"submit-run-input", func(c *HTTPClient) error {
			_, e := c.SubmitRunInput(context.Background(), "r1", "cancel", "", nil)
			return e
		}},
	}

	// Three failure surfaces: 5xx, malformed 200 body, and unreachable server.
	fiveHundred := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fiveHundred.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("this is not json at all <<<"))
	}))
	defer garbage.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // closed → connection refused

	surfaces := map[string]*HTTPClient{
		"5xx":         {BaseURL: fiveHundred.URL, Token: "uzc_x", HTTP: fiveHundred.Client()},
		"malformed":   {BaseURL: garbage.URL, Token: "uzc_x", HTTP: garbage.Client()},
		"unreachable": {BaseURL: deadURL, Token: "uzc_x", HTTP: &http.Client{}},
	}

	for _, call := range calls {
		for sName, c := range surfaces {
			err := call.fn(c)
			if err == nil {
				t.Errorf("%s/%s: want error, got nil", call.name, sName)
				continue
			}
			var ee *ExitError
			if !errors.As(err, &ee) {
				t.Errorf("%s/%s: err is not *ExitError: %T %v", call.name, sName, err, err)
			}
		}
	}
}

// TestBuildInfoIsGatedByCredentialSafeBase pins the property that makes the https
// guard REQUIRED on this call rather than merely consistent with the others.
//
// GET /api/version is an unauthenticated ROUTE, so it is tempting to argue the
// guard is defence-in-depth here and skip it. The argument is wrong, and the third
// arm below is why: newRequest attaches `Authorization: Bearer <token>` to EVERY
// request whose client holds one, this call included. The route needs no
// credential; the request carries one anyway. Skipping credentialSafeBase for
// BuildInfo would therefore put a real uzc_ token on the wire in cleartext the
// first time someone ran `uzi version --url http://…`.
//
// Nothing else pins this. Every `uzi version` command test uses FakeClient, which
// has no BaseURL and never reaches the guard — so the whole M4 suite would stay
// green if a future change built a bare http.Client for this one call.
//
// The loopback arm is what makes the refusal attributable: without it, a guard
// that blanket-refused http would pass the first arm while breaking every compose
// user.
func TestBuildInfoIsGatedByCredentialSafeBase(t *testing.T) {
	t.Run("non-loopback http is refused before any request is sent", func(t *testing.T) {
		c := NewHTTPClient(Settings{URL: "http://uzi.example.com", Token: "uzc_secret"})

		_, err := c.BuildInfo(context.Background())
		if err == nil {
			t.Fatal("want a refusal for a plain-http remote, got nil — the bearer token would have gone out in cleartext")
		}
		if ExitCodeFor(err) != ExitUsage {
			t.Errorf("exit = %d, want %d", ExitCodeFor(err), ExitUsage)
		}
		// The guard's own message, not just any error: a connection failure would
		// also be non-nil, and would mean the request WAS attempted.
		if !strings.Contains(err.Error(), "refusing to send credentials") {
			t.Errorf("error is not the credential guard's: %v", err)
		}
	})

	t.Run("loopback http is allowed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"version":"0.11.12","founded":"2026-07-03"}`))
		}))
		defer srv.Close()

		c := NewHTTPClient(Settings{URL: srv.URL, Token: "uzc_secret"})
		got, err := c.BuildInfo(context.Background())
		if err != nil {
			t.Fatalf("loopback http must be allowed (it is the compose stack): %v", err)
		}
		if got.Version != "0.11.12" {
			t.Errorf("version = %q, want %q", got.Version, "0.11.12")
		}
	})

	t.Run("the request carries the bearer token, which is why the guard is required", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"version":"0.11.12","founded":"2026-07-03"}`))
		}))
		defer srv.Close()

		c := NewHTTPClient(Settings{URL: srv.URL, Token: "uzc_SECRETTOKENVALUE"})
		if _, err := c.BuildInfo(context.Background()); err != nil {
			t.Fatalf("BuildInfo: %v", err)
		}
		if gotAuth != "Bearer uzc_SECRETTOKENVALUE" {
			t.Fatalf("Authorization = %q, want the bearer token.\n"+
				"If this now fails because the token is NOT attached, the guard's justification has "+
				"changed and the doc comments on BuildInfo and Client.BuildInfo must be re-derived — "+
				"do not simply delete this arm.", gotAuth)
		}
	})
}

// TestCreateRunWireBodyOmitsAbsentWaitOnLimit asserts the tri-state on the WIRE, which
// is the only place it is actually decided.
//
// The command-level test proves the CLI resolves the flag to nil; this proves nil then
// produces a body with NO `wait_on_limit` key at all. Those are two different failures
// with the same visible symptom: a `map[string]any{"wait_on_limit": waitOnLimit}` built
// without a nil guard, or a non-pointer field, marshals `"wait_on_limit": null` or
// `false` — both of which the server reads as a decision the user never made, and
// neither of which the command-level test can see.
//
// Asserted on the RAW BYTES rather than by decoding into a struct: decoding into a
// *bool maps an explicit `null` and an absent key to the same nil, so it would agree
// with the broken implementation.
func TestCreateRunWireBodyOmitsAbsentWaitOnLimit(t *testing.T) {
	tr := true
	fa := false
	for _, tc := range []struct {
		name        string
		in          *bool
		wantPresent bool
		wantJSON    string
	}{
		{"absent omits the key entirely", nil, false, ""},
		{"explicit true", &tr, true, `"wait_on_limit":true`},
		{"explicit false is SENT, not omitted", &fa, true, `"wait_on_limit":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b := make([]byte, r.ContentLength)
				_, _ = io.ReadFull(r.Body, b)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"run":{"id":"r1","status":"queued"}}`))
			}))
			defer srv.Close()

			if _, err := newTestClient(srv).CreateRun(context.Background(), "p1", 42, tc.in, nil); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if !strings.Contains(body, `"issue_iid":42`) {
				t.Errorf("body lost issue_iid: %s", body)
			}
			if got := strings.Contains(body, "wait_on_limit"); got != tc.wantPresent {
				t.Errorf("wait_on_limit present = %v, want %v; body = %s\n"+
					"An absent flag must send NO key — the server stamps the run from the owner's Settings "+
					"default when the field is missing, and reads null or false as an explicit decision.",
					got, tc.wantPresent, body)
			}
			if tc.wantJSON != "" && !strings.Contains(body, tc.wantJSON) {
				t.Errorf("body = %s, want it to contain %s", body, tc.wantJSON)
			}
		})
	}
}

// TestCreateRunWireBodySeededPlan pins PRD #209's seed → wire mapping at the RAW body,
// which the command-level (fake-client) tests cannot see. The load-bearing case is the
// first: a nil seed must send NEITHER plan_md nor agent_selection, so a run created
// without --plan-file is byte-identical to a pre-#209 create (Success Criterion 2).
// Asserted on the bytes, not a decoded struct, because omitempty is precisely what a
// decode-into-pointer would erase.
func TestCreateRunWireBodySeededPlan(t *testing.T) {
	for _, tc := range []struct {
		name         string
		seed         *CreateRunSeed
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:       "nil seed omits every seed key (byte-identical to a pre-#209 create)",
			seed:       nil,
			wantAbsent: []string{"plan_md", "agent_selection", "planned_commit", "require_base"},
		},
		{
			name:         "plan, no roster/staleness: plan_md present, the rest omitted",
			seed:         &CreateRunSeed{PlanMD: "do the thing"},
			wantContains: []string{`"plan_md":"do the thing"`},
			wantAbsent:   []string{"agent_selection", "planned_commit", "require_base"},
		},
		{
			name: "plan + roster: both present, selection is {source, exclusions}",
			seed: &CreateRunSeed{
				PlanMD:    "do it",
				Selection: &apitypes.AgentSelection{Source: "own", Exclusions: []string{"tester"}},
			},
			wantContains: []string{`"plan_md":"do it"`, `"agent_selection":{"source":"own","exclusions":["tester"]}`},
			wantAbsent:   []string{"planned_commit", "require_base"},
		},
		{
			// PRD #209 M4: planned_commit + require_base ride the wire when set.
			name: "plan + staleness guard: planned_commit and require_base present",
			seed: &CreateRunSeed{
				PlanMD:        "do it",
				PlannedCommit: "abc123def456",
				RequireBase:   true,
			},
			wantContains: []string{`"planned_commit":"abc123def456"`, `"require_base":true`},
		},
		{
			// planned_commit alone (warn-only): the commit rides, require_base is omitted
			// (omitempty on a false bool), so a warn-default seed sends no require_base key.
			name: "plan + planned_commit only: require_base omitted (warn default)",
			seed: &CreateRunSeed{
				PlanMD:        "do it",
				PlannedCommit: "abc123def456",
			},
			wantContains: []string{`"planned_commit":"abc123def456"`},
			wantAbsent:   []string{"require_base"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b := make([]byte, r.ContentLength)
				_, _ = io.ReadFull(r.Body, b)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"run":{"id":"r1","status":"queued"}}`))
			}))
			defer srv.Close()

			if _, err := newTestClient(srv).CreateRun(context.Background(), "p1", 42, nil, tc.seed); err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(body, want) {
					t.Errorf("body = %s, want it to contain %s", body, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(body, absent) {
					t.Errorf("body = %s, want it to OMIT %q (a nil/absent field must send no key)", body, absent)
				}
			}
		})
	}
}
