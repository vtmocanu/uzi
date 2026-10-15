package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestArtifactUsesDocumentedRoutesAndCachedSlug(t *testing.T) {
	lookups, listings, downloads := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing authenticated API request")
		}
		switch r.URL.Path {
		case "/api/v3/repositories/7":
			lookups++
			_, _ = fmt.Fprint(w, `{"id":7,"name":"repo","owner":{"login":"group"}}`)
		case "/api/v3/repos/group/repo/actions/runs/8/artifacts":
			listings++
			_, _ = fmt.Fprint(w, `{"total_count":0,"artifacts":[]}`)
		case "/api/v3/repos/group/repo/actions/artifacts/9/zip":
			downloads++
			w.Header().Set("Location", "https://blob.example/evidence")
			w.WriteHeader(http.StatusFound)
		default:
			t.Errorf("unexpected API route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	g, err := newGitHub(server.URL, "fixture-connection-secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hooks := artifactDriver(t, nil).artifactHooks
	hooks.blob = artifactRT(func(*http.Request) (*http.Response, error) {
		return artifactResponse(http.StatusOK, "zip"), nil
	})
	g.artifactHooks = hooks
	if _, err := g.ListRunArtifacts(context.Background(), 7, 8); err != nil {
		t.Fatal(err)
	}
	if data, err := g.DownloadArtifact(context.Background(), 7, 9, 3); err != nil || string(data) != "zip" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if lookups != 1 || listings != 1 || downloads != 1 {
		t.Fatalf("lookup=%d list=%d download=%d", lookups, listings, downloads)
	}
}

func TestArtifactSlugCannotSelectAnotherAPIPath(t *testing.T) {
	for _, name := range []string{"..", ".", "repo/../../evil", "https://evil.example", "repo?x=y", strings.Repeat("x", 256)} {
		g := artifactDriver(t, func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid slug reached API transport")
			return nil, nil
		})
		g.slugs[7] = repoSlug{owner: "group", repo: name}
		if _, err := g.ListRunArtifacts(context.Background(), 7, 8); err == nil {
			t.Errorf("accepted list slug %q", name)
		}
		if _, err := g.DownloadArtifact(context.Background(), 7, 9, 3); err == nil {
			t.Errorf("accepted download slug %q", name)
		}
	}
}
