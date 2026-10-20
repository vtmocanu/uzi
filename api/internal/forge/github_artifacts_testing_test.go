package forge

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func TestGitHubArtifactFixtureAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, location string
		addresses      []netip.Addr
	}{
		{"invalid URL", "http://ci-evidence.example/archive", []netip.Addr{netip.MustParseAddr("8.8.8.8")}},
		{"mixed DNS", "https://ci-evidence.example/archive", []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := artifactDriver(t, func(*http.Request) (*http.Response, error) { return artifactRedirect(tc.location), nil })
			calls := 0
			SetGitHubArtifactFixtureForTesting(t, g, func(context.Context, string) ([]netip.Addr, error) {
				return tc.addresses, nil
			}, artifactRT(func(*http.Request) (*http.Response, error) {
				calls++
				return artifactResponse(200, "unexpected"), nil
			}))
			if _, err := g.DownloadArtifact(context.Background(), 7, 9, 1024); err == nil {
				t.Fatal("unsafe destination accepted")
			}
			if calls != 0 {
				t.Fatalf("blob called %d times before admission", calls)
			}
			ordinary, err := New(TypeGitHub, "", "fixture-"+"pat", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			hooks := ordinary.(*github).artifactHooks
			if hooks.lookup != nil || hooks.blob != nil || hooks.dial != nil {
				t.Fatal("fixture changed an ordinary driver")
			}
			if g.artifactHooks.dial != nil {
				t.Fatal("fixture installed a dial hook")
			}
		})
	}
}

func TestGitHubArtifactFixtureRequiresTB(t *testing.T) {
	g, err := newGitHub("", "fixture-"+"pat", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Error("nil TB did not panic")
		}
		if g.artifactHooks.lookup != nil || g.artifactHooks.blob != nil || g.artifactHooks.dial != nil {
			t.Error("guard mutated driver")
		}
	}()
	SetGitHubArtifactFixtureForTesting(nil, g, nil, nil)
}
