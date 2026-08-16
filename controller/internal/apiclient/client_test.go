package apiclient

import (
	"context"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

func TestPollSendsBearerOnAGetAndParsesDesiredState(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"workers":[{"id":"w1","template":"base","size":"s","generation":2,"join_token":"uzw_t"}]}`)
	}))
	defer srv.Close()

	resp, err := New(srv.URL, "the-token", 5*time.Second, nil, testLogger()).Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if gotAuth != "Bearer the-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/api/controller/poll" {
		t.Fatalf("path = %q", gotPath)
	}
	// A GET with no body: the poll is a pure read, and the ack that once justified a
	// POST is gone (delivery is proved by the worker's own registration).
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", gotMethod)
	}
	if len(gotBody) != 0 {
		t.Fatalf("request body = %q, want none", gotBody)
	}
	if len(resp.Workers) != 1 || resp.Workers[0].ID != "w1" || resp.Workers[0].Generation != 2 {
		t.Fatalf("workers = %+v", resp.Workers)
	}
	if resp.Workers[0].JoinToken == nil || *resp.Workers[0].JoinToken != "uzw_t" {
		t.Fatalf("join token = %v", resp.Workers[0].JoinToken)
	}
}

// A non-2xx must be an error, never an empty fleet — the controller reads desired
// state as authoritative, so a silently-empty 401 would read as "delete everything".
func TestPollTreatsNon200AsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"invalid controller token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	resp, err := New(srv.URL, "wrong", 5*time.Second, nil, testLogger()).Poll(context.Background())
	if err == nil {
		t.Fatal("want an error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want it to name the status", err)
	}
	if len(resp.Workers) != 0 {
		t.Fatal("a failed poll must carry no desired state")
	}
}

// The response body can contain join-token plaintext, so a decode failure must not
// quote it — json's own error messages include the offending input.
func TestPollDecodeErrorDoesNotLeakTheBody(t *testing.T) {
	const secret = "uzw_super-secret-plaintext"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"workers":[{"join_token":"`+secret+`"`) // truncated, unparseable
	}))
	defer srv.Close()

	_, err := New(srv.URL, "t", 5*time.Second, nil, testLogger()).Poll(context.Background())
	if err == nil {
		t.Fatal("want a decode error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("decode error leaked the token plaintext: %v", err)
	}
}

// The hop that matters: a real TLS handshake against a certificate signed by a CA
// the system does not trust, verified through the pool the controller mounts.
//
// httptest's TLS server generates its own certificate and hands it back via
// srv.Certificate(), which stands in for cert-manager's leaf; feeding it to the
// pool is exactly what the chart does with the CA it mounts.
func TestPollVerifiesTheAPICertificateAgainstTheGivenCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"workers":[]}`)
	}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	if _, err := New(srv.URL, "tok", 5*time.Second, pool, testLogger()).Poll(context.Background()); err != nil {
		t.Fatalf("Poll over TLS with the issuing CA pooled: %v", err)
	}
}

// The other half of the same claim: without the CA, the handshake FAILS. A client
// that silently accepted the peer would make the encryption theatre — this hop's
// responses carry a decrypted forge PAT and Anthropic token.
func TestPollRefusesAnAPICertificateItCannotVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"workers":[]}`)
	}))
	defer srv.Close()

	// An empty pool: a well-formed trust anchor set that simply does not contain
	// this server's issuer. nil would fall back to the system roots, which reject it
	// too but for a less specific reason.
	_, err := New(srv.URL, "tok", 5*time.Second, x509.NewCertPool(), testLogger()).Poll(context.Background())
	if err == nil {
		t.Fatal("Poll: want a certificate verification failure, got nil (the client trusted an unverifiable api)")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("Poll error = %v, want a certificate verification failure", err)
	}
}

// testLogger discards: these tests assert on responses and errors, not on the
// once-per-process notice Report emits when the api has no /status endpoint.
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Report (PRD #113 M3). The 404 case is the one with a design decision behind it.

func TestReportPostsTheFleetWithBearerAuthAndReadsNoResponse(t *testing.T) {
	var gotAuth, gotPath, gotMethod, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	report := protocol.StatusReport{
		ReportedAt:          time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		PollIntervalSeconds: 10,
		WorkerImageTag:      "0.11.7",
		Workers:             []protocol.WorkerStatus{{ID: "w1", Phase: protocol.PhaseStuck, PodPhase: "Pending"}},
	}
	if err := New(srv.URL, "the-token", 5*time.Second, nil, testLogger()).Report(context.Background(), report); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/controller/status" {
		t.Errorf("path = %q, want /api/controller/status", gotPath)
	}
	if gotAuth != "Bearer the-token" {
		t.Errorf("authorization = %q, want the fleet bearer credential", gotAuth)
	}
	if gotType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotType)
	}
	if !strings.Contains(string(gotBody), `"worker_image_tag":"0.11.7"`) {
		t.Errorf("body does not carry the rolled tag (Decision 9's hosted target):\n%s", gotBody)
	}
	if !strings.Contains(string(gotBody), `"phase":"stuck"`) {
		t.Errorf("body does not carry the per-worker phase:\n%s", gotBody)
	}
}

// A 404 is SUCCESS. An api without the endpoint is the expected steady state until
// the api side ships, and the normal state under skew in either direction; it is also
// what hosting-disabled looks like, since the route is absent rather than refusing.
// Returning an error would make the controller complain, every ten seconds, about a
// condition it was designed to tolerate.
func TestReportTreatsA404AsSuccessAndLogsItOnceNotEveryTick(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var logged strings.Builder
	c := New(srv.URL, "t", 5*time.Second, nil, slog.New(slog.NewTextHandler(&logged, nil)))
	for i := 0; i < 3; i++ {
		if err := c.Report(context.Background(), protocol.StatusReport{}); err != nil {
			t.Fatalf("Report attempt %d returned an error for a 404; an api without the endpoint must be "+
				"tolerated, not reported as a failure: %v", i+1, err)
		}
	}
	if calls != 3 {
		t.Errorf("server saw %d calls, want 3: a 404 must not stop the controller trying again", calls)
	}
	if n := strings.Count(logged.String(), "no /api/controller/status endpoint"); n != 1 {
		t.Errorf("the missing-endpoint notice was logged %d times across 3 reports, want exactly 1. "+
			"At a 10s cadence an unguarded line is ~8640 entries a day describing something nobody can act on.", n)
	}
}

// Any OTHER non-2xx is a real error. Flattening a 500 into the same silence as a 404
// would hide a broken endpoint behind a tolerance meant for an absent one.
func TestReportReturnsAnErrorForNon404Failures(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusBadRequest, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		err := New(srv.URL, "t", 5*time.Second, nil, testLogger()).Report(context.Background(), protocol.StatusReport{})
		srv.Close()
		if err == nil {
			t.Errorf("Report returned nil for %d; only 404 is tolerated", code)
		}
	}
}
