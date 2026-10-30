package workersvc

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/redirectguard"
)

type forgeOriginFixture struct {
	URL             string `json:"url"`
	Origin          string `json:"origin"`
	Legacy          string `json:"legacy"`
	RedirectRefused bool   `json:"redirect_refused"`
}

func forgeOriginFixtures(t *testing.T) []forgeOriginFixture {
	t.Helper()
	b, err := os.ReadFile("../../../fixtures/forge-origin/normalization.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []forgeOriginFixture
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// Existing URL/allowlist semantics stay unchanged. Canonical cooldown identity
// additionally groups numeric-port and IPv6 aliases that redirectguard refuses.
func TestForgeOriginExistingURLHelpers(t *testing.T) {
	for _, c := range forgeOriginFixtures(t) {
		if c.Origin == "" {
			continue
		}
		t.Run(c.URL, func(t *testing.T) {
			clone := c.URL + ".git"
			got, err := forgeHostFromURL(clone)
			if err != nil || got != c.Legacy {
				t.Fatalf("forge host = %q, %v; want %q", got, err, c.Legacy)
			}
			base, err := config.NormalizeForgeBaseURL(c.Legacy)
			if err != nil || base != c.Legacy {
				t.Fatalf("base = %q, %v", base, err)
			}
			source, err := url.Parse(clone)
			if err != nil {
				t.Fatal(err)
			}
			target, err := url.Parse(c.Origin)
			if err != nil {
				t.Fatal(err)
			}
			err = redirectguard.SameOrigin(&http.Request{URL: target}, []*http.Request{{URL: source}})
			if (err != nil) != c.RedirectRefused {
				t.Fatalf("redirect refusal = %v, want %v", err, c.RedirectRefused)
			}
		})
	}
}
