package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPClientEgressProfiles pins the PRD #1906 M1 read client: the list envelope, the
// show envelope, the server's 404 surfacing as exit 4, and a name that is not a profile
// slug refused client-side (exit 2) so it cannot address a different route.
func TestHTTPClientEgressProfiles(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.EscapedPath())
		switch r.URL.EscapedPath() {
		case "/api/admin/egress-profiles":
			_, _ = w.Write([]byte(`{"egress_profiles":[{"name":"vendor-docs","hosts":["docs.vendor.com"]}]}`))
		case "/api/admin/egress-profiles/vendor-docs":
			_, _ = w.Write([]byte(`{"egress_profile":{"name":"vendor-docs","hosts":["docs.vendor.com"],"warnings":[]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"egress profile not found"}`))
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)

	list, err := c.AdminListEgressProfiles(context.Background())
	if err != nil || len(list) != 1 || list[0].Name != "vendor-docs" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	p, err := c.AdminGetEgressProfile(context.Background(), "vendor-docs")
	if err != nil || p.Name != "vendor-docs" || len(p.Hosts) != 1 {
		t.Fatalf("show = %+v, %v", p, err)
	}
	_, err = c.AdminGetEgressProfile(context.Background(), "no-such-profile")
	if ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("show no-such-profile exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
	// A name that is not a profile slug never reaches the server: url.PathEscape leaves
	// "." and ".." as they are, and a ".." segment would address the parent route.
	sent := len(gotPaths)
	for _, bad := range []string{"..", ".", "../users", "a/b", "Upper", "", "x\x1b[2J"} {
		_, err := c.AdminGetEgressProfile(context.Background(), bad)
		if ExitCodeFor(err) != ExitUsage {
			t.Errorf("show %q exit = %d (%v), want %d", bad, ExitCodeFor(err), err, ExitUsage)
		}
	}
	if len(gotPaths) != sent {
		t.Fatalf("an invalid name reached the server: %v", gotPaths[sent:])
	}
}
