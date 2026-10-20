package forge

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
)

// SetGitHubArtifactFixtureForTesting installs instance-local artifact fixtures.
// The caller must exclusively own a fresh driver and install these before use.
// URL and DNS admission still run in artifactBlob; this does not exercise TLS dialing.
func SetGitHubArtifactFixtureForTesting(tb testing.TB, driver Forge, lookup func(context.Context, string) ([]netip.Addr, error), blob http.RoundTripper) {
	if !testing.Testing() || tb == nil {
		panic("GitHub artifact fixtures require a test")
	}
	tb.Helper()
	g, ok := driver.(*github)
	if !ok || g == nil || lookup == nil || blob == nil {
		tb.Fatal("GitHub artifact fixtures require a GitHub driver and nonnil hooks")
		return
	}
	g.artifactHooks = artifactTestHooks{lookup: lookup, blob: blob}
}
