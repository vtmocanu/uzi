package anthropic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// capturingTransport drives Usage/ProbeHeaders end to end with no network: it
// records exactly what the client sent (method, URL, headers, body) and returns a
// canned response. This exercises the real public methods — c.hc.Do, the status
// check, the io.LimitReader body read, and the parser — rather than the test-only
// doAndClassify mirror, so the outbound-request contract that carries the token
// finally gets an assertion.
type capturingTransport struct {
	// response to serve
	status int
	body   string
	header http.Header

	// captured from the single outbound request
	gotMethod string
	gotURL    string
	gotHeader http.Header
	gotBody   []byte
	calls     int
}

func (ct *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ct.calls++
	ct.gotMethod = req.Method
	ct.gotURL = req.URL.String()
	ct.gotHeader = req.Header.Clone()
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		ct.gotBody = b
	}
	h := ct.header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{
		StatusCode: ct.status,
		Status:     http.StatusText(ct.status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(ct.body)),
		Request:    req,
	}, nil
}

func stubClient(ct *capturingTransport) *Client {
	return &Client{hc: &http.Client{Transport: ct}}
}

// TestUsageSuccess drives the real Usage method against a 200 usage payload and
// asserts both the Reading a caller consumes and the outbound request it sent.
func TestUsageSuccess(t *testing.T) {
	ct := &capturingTransport{
		status: http.StatusOK,
		body:   `{"five_hour":{"utilization":0.55,"resets_at":"2026-07-15T09:20:11Z"},"seven_day":{"utilization":0.10}}`,
	}
	c := stubClient(ct)

	r, err := c.Usage(context.Background(), []byte(secretToken))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The reading is the parsed usage-endpoint result. Five/seven differ so a
	// window swap is observable.
	if r.Source != SourceUsageEndpoint {
		t.Errorf("Source = %q, want %q", r.Source, SourceUsageEndpoint)
	}
	if r.FiveHour.Pct != 55 || r.SevenDay.Pct != 10 {
		t.Errorf("Pct = (%d,%d), want (55,10)", r.FiveHour.Pct, r.SevenDay.Pct)
	}
	if r.FiveHour.ResetsAt == nil {
		t.Error("FiveHour.ResetsAt = nil, want the parsed reset")
	}
	// Outbound contract: a GET to the usage endpoint, the token only on
	// Authorization, and no request body.
	if ct.gotMethod != http.MethodGet {
		t.Errorf("method = %q, want %q", ct.gotMethod, http.MethodGet)
	}
	if ct.gotURL != usageURL {
		t.Errorf("url = %q, want %q", ct.gotURL, usageURL)
	}
	if got := ct.gotHeader.Get("Authorization"); got != "Bearer "+secretToken {
		t.Errorf("Authorization = %q, want %q", got, "Bearer "+secretToken)
	}
	if len(ct.gotBody) != 0 {
		t.Errorf("GET usage sent a body: %q", ct.gotBody)
	}
}

// TestUsageRefusalKindHTTP asserts a non-2xx usage response is classified as a
// KindHTTP refusal (which drives the poller's usage->probe fallback) and that the
// token never rides in the error.
func TestUsageRefusalKindHTTP(t *testing.T) {
	ct := &capturingTransport{
		status: http.StatusTooManyRequests,
		body:   `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
	}
	c := stubClient(ct)

	_, err := c.Usage(context.Background(), []byte(secretToken))
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *Error, got %v", err)
	}
	if ae.Kind != KindHTTP {
		t.Errorf("Kind = %d, want KindHTTP(%d)", ae.Kind, KindHTTP)
	}
	if ae.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want %d", ae.Status, http.StatusTooManyRequests)
	}
	if !strings.Contains(err.Error(), "anthropic usage: HTTP 429") {
		t.Errorf("message = %q, want it to contain %q", err.Error(), "anthropic usage: HTTP 429")
	}
	assertNoToken(t, err)
}

// TestProbeHeadersSuccess drives the real ProbeHeaders method against a 200 with
// unified rate-limit headers and asserts the Reading plus the full outbound
// contract: the PINNED probe body, POSTed to the messages endpoint, with the token
// and the required Anthropic headers.
func TestProbeHeadersSuccess(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.55")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.03")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1784000000")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1784500000")
	ct := &capturingTransport{status: http.StatusOK, header: h}
	c := stubClient(ct)

	r, err := c.ProbeHeaders(context.Background(), []byte(secretToken))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Source != SourceHeaderProbe {
		t.Errorf("Source = %q, want %q", r.Source, SourceHeaderProbe)
	}
	if r.FiveHour.Pct != 55 || r.SevenDay.Pct != 3 {
		t.Errorf("Pct = (%d,%d), want (55,3)", r.FiveHour.Pct, r.SevenDay.Pct)
	}
	if ct.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", ct.gotMethod, http.MethodPost)
	}
	if ct.gotURL != messagesURL {
		t.Errorf("url = %q, want %q", ct.gotURL, messagesURL)
	}
	if !bytes.Equal(ct.gotBody, probeBody) {
		t.Errorf("body = %q, want probeBody %q", ct.gotBody, probeBody)
	}
	if got := ct.gotHeader.Get("Authorization"); got != "Bearer "+secretToken {
		t.Errorf("Authorization = %q, want %q", got, "Bearer "+secretToken)
	}
	if got := ct.gotHeader.Get("anthropic-beta"); got != oauthBeta {
		t.Errorf("anthropic-beta = %q, want %q", got, oauthBeta)
	}
	if got := ct.gotHeader.Get("anthropic-version"); got != apiVersion {
		t.Errorf("anthropic-version = %q, want %q", got, apiVersion)
	}
	if got := ct.gotHeader.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
}

// TestProbeHeadersRefusalKindHTTP mirrors the usage refusal for the probe path,
// pinning the "probe" label so a label swap between the two calls is observable.
func TestProbeHeadersRefusalKindHTTP(t *testing.T) {
	ct := &capturingTransport{
		status: http.StatusTooManyRequests,
		body:   `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
	}
	c := stubClient(ct)

	_, err := c.ProbeHeaders(context.Background(), []byte(secretToken))
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *Error, got %v", err)
	}
	if ae.Kind != KindHTTP {
		t.Errorf("Kind = %d, want KindHTTP(%d)", ae.Kind, KindHTTP)
	}
	if ae.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want %d", ae.Status, http.StatusTooManyRequests)
	}
	if !strings.Contains(err.Error(), "anthropic probe: HTTP 429") {
		t.Errorf("message = %q, want it to contain %q", err.Error(), "anthropic probe: HTTP 429")
	}
	assertNoToken(t, err)
}

// TestAllSources pins the closed `source` vocabulary (a drift test compares it
// against the migration CHECK) and the documented fresh-slice invariant: one
// caller's mutation must not corrupt another reader's view.
func TestAllSources(t *testing.T) {
	want := []string{SourceUsageEndpoint, SourceHeaderProbe, SourceLimitReport}
	got := AllSources()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllSources() = %v, want %v", got, want)
	}
	got[0] = "mutated-by-caller"
	again := AllSources()
	if again[0] != SourceUsageEndpoint {
		t.Fatalf("AllSources()[0] = %q after a prior caller mutated its result; want %q (slice is aliased)", again[0], SourceUsageEndpoint)
	}
}

// TestUsageNaNUtilizationFailsClosed pins the D5 fail-closed rule: a non-finite
// utilization must fail the whole reading rather than surface a bogus percent.
func TestUsageNaNUtilizationFailsClosed(t *testing.T) {
	nan := math.NaN()
	w := usageWindow{Utilization: &nan}

	_, err := w.window("five_hour")
	var ae *Error
	if !errors.As(err, &ae) || ae.Kind != KindMalformed {
		t.Fatalf("NaN utilization: want KindMalformed error, got %v", err)
	}
	if !strings.Contains(err.Error(), "five_hour utilization is not a finite number") {
		t.Errorf("message = %q, want it to name the non-finite five_hour utilization", err.Error())
	}
}

// TestSanitizeExcerpt pins the log/error-safety transform: \n and \t collapse to a
// single space, other control chars are dropped, the result is trimmed, and a long
// body is capped to errExcerptBytes.
func TestSanitizeExcerpt(t *testing.T) {
	if got := sanitizeExcerpt([]byte("a\tb")); got != "a b" {
		t.Errorf("tab: got %q, want %q", got, "a b")
	}
	if got := sanitizeExcerpt([]byte("a\nb")); got != "a b" {
		t.Errorf("newline: got %q, want %q", got, "a b")
	}
	if got := sanitizeExcerpt([]byte("a\x00b\x7f")); got != "ab" {
		t.Errorf("control chars: got %q, want %q", got, "ab")
	}
	if got := sanitizeExcerpt([]byte("  hi  ")); got != "hi" {
		t.Errorf("trim: got %q, want %q", got, "hi")
	}
	long := strings.Repeat("x", errExcerptBytes+50)
	if got, want := sanitizeExcerpt([]byte(long)), strings.Repeat("x", errExcerptBytes); got != want {
		t.Errorf("capped excerpt len = %d, want %d", len(got), errExcerptBytes)
	}
}

// TestDoBuildRequestError covers the do() build-failure branch: an invalid method
// fails request construction before any network use and must surface as a
// transport-classed error with no token in it.
func TestDoBuildRequestError(t *testing.T) {
	c := New(&http.Client{})

	_, err := c.do(context.Background(), "bad method", usageURL, nil, []byte(secretToken))
	var ae *Error
	if !errors.As(err, &ae) || ae.Kind != KindTransport {
		t.Fatalf("want KindTransport error, got %v", err)
	}
	assertNoToken(t, err)
}

// TestNewNilClientUsesDefault covers New's nil-client branch.
func TestNewNilClientUsesDefault(t *testing.T) {
	c := New(nil)
	if c.hc != http.DefaultClient {
		t.Fatalf("New(nil).hc = %v, want http.DefaultClient", c.hc)
	}
}
