package uzicli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if rv, err := f.RunReview(context.Background(), "judged"); err != nil || rv == nil {
		t.Fatalf("judged: rv=%v err=%v", rv, err)
	}
	if rv, err := f.RunReview(context.Background(), "unjudged"); err != nil || rv != nil {
		t.Fatalf("unjudged: want (nil,nil), got rv=%v err=%v", rv, err)
	}
	if _, err := f.RunReview(context.Background(), "absent"); ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("absent: exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
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
		w.Write([]byte(`{"user":{"id":"u1","email":"a@b.c","is_admin":true},"prd_label":"PRD"}`))
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
		w.Write([]byte(`{"review":null}`))
	}))
	defer srv.Close()
	rv, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if err != nil || rv != nil {
		t.Fatalf("null review: want (nil,nil), got rv=%v err=%v", rv, err)
	}
}

func TestHTTPClientReviewPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"review":{"id":"rv1","verdict":"needs_work","status":"failed"}}`))
	}))
	defer srv.Close()
	rv, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if err != nil || rv == nil || rv.Verdict != "needs_work" || rv.Status != "failed" {
		t.Fatalf("present review: rv=%+v err=%v", rv, err)
	}
}

// A real 404 (run absent or not visible) is exit 4 — reserved, distinct from the
// null-review exit-0 path above.
func TestHTTPClientReview404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"run not found"}`))
	}))
	defer srv.Close()
	_, err := newTestClient(srv).RunReview(context.Background(), "r1")
	if ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
}

// A malformed 200 body must fail cleanly with the decode exit code (1), never
// panic.
func TestHTTPClientMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"user": not-json`))
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
			w.Write([]byte(`{"error":"x"}`))
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
		w.Write([]byte(`{"error":"admin required"}`))
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
		{"run-review", func(c *HTTPClient) error { _, e := c.RunReview(context.Background(), "r1"); return e }},
		{"list-workers", func(c *HTTPClient) error { _, e := c.ListWorkers(context.Background()); return e }},
		{"list-repos", func(c *HTTPClient) error { _, e := c.ListRepos(context.Background()); return e }},
		{"admin-users", func(c *HTTPClient) error { _, e := c.AdminListUsers(context.Background()); return e }},
		{"admin-runs", func(c *HTTPClient) error { _, e := c.AdminListRuns(context.Background()); return e }},
		{"admin-workers", func(c *HTTPClient) error { _, e := c.AdminListWorkers(context.Background()); return e }},
		{"admin-usage", func(c *HTTPClient) error { _, e := c.AdminUsage(context.Background()); return e }},
		{"admin-rate-limits", func(c *HTTPClient) error { _, e := c.AdminRateLimits(context.Background()); return e }},
	}

	// Three failure surfaces: 5xx, malformed 200 body, and unreachable server.
	fiveHundred := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fiveHundred.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not json at all <<<"))
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
