package fetcher

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// testServiceToken is a fake fetcher service token, assembled at runtime.
var testServiceToken = strings.Repeat("s", 8) + "-service-token"

// fakeAPI is a TLS api answering the fetcher control routes.
func fakeAPI(t *testing.T, h http.HandlerFunc) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func strictDecode[T any](t *testing.T, r *http.Request) T {
	t.Helper()
	var v T
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Errorf("api received %s: %v", b, err)
	}
	return v
}

func TestHTTPControlBegin(t *testing.T) {
	var status atomic.Int32
	var body atomic.Value
	srv, pool := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != BeginPath {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testServiceToken {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		in := strictDecode[apitypes.FetcherBeginRequest](t, r)
		if in.Credential != testCredential || in.URL != "https://docs.example.com/a" {
			t.Errorf("begin body %+v", in)
		}
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, body.Load().(string))
	})
	c, err := NewHTTPControl(srv.URL, testServiceToken, pool, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	begin := func(st int, b string) (Admission, error) {
		status.Store(int32(st)) //nolint:gosec // G115: an HTTP status fits in int32.
		body.Store(b)
		return c.Begin(context.Background(), testCredential, "https://docs.example.com/a")
	}

	adm, err := begin(200, `{"reservation_id":"r1","entries":["docs.example.com","*.example.org"],"max_bytes":1024}`)
	if err != nil || adm.ReservationID != "r1" || adm.MaxBytes != 1024 || len(adm.Entries) != 2 {
		t.Fatalf("admitted: %+v %v", adm, err)
	}
	if _, err := begin(403, `{"error":"x","reason":"credential_invalid"}`); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("403 credential_invalid: %v", err)
	}
	var ar *AdmissionRefusedError
	if _, err := begin(429, `{"error":"x","reason":"run_files_exhausted"}`); !errors.As(err, &ar) || ar.Reason != "run_files_exhausted" {
		t.Fatalf("429: %v", err)
	}
	if _, err := begin(429, "{\"error\":\"x\",\"reason\":\"Bad\u202eCode\"}"); !errors.As(err, &ar) || ar.Reason != "unspecified" {
		t.Fatalf("429 with an unsafe code: %v", err)
	}
	for _, bad := range []struct {
		status int
		body   string
	}{
		{401, `{"error":"x","reason":"credential_invalid"}`}, // the SERVICE token was refused
		{403, `{"error":"x","reason":"other"}`},
		{500, `{}`},
		{200, `{"reservation_id":"","entries":[],"max_bytes":1}`},
		{200, `{"reservation_id":"r","entries":[],"max_bytes":0}`},
		{200, `{"reservation_id":"r","entries":[],"max_bytes":1,"run_id":"x"}`},
		{200, `not json`},
		{302, ``},
	} {
		adm, err := begin(bad.status, bad.body)
		if err == nil || errors.Is(err, ErrCredentialInvalid) || errors.As(err, &ar) {
			t.Errorf("%d %s: got %+v %v, want a control failure", bad.status, bad.body, adm, err)
		}
	}
}

func TestHTTPControlComplete(t *testing.T) {
	var status atomic.Int32
	var got atomic.Value
	srv, pool := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != CompletePath || r.Header.Get("Authorization") != "Bearer "+testServiceToken {
			t.Errorf("%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		got.Store(strictDecode[apitypes.FetcherCompleteRequest](t, r))
		w.WriteHeader(int(status.Load()))
	})
	c, err := NewHTTPControl(srv.URL+"/", testServiceToken, pool, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	rec := AttemptRecord{
		ReservationID: "r1", URL: "https://docs.example.com/a", FinalURL: "https://docs.example.com/b",
		Verdict: VerdictAllowed, HTTPStatus: 200, ContentType: "application/pdf", Bytes: 10,
		SHA256: strings.Repeat("ab", 32), StartedAt: at, FinishedAt: at.Add(time.Second),
	}
	status.Store(204)
	if err := c.Complete(context.Background(), testCredential, rec); err != nil {
		t.Fatal(err)
	}
	sent := got.Load().(apitypes.FetcherCompleteRequest)
	if sent.Credential != testCredential || sent.ReservationID != "r1" || sent.FinalURL != rec.FinalURL ||
		sent.Verdict != VerdictAllowed || sent.SHA256 != rec.SHA256 || !sent.FinishedAt.Equal(rec.FinishedAt) {
		t.Fatalf("complete body %+v", sent)
	}
	for _, st := range []int32{400, 403, 500, 302} {
		status.Store(st)
		if err := c.Complete(context.Background(), testCredential, rec); err == nil {
			t.Errorf("status %d: want an error", st)
		}
	}
}

// The api's certificate is verified against the configured CA only.
func TestHTTPControlVerifiesAPICertificate(t *testing.T) {
	srv, _ := fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	wrong := newTestCA(t)
	c, err := NewHTTPControl(srv.URL, testServiceToken, wrong.pool, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(context.Background(), testCredential, AttemptRecord{}); err == nil {
		t.Fatal("an api certificate from an untrusted CA was accepted")
	}
	c, err = NewHTTPControl(srv.URL, testServiceToken, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(context.Background(), testCredential, AttemptRecord{}); err == nil {
		t.Fatal("a self-signed api certificate verified against the system roots")
	}
}

// The control hop ignores proxy environment too: the service token and run credentials
// cross it.
func TestHTTPControlIgnoresProxyEnvironment(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(k, proxy.URL)
	}
	srv, pool := fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	c, err := NewHTTPControl(srv.URL, testServiceToken, pool, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Complete(context.Background(), testCredential, AttemptRecord{}); err != nil {
		t.Fatal(err)
	}
	if proxied.Load() != 0 {
		t.Fatal("the control client used the proxy")
	}
}

func TestNewHTTPControlValidation(t *testing.T) {
	for _, u := range []string{"", "api:8443", "ftp://api", "http://api:8080", "https://u:p@api", "https://api/path", "https://api?x=1"} {
		if _, err := NewHTTPControl(u, testServiceToken, nil, 0); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if _, err := NewHTTPControl("https://api:8443", "", nil, 0); err == nil {
		t.Error("an empty service token was accepted")
	}
}
