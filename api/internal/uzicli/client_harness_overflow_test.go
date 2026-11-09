package uzicli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPClientSetHarnessOverflowEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[enabled], func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body map[string]bool
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				value, present := body["harness_overflow_enabled"]
				if r.Method != http.MethodPut || r.URL.Path != "/api/me/harness-overflow" || !present || len(body) != 1 || value != enabled {
					t.Errorf("request = %s %s %+v", r.Method, r.URL.Path, body)
				}
				// A different stored value proves the client decodes instead of echoing.
				_ = json.NewEncoder(w).Encode(map[string]bool{"harness_overflow_enabled": !enabled})
			}))
			defer srv.Close()
			got, err := newTestClient(srv).SetHarnessOverflowEnabled(context.Background(), enabled)
			if err != nil || got != !enabled || calls != 1 {
				t.Fatalf("got=%v calls=%d err=%v", got, calls, err)
			}
		})
	}
}

func TestHTTPClientSetHarnessOverflowErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"refused", `{"error":"not allowed"}`, http.StatusForbidden},
		{"malformed", `{"harness_overflow_enabled":`, http.StatusOK},
		{"wrong type", `{"harness_overflow_enabled":"true"}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			if _, err := newTestClient(srv).SetHarnessOverflowEnabled(context.Background(), true); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
