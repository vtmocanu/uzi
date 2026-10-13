package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
)

func TestSecretProbeDefaultDeadline(t *testing.T) {
	before := time.Now()
	ctx, cancel := (&Handler{}).armSecretTestProbe(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(12*time.Second)) || deadline.After(time.Now().Add(12*time.Second)) {
		t.Fatalf("default probe deadline %v (present=%v), want 12s", deadline, ok)
	}
}

type secretRoundTrip func(*http.Request) (*http.Response, error)

func (f secretRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnthropicTestOutcomes(t *testing.T) {
	tests := []struct {
		name                              string
		usageCode, messageCode            int
		usageBody                         string
		headers                           http.Header
		probeEnabled                      bool
		wantStatus, wantReason, wantWrite string
		wantCalls                         int
	}{
		{"usage unauthorized then messages accepted", 401, 200, "", nil, true, "ok", "", "clear", 2},
		{"usage forbidden then messages rejected", 403, 403, "", nil, true, "rejected", "", "reject", 2},
		{"usage unauthorized without probe", 401, 0, "", nil, false, "inconclusive", "generic", "", 1},
		{"messages unauthorized", 429, 401, "", nil, true, "rejected", "", "reject", 2},
		{"messages unavailable", 429, 503, "", nil, true, "inconclusive", "generic", "", 2},
		{"usage success", 200, 0, `{"five_hour":{"utilization":0.2},"seven_day":{"utilization":0.4}}`, nil, true, "ok", "", "success", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client := anthropic.New(&http.Client{Transport: secretRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				code, body := tt.usageCode, tt.usageBody
				if r.URL.Path == "/v1/messages" {
					code, body = tt.messageCode, "provider-private-message"
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: tt.headers}, nil
			})})
			write := ""
			got := testAnthropicOutcome(context.Background(), client, []byte("fixture"), tt.probeEnabled,
				func(anthropic.Reading) secretTestResult { write = "success"; return secretTestResult{Status: "ok"} },
				func() secretTestResult { write = "clear"; return secretTestResult{Status: "ok"} },
				func() secretTestResult { write = "reject"; return secretTestResult{Status: "rejected"} })
			if got.Status != tt.wantStatus || got.Reason != tt.wantReason || write != tt.wantWrite || calls != tt.wantCalls {
				t.Fatalf("got %+v, write %q, calls %d", got, write, calls)
			}
			if strings.Contains(got.Display+got.Reason, "provider-private-message") {
				t.Fatal("provider body leaked")
			}
		})
	}
}

func TestOpenAIModelsProbeOutcomes(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
		err  error
		want string
	}{
		{"valid models", 200, `{"object":"list","data":[{"id":"gpt-4o","object":"model"}]}`, nil, "ok"},
		{"invalid json", 200, "provider-private-message", nil, "inconclusive"},
		{"unsupported object", 200, `{"object":"error","data":[{"id":"gpt-4o","object":"model"}]}`, nil, "inconclusive"},
		{"empty list", 200, `{"object":"list","data":[]}`, nil, "ok"},
		{"large valid list", 200, `{"object":"list","data":[{"id":"gpt-4o","object":"model"}]}` + strings.Repeat(" ", 16384), nil, "ok"},
		{"invalid model", 200, `{"object":"list","data":[{"id":"gpt-4o","object":"other"}]}`, nil, "inconclusive"},
		{"trailing json", 200, `{"object":"list","data":[{"id":"gpt-4o","object":"model"}]}{}`, nil, "inconclusive"},
		{"oversize", 200, `{"object":"list","data":[{"id":"gpt-4o","object":"model"}]}` + strings.Repeat(" ", (2<<20)+1), nil, "inconclusive"},
		{"unauthorized", 401, "provider-private-message", nil, "rejected"},
		{"forbidden", 403, "provider-private-message", nil, "permission_denied"},
		{"server error", 503, "provider-private-message", nil, "inconclusive"},
		{"transport error", 0, "", errors.New("provider-private-message"), "inconclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: secretRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.String() != "https://api.openai.com/v1/models" || r.Header.Get("Authorization") != "Bearer fixture" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				if tt.err != nil {
					return nil, tt.err
				}
				return &http.Response{StatusCode: tt.code, Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header)}, nil
			})}
			got := probeOpenAIModels(context.Background(), client, []byte("fixture"))
			if got.Status != tt.want {
				t.Fatalf("got %+v, want %s", got, tt.want)
			}
			if strings.Contains(got.Display+got.Reason, "provider-private-message") {
				t.Fatal("provider response leaked")
			}
		})
	}
}
