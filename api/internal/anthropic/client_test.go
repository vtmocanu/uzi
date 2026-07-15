package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseUsage(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantErr  ErrKind // -1 = expect success
		wantFive int
		wantSev  int
		fiveHas  bool // expect a non-nil 5h reset
	}{
		{
			name:     "valid with resets",
			body:     `{"five_hour":{"utilization":0.55,"resets_at":"2026-07-15T09:20:11Z"},"seven_day":{"utilization":0.10,"resets_at":"2026-07-22T00:00:00Z"}}`,
			wantErr:  -1,
			wantFive: 55, wantSev: 10, fiveHas: true,
		},
		{
			name:     "floor not round",
			body:     `{"five_hour":{"utilization":0.559},"seven_day":{"utilization":0.999}}`,
			wantErr:  -1,
			wantFive: 55, wantSev: 99, fiveHas: false,
		},
		{
			name:     "clamp above 1 and below 0",
			body:     `{"five_hour":{"utilization":1.5},"seven_day":{"utilization":-0.2}}`,
			wantErr:  -1,
			wantFive: 100, wantSev: 0, fiveHas: false,
		},
		{
			name:     "zero utilization is a valid reading",
			body:     `{"five_hour":{"utilization":0},"seven_day":{"utilization":0}}`,
			wantErr:  -1,
			wantFive: 0, wantSev: 0, fiveHas: false,
		},
		{
			name:    "missing 5h utilization fails closed",
			body:    `{"five_hour":{"resets_at":"2026-07-15T09:20:11Z"},"seven_day":{"utilization":0.1}}`,
			wantErr: KindMalformed,
		},
		{
			name:    "missing 7d utilization fails closed",
			body:    `{"five_hour":{"utilization":0.1},"seven_day":{}}`,
			wantErr: KindMalformed,
		},
		{
			name:    "not JSON fails closed",
			body:    `not json at all`,
			wantErr: KindMalformed,
		},
		{
			name:    "error body (fields absent) fails closed",
			body:    `{"type":"error","error":{"type":"authentication_error","message":"bad"}}`,
			wantErr: KindMalformed,
		},
		{
			name:     "unparseable reset keeps the reading, nil reset",
			body:     `{"five_hour":{"utilization":0.4,"resets_at":"tomorrow-ish"},"seven_day":{"utilization":0.4}}`,
			wantErr:  -1,
			wantFive: 40, wantSev: 40, fiveHas: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := parseUsage([]byte(tt.body))
			if tt.wantErr != -1 {
				var ae *Error
				if !errors.As(err, &ae) || ae.Kind != tt.wantErr {
					t.Fatalf("want Error kind %d, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.Source != SourceUsageEndpoint {
				t.Errorf("source = %q, want %q", r.Source, SourceUsageEndpoint)
			}
			if r.FiveHour.Pct != tt.wantFive || r.SevenDay.Pct != tt.wantSev {
				t.Errorf("pct = (%d,%d), want (%d,%d)", r.FiveHour.Pct, r.SevenDay.Pct, tt.wantFive, tt.wantSev)
			}
			if (r.FiveHour.ResetsAt != nil) != tt.fiveHas {
				t.Errorf("5h reset present = %v, want %v", r.FiveHour.ResetsAt != nil, tt.fiveHas)
			}
		})
	}
}

func TestParseUsageResetEpoch(t *testing.T) {
	r, err := parseUsage([]byte(`{"five_hour":{"utilization":0.5,"resets_at":"2026-07-15T09:20:11Z"},"seven_day":{"utilization":0.5}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 15, 9, 20, 11, 0, time.UTC)
	if r.FiveHour.ResetsAt == nil || !r.FiveHour.ResetsAt.Equal(want) {
		t.Fatalf("5h reset = %v, want %v", r.FiveHour.ResetsAt, want)
	}
}

func TestParseHeaders(t *testing.T) {
	tests := []struct {
		name     string
		hdr      map[string]string
		wantErr  bool
		wantFive int
		wantSev  int
		fiveHas  bool
	}{
		{
			name: "valid",
			hdr: map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": "0.55",
				"anthropic-ratelimit-unified-7d-utilization": "0.03",
				"anthropic-ratelimit-unified-5h-reset":       "1784000000",
				"anthropic-ratelimit-unified-7d-reset":       "1784500000",
			},
			wantFive: 55, wantSev: 3, fiveHas: true,
		},
		{
			name: "utilization present, reset absent -> nil reset, still valid",
			hdr: map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": "0.9",
				"anthropic-ratelimit-unified-7d-utilization": "0.9",
			},
			wantFive: 90, wantSev: 90, fiveHas: false,
		},
		{
			name: "missing utilization header fails closed (Console API key)",
			hdr: map[string]string{
				"x-ratelimit-limit-requests": "50",
			},
			wantErr: true,
		},
		{
			name: "non-numeric utilization fails closed",
			hdr: map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": "n/a",
				"anthropic-ratelimit-unified-7d-utilization": "0.1",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.hdr {
				h.Set(k, v)
			}
			r, err := parseHeaders(h)
			if tt.wantErr {
				var ae *Error
				if !errors.As(err, &ae) || ae.Kind != KindMalformed {
					t.Fatalf("want KindMalformed, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.Source != SourceHeaderProbe {
				t.Errorf("source = %q, want %q", r.Source, SourceHeaderProbe)
			}
			if r.FiveHour.Pct != tt.wantFive || r.SevenDay.Pct != tt.wantSev {
				t.Errorf("pct = (%d,%d), want (%d,%d)", r.FiveHour.Pct, r.SevenDay.Pct, tt.wantFive, tt.wantSev)
			}
			if (r.FiveHour.ResetsAt != nil) != tt.fiveHas {
				t.Errorf("5h reset present = %v, want %v", r.FiveHour.ResetsAt != nil, tt.fiveHas)
			}
		})
	}
}

const secretToken = "sk-ant-oat-SECRETsentinelVALUE-do-not-leak"

// TestNoTokenInError is the load-bearing security test: no error path can carry
// the token. It exercises HTTP-refusal and transport failures and asserts the
// sentinel token never appears in the returned error string.
func TestNoTokenInError(t *testing.T) {
	// HTTP-refusal path: a real request carrying the token on its Authorization
	// header gets a 429 whose fixed error body has no token in it; the error is
	// built from status + that body excerpt, so it cannot leak the token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()

	c := &Client{hc: srv.Client()}
	// Point both calls at the test server via do() directly.
	_, err := c.doAndClassify(context.Background(), http.MethodGet, srv.URL, nil, []byte(secretToken))
	assertNoToken(t, err)
	var ae *Error
	if !errors.As(err, &ae) || ae.Kind != KindHTTP || ae.Status != http.StatusTooManyRequests {
		t.Fatalf("want KindHTTP 429, got %v", err)
	}

	// Transport path: an unroutable address fails the round-trip.
	c2 := New(&http.Client{Timeout: 200 * time.Millisecond})
	_, terr := c2.doAndClassify(context.Background(), http.MethodGet, "http://127.0.0.1:1/nope", nil, []byte(secretToken))
	assertNoToken(t, terr)
	if !errors.As(terr, &ae) || ae.Kind != KindTransport {
		t.Fatalf("want KindTransport, got %v", terr)
	}
}

// doAndClassify mirrors the Usage/ProbeHeaders error handling so the security test
// can exercise a real server without a live Anthropic. It is test-only.
func (c *Client) doAndClassify(ctx context.Context, method, url string, body, token []byte) (Reading, error) {
	resp, err := c.do(ctx, method, url, body, token)
	if err != nil {
		return Reading{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode != http.StatusOK {
		return Reading{}, httpError("test", resp.StatusCode, b)
	}
	return parseUsage(b)
}

func assertNoToken(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("error leaked the token: %q", err.Error())
	}
}

// TestProbeBodyPinned guards the pinned probe: cheapest Haiku, max_tokens 1, fixed
// prompt — never user/run content.
func TestProbeBodyPinned(t *testing.T) {
	s := string(probeBody)
	for _, want := range []string{`"model":"claude-haiku-4-5"`, `"max_tokens":1`, `"content":"hi"`} {
		if !strings.Contains(s, want) {
			t.Errorf("probe body missing %q; got %s", want, s)
		}
	}
}
