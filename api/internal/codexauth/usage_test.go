package codexauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// usageResponder serves a canned /wham/usage reply and records the request headers so a
// test can assert the ChatGPT-Account-Id header without a second seam. The REAL Client
// decode path runs above it.
type usageResponder struct {
	status  int
	body    string
	header  http.Header
	gotHdrs http.Header
}

func newUsageClient(r *usageResponder) *Client {
	tr := newCountingTransport(func(_ string, req *http.Request) (*http.Response, error) {
		r.gotHdrs = req.Header.Clone()
		resp := jsonResponse(r.status, r.body)
		for k, vs := range r.header {
			for _, v := range vs {
				resp.Header.Add(k, v)
			}
		}
		return resp, nil
	})
	return newTestClient(tr)
}

func TestReadUsageDecodeTable(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantErr   bool
		wantErrIs error // when set, the error must satisfy errors.Is(err, wantErrIs)
		check     func(t *testing.T, r UsageReading)
	}{
		{
			name: "full payload",
			body: `{
				"user_id": "u-1",
				"account_id": "acct-1",
				"rate_limit": {
					"allowed": true,
					"limit_reached": false,
					"primary_window": {"used_percent": 42.5, "limit_window_seconds": 18000, "reset_after_seconds": 3600, "reset_at": 1893456000},
					"secondary_window": {"used_percent": 7, "limit_window_seconds": 604800, "reset_after_seconds": 200000, "reset_at": 1894000000}
				},
				"additional_rate_limits": [
					{"metered_feature": "gpt5_codex", "limit_name": "codex_high", "rate_limit": {"allowed": false, "limit_reached": true, "primary_window": {"used_percent": 100, "limit_window_seconds": 300, "reset_after_seconds": 120, "reset_at": 1893450000}}}
				]
			}`,
			check: func(t *testing.T, r UsageReading) {
				if r.UserID != "u-1" || r.AccountID != "acct-1" {
					t.Fatalf("identity: got %q/%q", r.UserID, r.AccountID)
				}
				if len(r.Buckets) != 2 {
					t.Fatalf("want 2 buckets, got %d", len(r.Buckets))
				}
				main := r.Buckets[0]
				if main.ID != "codex" {
					t.Fatalf("main bucket id = %q", main.ID)
				}
				if main.Allowed == nil || !*main.Allowed {
					t.Fatalf("main allowed = %v", main.Allowed)
				}
				if main.Primary == nil || main.Primary.UsedPercent == nil || *main.Primary.UsedPercent != 42.5 {
					t.Fatalf("main primary used_percent wrong: %+v", main.Primary)
				}
				if main.Secondary == nil || main.Secondary.LimitWindowSeconds == nil || *main.Secondary.LimitWindowSeconds != 604800 {
					t.Fatalf("main secondary window wrong: %+v", main.Secondary)
				}
				add := r.Buckets[1]
				if add.ID != "codex_high" || add.DisplayName != "codex_high" {
					t.Fatalf("additional bucket id/name = %q/%q", add.ID, add.DisplayName)
				}
				if add.LimitReached == nil || !*add.LimitReached {
					t.Fatalf("additional limit_reached = %v", add.LimitReached)
				}
			},
		},
		{
			name: "valid numeric zero is a reading, missing sub-fields stay nil (partial windows)",
			body: `{"user_id":"u","account_id":"a","rate_limit":{"allowed":true,"primary_window":{"used_percent":0}}}`,
			check: func(t *testing.T, r UsageReading) {
				if len(r.Buckets) != 1 {
					t.Fatalf("want 1 bucket, got %d", len(r.Buckets))
				}
				w := r.Buckets[0].Primary
				if w == nil || w.UsedPercent == nil || *w.UsedPercent != 0 {
					t.Fatalf("used_percent zero not preserved: %+v", w)
				}
				if w.LimitWindowSeconds != nil || w.ResetAfterSeconds != nil || w.ResetAt != nil {
					t.Fatalf("absent window sub-fields should be nil: %+v", w)
				}
				if r.Buckets[0].LimitReached != nil {
					t.Fatalf("absent limit_reached should be nil")
				}
			},
		},
		{
			name: "null windows",
			body: `{"user_id":"u","account_id":"a","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":null,"secondary_window":null}}`,
			check: func(t *testing.T, r UsageReading) {
				if r.Buckets[0].Primary != nil || r.Buckets[0].Secondary != nil {
					t.Fatalf("null windows should decode to nil: %+v", r.Buckets[0])
				}
			},
		},
		{
			name: "two windows same duration both preserved",
			body: `{"user_id":"u","account_id":"a","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":3600},"secondary_window":{"used_percent":90,"limit_window_seconds":3600}}}`,
			check: func(t *testing.T, r UsageReading) {
				p, s := r.Buckets[0].Primary, r.Buckets[0].Secondary
				if p == nil || s == nil {
					t.Fatalf("both windows expected: %+v", r.Buckets[0])
				}
				if *p.LimitWindowSeconds != 3600 || *s.LimitWindowSeconds != 3600 {
					t.Fatalf("both durations should be 3600: %d/%d", *p.LimitWindowSeconds, *s.LimitWindowSeconds)
				}
				if *p.UsedPercent == *s.UsedPercent {
					t.Fatalf("windows should stay distinct despite equal duration")
				}
			},
		},
		{
			name: "multiple additional buckets",
			body: `{"user_id":"u","account_id":"a","rate_limit":{"allowed":true},"additional_rate_limits":[
				{"limit_name":"one","rate_limit":{"allowed":true}},
				{"limit_name":"two","rate_limit":{"allowed":false}},
				{"metered_feature":"three_feature","rate_limit":{"limit_reached":true}}
			]}`,
			check: func(t *testing.T, r UsageReading) {
				if len(r.Buckets) != 4 {
					t.Fatalf("want main + 3 additional = 4 buckets, got %d", len(r.Buckets))
				}
				ids := []string{r.Buckets[1].ID, r.Buckets[2].ID, r.Buckets[3].ID}
				want := []string{"one", "two", "three_feature"}
				for i := range want {
					if ids[i] != want[i] {
						t.Fatalf("bucket %d id = %q, want %q", i+1, ids[i], want[i])
					}
				}
			},
		},
		{
			name:      "duplicate additional id rejected",
			body:      `{"user_id":"u","account_id":"a","additional_rate_limits":[{"limit_name":"dup","rate_limit":{}},{"limit_name":"dup","rate_limit":{}}]}`,
			wantErr:   true,
			wantErrIs: ErrUsageDuplicateBucket,
		},
		{
			name:      "additional colliding with reserved codex id rejected",
			body:      `{"user_id":"u","account_id":"a","rate_limit":{"allowed":true},"additional_rate_limits":[{"limit_name":"codex","rate_limit":{}}]}`,
			wantErr:   true,
			wantErrIs: ErrUsageDuplicateBucket,
		},
		{
			name: "control/bidi-laden label sanitized",
			// limit_name (in the JSON body below) wraps the real slug in a C0 ESC introducer,
			// a bidi override (U+202E), a newline and a zero-width space (U+200B).
			// sanitizeBucketLabel strips the ESC byte and the other unsafe runes but KEEPS the
			// printable bracket-2-J tail that trailed the ESC, so the slug reduces to "da[2Jnger".
			body: "{\"user_id\":\"u\",\"account_id\":\"a\",\"additional_rate_limits\":[{\"limit_name\":\"da\\u001b[2Jn\\u202eg\\ne\\u200br\",\"rate_limit\":{}}]}",
			check: func(t *testing.T, r UsageReading) {
				if len(r.Buckets) != 1 {
					t.Fatalf("want 1 bucket, got %d", len(r.Buckets))
				}
				got := r.Buckets[0].ID
				if got != "da[2Jnger" {
					t.Fatalf("sanitized label = %q, want %q", got, "da[2Jnger")
				}
				for _, ru := range []rune{0x1b, 0x202e, '\n', 0x200b} {
					if strings.ContainsRune(got, ru) {
						t.Fatalf("sanitized label still carries unsafe rune %U: %q", ru, got)
					}
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newUsageClient(&usageResponder{status: 200, body: tc.body})
			r, err := c.ReadUsage(context.Background(), assembleToken("sk"), "acct-1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got reading %+v", r)
				}
				if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error = %v, want errors.Is(err, %v)", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.check(t, r)
		})
	}
}

func TestReadUsageCapsAdditional(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"user_id":"u","account_id":"a","additional_rate_limits":[`)
	for i := 0; i < maxAdditionalRateLimits+8; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		// distinct ids so, absent the cap, all would survive dedup.
		sb.WriteString(`{"limit_name":"b` + strings.Repeat("x", i%3) + itoa(i) + `","rate_limit":{}}`)
	}
	sb.WriteString(`]}`)
	c := newUsageClient(&usageResponder{status: 200, body: sb.String()})
	r, err := c.ReadUsage(context.Background(), assembleToken("sk"), "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(r.Buckets) != maxAdditionalRateLimits {
		t.Fatalf("want %d capped buckets, got %d", maxAdditionalRateLimits, len(r.Buckets))
	}
}

// itoa avoids strconv noise in the fixture builder above.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

func TestReadUsageSetsAccountHeader(t *testing.T) {
	r := &usageResponder{status: 200, body: `{"user_id":"u","account_id":"a"}`}
	c := newUsageClient(r)
	if _, err := c.ReadUsage(context.Background(), assembleToken("sk"), "workspace-99"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := r.gotHdrs.Get("ChatGPT-Account-Id"); got != "workspace-99" {
		t.Fatalf("ChatGPT-Account-Id = %q, want workspace-99", got)
	}
	if got := r.gotHdrs.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if !strings.HasPrefix(r.gotHdrs.Get("Authorization"), "Bearer ") {
		t.Fatalf("Authorization = %q", r.gotHdrs.Get("Authorization"))
	}
}

func TestReadUsageNon2xx(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusFound} {
		c := newUsageClient(&usageResponder{status: status, body: `{}`})
		_, err := c.ReadUsage(context.Background(), assembleToken("sk"), "a")
		var aerr *AuthError
		if !errors.As(err, &aerr) {
			t.Fatalf("status %d: want *AuthError, got %v", status, err)
		}
		if aerr.StatusCode != status {
			t.Fatalf("AuthError status = %d, want %d", aerr.StatusCode, status)
		}
		if aerr.Op != "read_usage" {
			t.Fatalf("AuthError op = %q", aerr.Op)
		}
	}
}

func TestReadUsage429CarriesRetryAfter(t *testing.T) {
	c := newUsageClient(&usageResponder{
		status: http.StatusTooManyRequests,
		body:   `{}`,
		header: http.Header{"Retry-After": []string{"90"}},
	})
	_, err := c.ReadUsage(context.Background(), assembleToken("sk"), "a")
	var aerr *AuthError
	if !errors.As(err, &aerr) {
		t.Fatalf("want *AuthError, got %v", err)
	}
	if aerr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", aerr.StatusCode)
	}
	if aerr.RetryAfter != 90*time.Second {
		t.Fatalf("RetryAfter = %v, want 90s", aerr.RetryAfter)
	}
}

// TestNewClientRefusesRedirects proves NewClient wires the exact redirect-refusal policy
// (ErrUseLastResponse) on its default doer, so a 3xx surfaces as a non-2xx rather than
// being followed with the custom ChatGPT-Account-Id header attached.
func TestNewClientRefusesRedirects(t *testing.T) {
	c := NewClient()
	hc, ok := c.doer.(*http.Client)
	if !ok {
		t.Fatalf("default doer is %T, want *http.Client", c.doer)
	}
	if hc.CheckRedirect == nil {
		t.Fatal("default doer has no CheckRedirect policy")
	}
	if err := hc.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("CheckRedirect = %v, want ErrUseLastResponse", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"  ", 0},
		{"0", 0},
		{"-5", 0},
		{"120", 120 * time.Second},
		{" 30 ", 30 * time.Second},
		{"garbage", 0},
		{now.Add(2 * time.Minute).UTC().Format(http.TimeFormat), 2 * time.Minute},
		{now.Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, tc := range tests {
		if got := parseRetryAfter(tc.in, now); got != tc.want {
			t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
