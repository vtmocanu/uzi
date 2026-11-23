package workersvc

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestProviderGatewayLimitLiteralMirror pins the one literal that the server, the agent, the CLI and
// the web UI spell independently: the server's RateLimitTypeProviderGatewayLimit, the agent's
// PROVIDER_GATEWAY_LIMIT, the CLI's providerGatewayLimitType, the web's PROVIDER_GATEWAY_LIMIT, and
// the value the runs CHECK admits. A drift in any one parks or renders
// a provider gateway limit_wait wrongly, and no compiler sees across the module boundary.
func TestProviderGatewayLimitLiteralMirror(t *testing.T) {
	const want = "provider_gateway_limit"
	if RateLimitTypeProviderGatewayLimit != want {
		t.Fatalf("RateLimitTypeProviderGatewayLimit = %q, want %q", RateLimitTypeProviderGatewayLimit, want)
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path) //nolint:gosec // G304: path is one of the fixed in-repo mirror constants below, not attacker-controlled
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	mirrors := []struct{ name, path, pattern string }{
		{"agent PROVIDER_GATEWAY_LIMIT", "../../../agent/src/provider-binding.ts",
			`(?m)^export const PROVIDER_GATEWAY_LIMIT = "([^"]+)";$`},
		{"CLI providerGatewayLimitType", "../../cmd/uzi/run_render.go",
			`(?m)^const providerGatewayLimitType = "([^"]+)"$`},
		{"web PROVIDER_GATEWAY_LIMIT", "../../../web/src/lib/limitWait.ts",
			`(?m)^export const PROVIDER_GATEWAY_LIMIT = "([^"]+)";$`},
	}
	for _, m := range mirrors {
		sub := regexp.MustCompile(m.pattern).FindStringSubmatch(read(m.path))
		if sub == nil {
			t.Fatalf("%s: declaration not found in %s", m.name, m.path)
		}
		if sub[1] != RateLimitTypeProviderGatewayLimit {
			t.Fatalf("%s = %q, server RateLimitTypeProviderGatewayLimit = %q", m.name, sub[1], RateLimitTypeProviderGatewayLimit)
		}
	}

	// The runs CHECK: the Up half of 00488 must list the literal inside runs_rate_limit_type_check.
	mig := read("../store/migrations/00488_runs_provider_gateway_limit_type.sql")
	up, _, _ := strings.Cut(mig, "-- +goose Down")
	check := regexp.MustCompile(`(?s)ADD CONSTRAINT runs_rate_limit_type_check\s+CHECK \(rate_limit_type IS NULL OR rate_limit_type IN \((.*?)\)\)`).FindStringSubmatch(up)
	if check == nil {
		t.Fatal("00488 Up: runs_rate_limit_type_check definition not found")
	}
	admitted := map[string]bool{}
	for _, v := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(check[1], -1) {
		admitted[v[1]] = true
	}
	if !admitted[RateLimitTypeProviderGatewayLimit] {
		t.Fatalf("runs_rate_limit_type_check admits %v, not %q", admitted, RateLimitTypeProviderGatewayLimit)
	}
}
