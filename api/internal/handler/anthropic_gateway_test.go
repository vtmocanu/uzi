package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func gatewayHandler(allow []string, rows ...store.AppSetting) *Handler {
	return &Handler{
		settings: settings.New(&settingsStore{rows: rows}, time.Minute),
		cfg:      config.Config{AnthropicGatewayAllowedBaseURLs: allow},
	}
}

// The wire workers read: the gateway when configured AND allowlisted, otherwise null,
// so narrowing the operator allowlist stops every worker sending tokens there.
func TestAnthropicGatewayWireFollowsSettingAndAllowlist(t *testing.T) {
	rows := []store.AppSetting{
		{Key: settings.KeyAnthropicGatewayBaseURL, Value: "https://gw.example.com/anthropic"},
		{Key: settings.KeyAnthropicGatewayAuthHeader, Value: "x-api-key"},
		{Key: settings.KeyAnthropicGatewayModels, Value: "opus=gw-opus,haiku=gw-haiku"},
	}
	got := gatewayHandler([]string{"https://gw.example.com"}, rows...).anthropicGatewayWire(context.Background())
	if got == nil || got.BaseURL != "https://gw.example.com/anthropic" || got.AuthHeader != "x-api-key" ||
		got.Models["opus"] != "gw-opus" || got.Models["haiku"] != "gw-haiku" {
		t.Fatalf("wire = %+v, want the configured gateway", got)
	}
	// Configured but unusable is BLOCKED, never off: the tokens are gateway keys.
	if w := gatewayHandler(nil, rows...).anthropicGatewayWire(context.Background()); w == nil || !w.Blocked || w.BaseURL != "" {
		t.Fatalf("empty allowlist: wire = %+v, want blocked", w)
	}
	if w := gatewayHandler([]string{"https://other.example.com"}, rows...).anthropicGatewayWire(context.Background()); w == nil || !w.Blocked {
		t.Fatalf("host not allowlisted: wire = %+v, want blocked", w)
	}
	if w := gatewayHandler([]string{"https://gw.example.com"}).anthropicGatewayWire(context.Background()); w != nil {
		t.Fatalf("no gateway configured: wire = %+v, want nil", w)
	}
}

func TestUpdateSettingsGatewayURLMustBeAllowlisted(t *testing.T) {
	admin := adminUser()
	for _, allow := range [][]string{nil, {"https://other.example.com"}} {
		rec := httptest.NewRecorder()
		gatewayHandler(allow).UpdateSettings(rec, putSettings(&admin, `{"settings":{"anthropic_gateway_base_url":"https://gw.example.com"}}`))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), settings.KeyAnthropicGatewayBaseURL) {
			t.Fatalf("allow=%v: status %d body %s, want a 400 naming the key", allow, rec.Code, rec.Body.String())
		}
	}
}

type gatewayTesterFake struct {
	err   error
	calls int
}

func (f *gatewayTesterFake) Usage(context.Context, []byte) (anthropic.Reading, error) {
	return anthropic.Reading{}, errors.New("unexpected usage call behind a gateway")
}
func (f *gatewayTesterFake) ProbeTest(context.Context, []byte) (anthropic.Reading, bool, error) {
	return anthropic.Reading{}, false, errors.New("unexpected direct probe behind a gateway")
}
func (f *gatewayTesterFake) GatewayProbe(context.Context, anthropic.Gateway, []byte) error {
	f.calls++
	return f.err
}

// Behind a gateway only the gateway's own answer decides: 2xx clears, 401/403 rejects,
// anything else (an unknown probe model, a 5xx, a redirect, transport) is inconclusive.
func TestGatewayOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"accepted", nil, "cleared"},
		{"unauthorized", &anthropic.Error{Kind: anthropic.KindHTTP, Status: 401}, "rejected"},
		{"forbidden: often a per-key model rule", &anthropic.Error{Kind: anthropic.KindHTTP, Status: 403}, "inconclusive"},
		{"unknown model", &anthropic.Error{Kind: anthropic.KindHTTP, Status: 404}, "inconclusive"},
		{"redirect", &anthropic.Error{Kind: anthropic.KindHTTP, Status: 307}, "inconclusive"},
		{"transport", &anthropic.Error{Kind: anthropic.KindTransport}, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &gatewayTesterFake{err: tc.err}
			got := testGatewayOutcome(context.Background(), fake, anthropic.Gateway{BaseURL: "https://gw.example.com"}, []byte("k"),
				func() secretTestResult { return secretTestResult{Status: "cleared"} },
				func() secretTestResult { return secretTestResult{Status: "rejected"} })
			if got.Status != tc.want || fake.calls != 1 {
				t.Fatalf("status = %q after %d probes, want %q after 1", got.Status, fake.calls, tc.want)
			}
		})
	}
}

// While a gateway is configured a worker without anthropic_gateway_v1 claims nothing: an
// older worker would send the saved gateway key to api.anthropic.com.
func TestWorkerClaimRefusesWorkersWithoutGatewaySupport(t *testing.T) {
	h := gatewayHandler(nil, store.AppSetting{Key: settings.KeyAnthropicGatewayBaseURL, Value: "https://gw.example.com"})
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/claim?lane=chat", nil)
	req = req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ProtocolCapabilities: []string{"pending_outcome_scoped_claim_v1"}}))
	rec := httptest.NewRecorder()
	h.WorkerClaim(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (no claim) for a worker without gateway support", rec.Code)
	}
}

type gatewayFailingSettings struct{}

func (gatewayFailingSettings) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	return nil, errors.New("settings read failed")
}

// A failed settings read must read as "configured": the safe answer when it decides
// whether a token may go to api.anthropic.com.
func TestGatewayConfiguredOnSettingsReadFailure(t *testing.T) {
	c := settings.New(gatewayFailingSettings{}, time.Minute)
	if !c.AnthropicGatewayConfigured(context.Background()) {
		t.Fatal("a failed settings read reported no gateway")
	}
}
