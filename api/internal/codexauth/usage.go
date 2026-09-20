package codexauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// The Codex per-account usage reader (PRD #1209 M2). It is the THIRD provider surface on
// this client, a sibling of DiscoverIdentity: a NONROTATING GET of the /wham/usage
// endpoint that returns the account's rate-limit buckets, presenting the persisted
// WorkspaceAccountID as the ChatGPT-Account-Id header (NOT a decoded claim). The raw
// access token is a bearer parameter and is never returned; only sanitized labels, flags
// and numeric windows leave this method.

// maxAdditionalRateLimits caps how many additional_rate_limits entries a single reading
// may carry. A hostile or broken upstream cannot make the poller store an unbounded bucket
// set; entries past the cap are dropped (the first N are kept). 16 comfortably exceeds the
// handful of metered features Codex reports today.
const maxAdditionalRateLimits = 16

// maxBucketLabelBytes bounds a sanitized bucket id / display name. Codex labels are short
// slugs; this guards a display column against an over-long provider string after the
// control/bidi strip.
const maxBucketLabelBytes = 96

// codexMainBucketID is the stable key for the account's top-level rate_limit bucket. The
// additional_rate_limits entries are keyed by their own limit_name/metered_feature; a
// collision with this reserved id is rejected as a duplicate (see normalizeUsage).
const codexMainBucketID = "codex"

// UsageWindow is one utilization window of a Codex rate-limit bucket, decoded from the
// provider's snake_case wire shape. Every field is a pointer so a MISSING/null sub-field
// stays unknown (nil) rather than collapsing to a misleading zero — a valid numeric zero
// (used_percent 0) is a distinct, real reading.
type UsageWindow struct {
	UsedPercent        *float64
	LimitWindowSeconds *int64
	ResetAfterSeconds  *int64
	ResetAt            *int64
}

// UsageBucket is one named Codex rate-limit bucket: a stable id/display label, the two
// boolean signals (nil when the provider omitted them), and up to two windows (nil when
// absent). It is codexauth's OWN normalized shape; workersvc maps it onto the frozen
// apitypes DTO. ID is already sanitized (control/ANSI/bidi/newline stripped) and bounded.
type UsageBucket struct {
	ID           string
	DisplayName  string
	Allowed      *bool
	LimitReached *bool
	Primary      *UsageWindow
	Secondary    *UsageWindow
}

// UsageReading is the normalized result of ReadUsage: the provider's decoded identity
// anchors (for the caller's identity-mismatch check) plus the normalized bucket set. The
// identity fields are handed to the caller ONLY for that in-package check and are never
// persisted, logged or returned onward.
type UsageReading struct {
	UserID    string
	AccountID string
	Buckets   []UsageBucket
}

// wire types: the HTTP snake_case payload. Unknown additive fields are ignored (a plain
// decode); an invalid KNOWN field (a type mismatch) makes json.Decode fail, which
// ReadUsage surfaces as an error so the caller preserves its last-good reading.
type usageWindowWire struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

type usageRateLimitWire struct {
	Allowed         *bool            `json:"allowed"`
	LimitReached    *bool            `json:"limit_reached"`
	PrimaryWindow   *usageWindowWire `json:"primary_window"`
	SecondaryWindow *usageWindowWire `json:"secondary_window"`
}

type additionalRateLimitWire struct {
	MeteredFeature string             `json:"metered_feature"`
	LimitName      string             `json:"limit_name"`
	RateLimit      usageRateLimitWire `json:"rate_limit"`
}

type usageResponseWire struct {
	UserID               string                    `json:"user_id"`
	AccountID            string                    `json:"account_id"`
	RateLimit            *usageRateLimitWire       `json:"rate_limit"`
	AdditionalRateLimits []additionalRateLimitWire `json:"additional_rate_limits"`
}

// ErrUsageDuplicateBucket is returned by ReadUsage when two additional_rate_limits entries
// resolve to the same bucket id (or one collides with the reserved "codex" id). An
// ambiguous bucket set cannot be stored coherently, so the whole reading is rejected and
// the caller keeps its last-good — the same fail-closed posture as an invalid known field.
var ErrUsageDuplicateBucket = fmt.Errorf("codexauth: usage response carries duplicate rate-limit bucket ids")

// ReadUsage reads one Codex account's rate-limit buckets with a single NONROTATING GET
// against the usage surface (PRD #1209 M2). accessToken is presented as a bearer
// credential; workspaceAccountID is the PERSISTED workspace id, sent as the
// ChatGPT-Account-Id header (never a claim decoded here).
//
//   - 2xx: the body is decoded and normalized. A duplicate bucket id → ErrUsageDuplicateBucket;
//     any other decode failure (invalid known field) → a wrapped error. Otherwise a
//     UsageReading with the decoded identity anchors and the sanitized, item-capped buckets.
//   - non-2xx (including 401/403/429) → *AuthError carrying the status; a 429 also carries
//     the parsed Retry-After.
func (c *Client) ReadUsage(ctx context.Context, accessToken, workspaceAccountID string) (UsageReading, error) {
	ctx, cancel := c.requestContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.usageBase+"/wham/usage", nil)
	if err != nil {
		return UsageReading{}, fmt.Errorf("codexauth: build usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	// The PERSISTED workspace id, not a claim decoded here. Go forwards this custom header
	// across a cross-host redirect, which is exactly why NewClient refuses redirects.
	req.Header.Set("ChatGPT-Account-Id", workspaceAccountID)

	resp, err := c.doer.Do(req)
	if err != nil {
		return UsageReading{}, fmt.Errorf("codexauth: usage request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close of a drained response body

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		aerr := &AuthError{Op: "read_usage", StatusCode: resp.StatusCode}
		if resp.StatusCode == http.StatusTooManyRequests {
			aerr.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		}
		return UsageReading{}, aerr
	}

	// A plain decode ignores unknown additive fields but FAILS on an invalid known field
	// (a type mismatch), which ReadUsage surfaces so the caller keeps its last-good reading.
	var body usageResponseWire
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return UsageReading{}, fmt.Errorf("codexauth: decode usage response: %w", err)
	}
	return normalizeUsage(body)
}

// normalizeUsage builds the UsageReading from the decoded wire body: the top-level
// rate_limit becomes the reserved "codex" bucket; each additional_rate_limits entry (capped
// at maxAdditionalRateLimits, taking the first N) becomes a bucket keyed by its sanitized
// limit_name (falling back to metered_feature). A duplicate id across the whole set is
// rejected. An additional entry with no usable label after sanitization is dropped (it
// carries no identity to key or display).
func normalizeUsage(body usageResponseWire) (UsageReading, error) {
	reading := UsageReading{UserID: body.UserID, AccountID: body.AccountID}
	seen := map[string]struct{}{}

	if body.RateLimit != nil {
		reading.Buckets = append(reading.Buckets, bucketFromWire(codexMainBucketID, "", *body.RateLimit))
		seen[codexMainBucketID] = struct{}{}
	}

	additional := body.AdditionalRateLimits
	if len(additional) > maxAdditionalRateLimits {
		additional = additional[:maxAdditionalRateLimits]
	}
	for _, add := range additional {
		id := sanitizeBucketLabel(add.LimitName)
		if id == "" {
			id = sanitizeBucketLabel(add.MeteredFeature)
		}
		if id == "" {
			// No usable key or label: nothing to store coherently, so drop this entry
			// rather than fail the whole reading.
			continue
		}
		if _, dup := seen[id]; dup {
			return UsageReading{}, ErrUsageDuplicateBucket
		}
		seen[id] = struct{}{}
		reading.Buckets = append(reading.Buckets, bucketFromWire(id, id, add.RateLimit))
	}
	return reading, nil
}

// bucketFromWire maps one wire rate_limit into a normalized bucket under the given id and
// display name. Both windows carry through as pointers (nil when the provider omitted them).
func bucketFromWire(id, displayName string, rl usageRateLimitWire) UsageBucket {
	return UsageBucket{
		ID:           id,
		DisplayName:  displayName,
		Allowed:      rl.Allowed,
		LimitReached: rl.LimitReached,
		Primary:      windowFromWire(rl.PrimaryWindow),
		Secondary:    windowFromWire(rl.SecondaryWindow),
	}
}

func windowFromWire(w *usageWindowWire) *UsageWindow {
	if w == nil {
		return nil
	}
	return &UsageWindow{
		UsedPercent:        w.UsedPercent,
		LimitWindowSeconds: w.LimitWindowSeconds,
		ResetAfterSeconds:  w.ResetAfterSeconds,
		ResetAt:            w.ResetAt,
	}
}

// sanitizeBucketLabel strips every terminal-unsafe rune (control, ANSI-escape, bidi/Cf,
// newline — termsafe.Unsafe) and the UTF-8 replacement rune from a provider-supplied
// label, bounds it after each whole rune, and trims edge whitespace. Because a bucket id
// and display name land in a stored/displayed DTO another user (admin view) reads, this is
// the same Trojan-Source defence the house predicate provides everywhere else — with NO
// \n/\t exception, since a bucket id is a single-line slug, not flowing text.
func sanitizeBucketLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == utf8.RuneError || termsafe.Unsafe(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxBucketLabelBytes {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// parseRetryAfter parses a Retry-After header value: either delta-seconds (a non-negative
// integer) or an HTTP-date, per RFC 9110. It returns a non-negative duration, or 0 when the
// header is empty/unparseable or already in the past. now is injected so the date form is
// testable.
func parseRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
