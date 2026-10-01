package config

import "testing"

// TestIsolatedLaneEnabledFollowsFetcherToken: the lane reads enabled exactly when the chart
// rendered UZI_FETCHER_TOKEN_SHA256 (issue #1965); unset fails closed.
func TestIsolatedLaneEnabledFollowsFetcherToken(t *testing.T) {
	hostingBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.IsolatedLaneEnabled() {
		t.Error("lane reads enabled with UZI_FETCHER_TOKEN_SHA256 unset")
	}

	t.Setenv("UZI_FETCHER_TOKEN_SHA256", controllerTokenHashHex("the-fetcher-service-credential"))
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !cfg.IsolatedLaneEnabled() {
		t.Error("lane reads disabled with UZI_FETCHER_TOKEN_SHA256 set")
	}
}
