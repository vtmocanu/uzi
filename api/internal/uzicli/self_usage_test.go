package uzicli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestHTTPClientSelfUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/usage" {
			t.Errorf("request = %s %s, want GET /api/usage", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer uzc_test" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"lifetime":{"input_tokens":12,"cost_usd":1.25},"last_7_days":{"output_tokens":4},"run_count":3,"outcomes":{"lifetime":{"finished":2,"failed":1,"fail_origins":{"unknown":1}},"last_7_days":{"finished":1,"completed":1,"fail_origins":{}}},"lifetime_subscription_run_count":1}`))
	}))
	defer srv.Close()

	got, err := newTestClient(srv).SelfUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := apitypes.SelfUsageDTO{
		Lifetime:  apitypes.UsageDTO{InputTokens: 12, CostUSD: 1.25},
		Last7Days: apitypes.UsageDTO{OutputTokens: 4},
		RunCount:  3,
		Outcomes: apitypes.RunOutcomeWindowsDTO{
			Lifetime:  apitypes.RunOutcomesDTO{Finished: 2, Failed: 1, FailOrigins: map[string]int64{"unknown": 1}},
			Last7Days: apitypes.RunOutcomesDTO{Finished: 1, Completed: 1, FailOrigins: map[string]int64{}},
		},
		LifetimeSubscriptionRunCount: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SelfUsage = %+v, want %+v", got, want)
	}
}

func TestHTTPClientSelfUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":"forbidden"}`, code: ExitAuth},
		{name: "malformed", status: http.StatusOK, body: `{"lifetime":`, code: ExitGeneric},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := newTestClient(srv).SelfUsage(context.Background())
			if !reflect.DeepEqual(got, apitypes.SelfUsageDTO{}) {
				t.Errorf("SelfUsage = %+v, want zero DTO", got)
			}
			var exit *ExitError
			if !errors.As(err, &exit) || exit.Code != tc.code {
				t.Errorf("SelfUsage error = %v, want ExitError code %d", err, tc.code)
			}
		})
	}
}

func TestFakeClientSelfUsage(t *testing.T) {
	want := apitypes.SelfUsageDTO{RunCount: 7}
	f := &FakeClient{SelfUsageV: want}
	if got, err := f.SelfUsage(context.Background()); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("SelfUsage = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	sentinel := errors.New("usage unavailable")
	f.Err = sentinel
	if got, err := f.SelfUsage(context.Background()); !reflect.DeepEqual(got, apitypes.SelfUsageDTO{}) || !errors.Is(err, sentinel) {
		t.Errorf("SelfUsage with Err = (%+v, %v), want (zero, sentinel)", got, err)
	}
}
