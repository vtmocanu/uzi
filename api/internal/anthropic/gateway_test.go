package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const gwFakeKey = "gw-fake-key-for-tests"

func TestGatewayProbeSendsTheKeyInTheConfiguredHeader(t *testing.T) {
	for _, tc := range []struct {
		header, wantName, wantValue string
	}{
		{AuthHeaderBearer, "Authorization", "Bearer " + gwFakeKey},
		{AuthHeaderAPIKey, "X-Api-Key", gwFakeKey},
	} {
		t.Run(tc.header, func(t *testing.T) {
			var got *http.Request
			var model string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r
				var b struct{ Model string }
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &b)
				model = b.Model
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			gw := Gateway{BaseURL: srv.URL + "/anthropic/", AuthHeader: tc.header, Models: map[string]string{"sonnet": "gw-sonnet", "haiku": "gw-haiku"}}
			if err := New(srv.Client()).GatewayProbe(context.Background(), gw, []byte(gwFakeKey)); err != nil {
				t.Fatal(err)
			}
			if got.URL.Path != "/anthropic/v1/messages" {
				t.Errorf("path = %q, want /anthropic/v1/messages", got.URL.Path)
			}
			if v := got.Header.Get(tc.wantName); v != tc.wantValue {
				t.Errorf("%s = %q, want %q", tc.wantName, v, tc.wantValue)
			}
			if got.Header.Get("anthropic-beta") != "" {
				t.Error("the OAuth beta header must not reach a gateway")
			}
			if model != "gw-haiku" {
				t.Errorf("probe model = %q, want the cheapest mapped alias gw-haiku", model)
			}
		})
	}
}

// A refusal surfaces as KindHTTP with the status, and no error text carries the key
// even when the gateway echoes it back.
func TestGatewayProbeRefusalNeverCarriesTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid key " + r.Header.Get("x-api-key")))
	}))
	defer srv.Close()
	err := New(srv.Client()).GatewayProbe(context.Background(), Gateway{BaseURL: srv.URL, AuthHeader: AuthHeaderAPIKey}, []byte(gwFakeKey))
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindHTTP || e.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want KindHTTP 401", err)
	}
	if strings.Contains(err.Error(), gwFakeKey) {
		t.Fatalf("error carries the key: %q", err.Error())
	}
}

// A redirect is never followed: x-api-key would ride a cross-host hop.
func TestGatewayProbeDoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = r.Header.Get("x-api-key") != "" }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	err := New(srv.Client()).GatewayProbe(context.Background(), Gateway{BaseURL: srv.URL, AuthHeader: AuthHeaderAPIKey}, []byte(gwFakeKey))
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindHTTP || e.Status != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want KindHTTP 307", err)
	}
	if leaked {
		t.Fatal("the redirect was followed and the key reached another host")
	}
}

func TestGatewayProbeModelFallsBackToTheDefault(t *testing.T) {
	if m := (Gateway{}).ProbeModel(); m != probeModelDefault {
		t.Fatalf("ProbeModel() = %q, want %q", m, probeModelDefault)
	}
}
