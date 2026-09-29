package fetcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func decodeRefusal(t *testing.T, body []byte) apitypes.FetchErrorDTO {
	t.Helper()
	var e apitypes.FetchErrorDTO
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("refusal body %q: %v", body, err)
	}
	return e
}

func TestServerAllowedPDF(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(pdfHandler))
	ctl := newControl("docs.example.com")
	s := NewServer(fetcherFor(publicResolver(), ca, port), ctl, quietLog(), 0)
	rec := doFetch(t, s, testCredential, urlBody(t, "https://docs.example.com/guide.pdf"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	sum := sha256.Sum256(pdfBytes)
	wantSum := hex.EncodeToString(sum[:])
	h := rec.Header()
	if !bytes.Equal(rec.Body.Bytes(), pdfBytes) ||
		h.Get("Content-Type") != "application/pdf" ||
		h.Get("X-Uzi-Final-Url") != "https://docs.example.com/guide.pdf" ||
		h.Get("X-Uzi-Sha256") != wantSum ||
		h.Get("X-Uzi-Bytes") != strconv.Itoa(len(pdfBytes)) {
		t.Fatalf("response headers %v", h)
	}
	if len(ctl.completes) != 1 {
		t.Fatalf("completes = %d, want 1", len(ctl.completes))
	}
	c := ctl.completes[0]
	if c.Verdict != VerdictAllowed || c.Reason != "" || c.SHA256 != wantSum || c.Bytes != int64(len(pdfBytes)) ||
		c.ContentType != "application/pdf" || c.HTTPStatus != 200 || c.ReservationID != "res-1" ||
		c.URL != "https://docs.example.com/guide.pdf" || c.FinalURL != "https://docs.example.com/guide.pdf" ||
		c.StartedAt.IsZero() || c.FinishedAt.Before(c.StartedAt) {
		t.Fatalf("complete record %+v", c)
	}
	for _, cr := range ctl.creds {
		if cr != testCredential {
			t.Fatalf("credential forwarded as %q", cr)
		}
	}
}

// Every attempt Begin admitted produces exactly one Complete, allowed or refused, and the
// worker-facing status and reason match.
func TestServerOneCompletePerAttempt(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("private error page"))
		case "/redirect-off":
			http.Redirect(w, r, "https://evil.example.net/", http.StatusFound)
		case "/big":
			_, _ = w.Write(bytes.Repeat([]byte("b"), 2048))
		default:
			pdfHandler(w, r)
		}
	}))
	cases := []struct {
		url      string
		status   int
		reason   string
		verdict  string
		upstream int
	}{
		{"https://docs.example.com/ok", 200, "", VerdictAllowed, 200},
		{"https://evil.example.net/", 403, ReasonOffList, VerdictRefused, 0},
		{"http://docs.example.com/", 403, ReasonNotHTTPS, VerdictRefused, 0},
		{"https://docs.example.com/redirect-off", 403, ReasonRedirectOffList, VerdictRefused, 302},
		{"https://docs.example.com/404", 502, ReasonUpstreamStatus, VerdictRefused, 404},
		{"https://docs.example.com/big", 502, ReasonTooLarge, VerdictRefused, 200},
		{"https://127.0.0.1/", 403, ReasonIPLiteral, VerdictRefused, 0},
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			ctl := newControl("docs.example.com")
			ctl.maxBytes = 1024
			s := NewServer(fetcherFor(publicResolver(), ca, port), ctl, quietLog(), 0)
			rec := doFetch(t, s, testCredential, urlBody(t, c.url))
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body)
			}
			if len(ctl.begins) != 1 || len(ctl.completes) != 1 {
				t.Fatalf("begins=%d completes=%d, want 1 each", len(ctl.begins), len(ctl.completes))
			}
			got := ctl.completes[0]
			if got.Verdict != c.verdict || got.Reason != c.reason || got.HTTPStatus != c.upstream || got.URL != c.url {
				t.Fatalf("complete record %+v", got)
			}
			if c.verdict == VerdictRefused {
				e := decodeRefusal(t, rec.Body.Bytes())
				if e.Reason != c.reason || e.Error == "" {
					t.Fatalf("refusal %+v", e)
				}
				if strings.Contains(rec.Body.String(), "private error page") {
					t.Fatal("an upstream error body reached the worker")
				}
				if c.reason == ReasonUpstreamStatus && e.UpstreamStatus != 404 {
					t.Fatalf("upstream_status = %d", e.UpstreamStatus)
				}
				if got.SHA256 != "" || got.Bytes != 0 {
					t.Fatalf("a refused attempt carries content metadata: %+v", got)
				}
			}
		})
	}
}

// A failed source-log write refuses the fetch and returns no content (fail closed).
func TestServerCompleteFailureReturnsNoContent(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(pdfHandler))
	ctl := newControl("docs.example.com")
	ctl.completeErr = errors.New("api down")
	s := NewServer(fetcherFor(publicResolver(), ca, port), ctl, quietLog(), 0)
	rec := doFetch(t, s, testCredential, urlBody(t, "https://docs.example.com/guide.pdf"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("%PDF")) || rec.Header().Get("X-Uzi-Sha256") != "" {
		t.Fatal("content was returned although the log write failed")
	}
	if e := decodeRefusal(t, rec.Body.Bytes()); e.Reason != ReasonLogFailed {
		t.Fatalf("reason %q", e.Reason)
	}
	if len(ctl.completes) != 1 {
		t.Fatalf("completes = %d", len(ctl.completes))
	}
}

// The log write runs even when the worker went away mid-fetch.
func TestServerCompleteSurvivesCallerCancel(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(pdfHandler))
	var sawCancelled atomic.Bool
	ctl := &ctxCheckingControl{fakeControl: newControl("docs.example.com"), sawCancelled: &sawCancelled}
	s := NewServer(fetcherFor(publicResolver(), ca, port), ctl, quietLog(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	ctl.onBegin = cancel
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/fetch", strings.NewReader(urlBody(t, "https://docs.example.com/x")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testCredential)
	rec := &discardRecorder{header: http.Header{}}
	s.ServeHTTP(rec, req)
	if len(ctl.completes) != 1 {
		t.Fatalf("completes = %d, want 1", len(ctl.completes))
	}
	if sawCancelled.Load() {
		t.Fatal("Complete ran with the cancelled request context")
	}
	if ctl.completes[0].Verdict != VerdictRefused || ctl.completes[0].Reason != ReasonTimeout {
		t.Fatalf("record %+v", ctl.completes[0])
	}
}

type ctxCheckingControl struct {
	*fakeControl
	sawCancelled *atomic.Bool
	onBegin      func()
}

func (c *ctxCheckingControl) Begin(ctx context.Context, cred, url string) (Admission, error) {
	a, err := c.fakeControl.Begin(ctx, cred, url)
	c.onBegin()
	return a, err
}

func (c *ctxCheckingControl) Complete(ctx context.Context, cred string, rec AttemptRecord) error {
	if ctx.Err() != nil {
		c.sawCancelled.Store(true)
	}
	return c.fakeControl.Complete(ctx, cred, rec)
}

type discardRecorder struct {
	header http.Header
	code   int
}

func (d *discardRecorder) Header() http.Header         { return d.header }
func (d *discardRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardRecorder) WriteHeader(code int)        { d.code = code }

func TestServerBeginRefusals(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		reason string
		admRsn string
	}{
		{"credential", ErrCredentialInvalid, http.StatusUnauthorized, ReasonCredentialInvalid, ""},
		{"admission", &AdmissionRefusedError{Reason: "run_bytes_exhausted"}, http.StatusTooManyRequests, ReasonAdmissionRefused, "run_bytes_exhausted"},
		{"control-down", errors.New("connection refused"), http.StatusServiceUnavailable, ReasonControlUnavailable, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := publicResolver()
			ctl := newControl("docs.example.com")
			ctl.beginErr = c.err
			s := NewServer(fetcherFor(res, nil, 0), ctl, quietLog(), 0)
			rec := doFetch(t, s, testCredential, urlBody(t, "https://docs.example.com/"))
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d", rec.Code, c.status)
			}
			e := decodeRefusal(t, rec.Body.Bytes())
			if e.Reason != c.reason || e.AdmissionReason != c.admRsn {
				t.Fatalf("refusal %+v", e)
			}
			if len(ctl.completes) != 0 || res.total() != 0 {
				t.Fatal("a refused admission fetched or logged")
			}
		})
	}
}

// Request-shape errors are answered before the api is asked.
func TestServerRequestShape(t *testing.T) {
	long := "https://docs.example.com/" + strings.Repeat("a", MaxURLLen)
	cases := []struct {
		name, cred, body string
		status           int
		reason           string
	}{
		{"no-bearer", "", `{"url":"https://docs.example.com/"}`, 401, ReasonCredentialInvalid},
		{"unknown-field", testCredential, `{"url":"https://docs.example.com/","method":"POST"}`, 400, ReasonBadRequest},
		{"headers-field", testCredential, `{"url":"https://docs.example.com/","headers":{"Host":"evil"}}`, 400, ReasonBadRequest},
		{"trailing-value", testCredential, `{"url":"https://docs.example.com/"}{"url":"x"}`, 400, ReasonBadRequest},
		{"empty-url", testCredential, `{"url":""}`, 400, ReasonBadRequest},
		{"null", testCredential, `null`, 400, ReasonBadRequest},
		{"not-json", testCredential, `url=https://docs.example.com/`, 400, ReasonBadRequest},
		{"oversize-body", testCredential, `{"url":"` + strings.Repeat("a", MaxRequestBody) + `"}`, 400, ReasonBadRequest},
		{"url-too-long", testCredential, `{"url":"` + long + `"}`, 400, ReasonURLTooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctl := newControl("docs.example.com")
			s := NewServer(fetcherFor(publicResolver(), nil, 0), ctl, quietLog(), 0)
			rec := doFetch(t, s, c.cred, c.body)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d", rec.Code, c.status)
			}
			if e := decodeRefusal(t, rec.Body.Bytes()); e.Reason != c.reason {
				t.Fatalf("reason %q", e.Reason)
			}
			if len(ctl.begins) != 0 {
				t.Fatal("Begin was called for a malformed request")
			}
		})
	}
	// Only POST reaches the fetch route.
	s := NewServer(fetcherFor(publicResolver(), nil, 0), newControl(), quietLog(), 0)
	req, _ := http.NewRequest(http.MethodGet, "/v1/fetch?url=https://docs.example.com/", nil)
	rec := &discardRecorder{header: http.Header{}}
	s.ServeHTTP(rec, req)
	if rec.code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/fetch = %d", rec.code)
	}
}

func TestBearer(t *testing.T) {
	for h, ok := range map[string]bool{
		"Bearer abc":  true,
		"bearer abc":  true,
		"Bearer":      false,
		"Bearer ":     false,
		"Basic abc":   false,
		"Bearer a b":  false,
		"Bearer a\tb": false,
		"Bearer " + strings.Repeat("x", maxCredentialLen+1): false,
	} {
		if _, got := bearer(h); got != ok {
			t.Errorf("bearer(%q) ok = %v, want %v", h, got, ok)
		}
	}
}

func TestServerBusy(t *testing.T) {
	ca := newTestCA(t)
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-block
		pdfHandler(w, r)
	}))
	ctl := newControl("docs.example.com")
	s := NewServer(fetcherFor(publicResolver(), ca, port), ctl, quietLog(), 1)
	done := make(chan int)
	go func() {
		done <- doFetch(t, s, testCredential, urlBody(t, "https://docs.example.com/a")).Code
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first fetch never started")
	}
	rec := doFetch(t, s, testCredential, urlBody(t, "https://docs.example.com/b"))
	close(block)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first fetch status %d", code)
	}
	if rec.Code != http.StatusServiceUnavailable || decodeRefusal(t, rec.Body.Bytes()).Reason != ReasonBusy {
		t.Fatalf("second fetch %d %s", rec.Code, rec.Body)
	}
	if len(ctl.begins) != 1 {
		t.Fatalf("begins = %d; a busy refusal must not reserve", len(ctl.begins))
	}
}

func TestServerHealthz(t *testing.T) {
	ctl := newControl()
	s := NewServer(fetcherFor(publicResolver(), nil, 0), ctl, quietLog(), 0)
	req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	rec := &discardRecorder{header: http.Header{}}
	s.ServeHTTP(rec, req)
	if rec.code != 0 && rec.code != http.StatusOK {
		t.Fatalf("healthz %d", rec.code)
	}
	if len(ctl.begins) != 0 {
		t.Fatal("healthz called the api")
	}
}

func TestHostOfLogsHostOnly(t *testing.T) {
	if got := hostOf("https://docs.example.com/a?token=secret#x"); got != "docs.example.com" {
		t.Fatalf("hostOf = %q", got)
	}
}

// A site-controlled Content-Type is re-serialized before it reaches the worker and the
// source log, and X-Uzi-Final-Url carries url.URL.String() output of printable ASCII.
func TestServerNormalizesSiteControlledHeaders(t *testing.T) {
	ca := newTestCA(t)
	_, port := upstream(t, ca.validLeaf(t, "docs.example.com"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/garbage":
			w.Header().Set("Content-Type", "not a media type <script>")
		default:
			w.Header().Set("Content-Type", "Text/HTML; charset=\"a\u202eb\"; boundary=evil")
		}
		_, _ = w.Write([]byte("<p>hi</p>"))
	}))
	for path, want := range map[string]string{"/html": "text/html", "/garbage": "application/octet-stream"} {
		ctl := newControl("docs.example.com")
		s := NewServer(fetcherFor(publicResolver(), ca, port), ctl, quietLog(), 0)
		rec := doFetch(t, s, testCredential, urlBody(t, "https://docs.example.com"+path+"?q=\u202e"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", path, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type %q, want %q", path, got, want)
		}
		if got := ctl.completes[0].ContentType; got != want {
			t.Errorf("%s: logged content type %q, want %q", path, got, want)
		}
		final := rec.Header().Get("X-Uzi-Final-Url")
		if final != "https://docs.example.com"+path+"?q=%E2%80%AE" || final != ctl.completes[0].FinalURL {
			t.Errorf("%s: final URL header %q, logged %q", path, final, ctl.completes[0].FinalURL)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
}
