// Package anthropic is a small outbound client for reading a credential's
// account-wide rate-limit state (PRD #53). It has exactly two calls:
//
//   - Usage: the free GET /api/oauth/usage endpoint, which returns 5h/7d
//     utilization + reset times for the credentials it accepts (persistently 429s
//     `claude setup-token` credentials — the common uzi case).
//   - ProbeHeaders: a minimal, pinned Messages request (cheapest Haiku, max_tokens
//     1) whose response carries the same numbers as anthropic-ratelimit-unified-*
//     headers. Costs ~1 token of the credential's own quota, so it is the fallback
//     when the free endpoint refuses the credential (D2).
//
// The one hard invariant (pinned by a test): no error this package returns can
// ever contain the token. The token rides only on the Authorization header of the
// outbound request; errors are built from the response (status + a sanitized body
// excerpt) or a transport failure, never from the request. Utilization fractions
// floor+clamp to 0..100 ints; a missing utilization fails the whole reading (fail
// closed, D5); a missing/unparseable reset is left nil (the DTO allows a null
// resets_at, D7). PRD #50's egress proxy, if it lands, wraps this same client.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	usageURL    = "https://api.anthropic.com/api/oauth/usage"
	messagesURL = "https://api.anthropic.com/v1/messages"

	apiVersion = "2023-06-01"
	oauthBeta  = "oauth-2025-04-20"
	userAgent  = "uzi/0.1.0"

	// maxBodyBytes bounds how much of a response body we read — enough for the tiny
	// usage JSON and for a sanitized error excerpt, without trusting the peer's
	// Content-Length.
	maxBodyBytes = 8192
	// errExcerptBytes caps the sanitized body excerpt carried in an HTTP error.
	errExcerptBytes = 256
)

// probeBody is PINNED: the cheapest current Haiku, max_tokens 1, and a fixed
// innocuous prompt. It is NEVER user or run content — the probe exists only to
// elicit the unified rate-limit headers, which are account-wide, so the model
// choice does not affect the numbers. `claude-haiku-4-5` is the alias for the
// cheapest Haiku tier.
var probeBody = []byte(`{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)

// Reading is one poll's result: both account windows plus which path produced it.
type Reading struct {
	FiveHour Window
	SevenDay Window
	Source   string // SourceUsageEndpoint | SourceHeaderProbe
}

// Window is one rate-limit window. Pct is the floor+clamped utilization percent
// (0..100). ResetsAt is what Anthropic reported, or nil when it gave none.
type Window struct {
	Pct      int
	ResetsAt *time.Time
}

// Source values, mirroring the migration's CHECK constraint.
const (
	SourceUsageEndpoint = "usage_endpoint"
	SourceHeaderProbe   = "header_probe"
	// SourceLimitReport marks a gauge row written at usage-limit park time from a
	// worker's limit report rather than measured by the poller (PRD #217 M1, D6).
	SourceLimitReport = "limit_report"
)

// AllSources is the whole `source` vocabulary in a form a guard can enumerate, the
// source-of-truth a drift test compares against migration 00108's CHECK (M4), the
// same shape autoselect.AllReasons() uses for its own CHECK.
//
// Returns a fresh slice: a package-level var would let one caller's append corrupt
// every other reader's view of a CLOSED set.
func AllSources() []string {
	return []string{SourceUsageEndpoint, SourceHeaderProbe, SourceLimitReport}
}

// ErrKind classifies a client failure so the poller can apply the D5 failure
// semantics (fail closed vs back off vs wait for the next tick).
type ErrKind int

const (
	// KindTransport: the HTTP round-trip never completed (DNS, dial, TLS, timeout).
	// Transient — the poller just waits for the next tick, no backoff.
	KindTransport ErrKind = iota
	// KindHTTP: the server returned a non-2xx status — a definitive refusal of this
	// credential/endpoint. Drives the usage→probe fallback and, with no usable
	// fallback, the per-user backoff.
	KindHTTP
	// KindMalformed: a 2xx response whose body/headers could not be parsed into a
	// complete reading. Fail closed — never overwrite the last good row, no backoff.
	KindMalformed
)

// Error is the only error type this package returns. Its message is sanitized and
// can never contain the request (and therefore never the token).
type Error struct {
	Kind   ErrKind
	Status int // set only for KindHTTP
	msg    string
}

func (e *Error) Error() string { return e.msg }

// Client reads rate-limit state from Anthropic. Safe for concurrent use (its
// http.Client is). Construct with New.
type Client struct {
	hc *http.Client
}

// New builds a Client over the given http.Client, whose Timeout bounds every call
// (UZI_ANTHROPIC_HTTP_TIMEOUT). A nil client uses http.DefaultClient (no timeout);
// production always passes a bounded one.
func New(hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{hc: hc}
}

// Usage reads the free /api/oauth/usage endpoint. It returns a KindHTTP Error when
// the endpoint refuses the credential (the caller then falls back to ProbeHeaders).
func (c *Client) Usage(ctx context.Context, token []byte) (Reading, error) {
	resp, err := c.do(ctx, http.MethodGet, usageURL, nil, token)
	if err != nil {
		return Reading{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return Reading{}, httpError("usage", resp.StatusCode, body)
	}
	return parseUsage(body)
}

// ProbeHeaders sends the pinned minimal Messages request and reads the unified
// rate-limit headers off the response. It spends ~1 token of the credential's own
// quota, so the poller only calls it when Usage returned a KindHTTP refusal (D2).
func (c *Client) ProbeHeaders(ctx context.Context, token []byte) (Reading, error) {
	resp, err := c.do(ctx, http.MethodPost, messagesURL, probeBody, token)
	if err != nil {
		return Reading{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return Reading{}, httpError("probe", resp.StatusCode, body)
	}
	return parseHeaders(resp.Header)
}

// do issues the request with the shared auth headers. The token is set ONLY on the
// Authorization header; no error path below reads the request, so no error can
// carry the token.
func (c *Client) do(ctx context.Context, method, url string, body []byte, token []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		// Constructed from method/url only (both compile-time constants here), never
		// from the token.
		return nil, &Error{Kind: KindTransport, msg: "anthropic: build request failed"}
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("anthropic-version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// A transport error (*url.Error) carries the method + URL, never a request
		// header, so it cannot include the token. Kept generic regardless.
		return nil, &Error{Kind: KindTransport, msg: "anthropic: request failed (transport error)"}
	}
	return resp, nil
}

// parseUsage parses the /api/oauth/usage body. Shape (verified against
// vtmocanu/cc-statusline, the prior art): {"five_hour":{"utilization":<0..1>,
// "resets_at":<ISO8601>}, "seven_day":{...}}. A missing utilization fails the
// whole reading (fail closed); a missing/unparseable resets_at is left nil.
func parseUsage(body []byte) (Reading, error) {
	var u struct {
		FiveHour usageWindow `json:"five_hour"`
		SevenDay usageWindow `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return Reading{}, &Error{Kind: KindMalformed, msg: "anthropic usage: response is not valid JSON"}
	}
	fh, err := u.FiveHour.window("five_hour")
	if err != nil {
		return Reading{}, err
	}
	sd, err := u.SevenDay.window("seven_day")
	if err != nil {
		return Reading{}, err
	}
	return Reading{FiveHour: fh, SevenDay: sd, Source: SourceUsageEndpoint}, nil
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func (w usageWindow) window(label string) (Window, error) {
	if w.Utilization == nil {
		return Window{}, &Error{Kind: KindMalformed, msg: "anthropic usage: missing " + label + " utilization"}
	}
	pct, ok := fractionToPct(*w.Utilization)
	if !ok {
		return Window{}, &Error{Kind: KindMalformed, msg: "anthropic usage: " + label + " utilization is not a finite number"}
	}
	out := Window{Pct: pct}
	if w.ResetsAt != nil {
		if t, ok := parseISO(*w.ResetsAt); ok {
			out.ResetsAt = &t
		}
	}
	return out, nil
}

// parseHeaders reads the unified rate-limit headers off a probe response.
// Utilization is a 0..1 fraction (calibrated: 0.55 == 55%); resets are already
// epoch seconds. A missing utilization header fails the reading (an account with
// no unified headers — e.g. a Console API key — is out of scope and surfaces as no
// reading, i.e. `unavailable`).
func parseHeaders(h http.Header) (Reading, error) {
	fp, ok := headerPct(h, "anthropic-ratelimit-unified-5h-utilization")
	if !ok {
		return Reading{}, &Error{Kind: KindMalformed, msg: "anthropic probe: missing 5h utilization header"}
	}
	sp, ok := headerPct(h, "anthropic-ratelimit-unified-7d-utilization")
	if !ok {
		return Reading{}, &Error{Kind: KindMalformed, msg: "anthropic probe: missing 7d utilization header"}
	}
	return Reading{
		FiveHour: Window{Pct: fp, ResetsAt: headerEpoch(h, "anthropic-ratelimit-unified-5h-reset")},
		SevenDay: Window{Pct: sp, ResetsAt: headerEpoch(h, "anthropic-ratelimit-unified-7d-reset")},
		Source:   SourceHeaderProbe,
	}, nil
}

// fractionToPct floors a 0..1 utilization fraction to an integer percent and
// clamps it to [0,100]. Reports ok=false for NaN/Inf so the caller can fail closed.
func fractionToPct(f float64) (int, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	p := int(math.Floor(f * 100))
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p, true
}

// headerPct reads a utilization header as a 0..1 fraction and converts it to a
// clamped percent. ok=false when the header is absent or not a finite number.
func headerPct(h http.Header, key string) (int, bool) {
	raw := strings.TrimSpace(h.Get(key))
	if raw == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return fractionToPct(f)
}

// headerEpoch reads a reset header as epoch seconds. Returns nil when absent or
// unparseable (a reading without a reset is still valid, D7).
func headerEpoch(h http.Header, key string) *time.Time {
	raw := strings.TrimSpace(h.Get(key))
	if raw == "" {
		return nil
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	t := time.Unix(secs, 0).UTC()
	return &t
}

// parseISO parses an ISO-8601 timestamp to UTC. RFC3339Nano covers the
// fractional-second and offset forms Anthropic emits; ok=false leaves the reset nil.
func parseISO(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// httpError builds a KindHTTP Error from the status and a sanitized excerpt of the
// response body. The body is Anthropic's (an error JSON), never the request, so it
// cannot carry the token.
func httpError(label string, status int, body []byte) *Error {
	excerpt := sanitizeExcerpt(body)
	msg := fmt.Sprintf("anthropic %s: HTTP %d", label, status)
	if excerpt != "" {
		msg += ": " + excerpt
	}
	return &Error{Kind: KindHTTP, Status: status, msg: msg}
}

// sanitizeExcerpt trims a response body to a short, control-char-free excerpt safe
// to embed in an error/log line.
func sanitizeExcerpt(body []byte) string {
	if len(body) > errExcerptBytes {
		body = body[:errExcerptBytes]
	}
	var b strings.Builder
	b.Grow(len(body))
	for _, r := range string(body) {
		switch {
		case r == '\n' || r == '\t' || r == ' ':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// drop other control chars
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
