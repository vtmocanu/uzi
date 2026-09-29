package uzicli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPClientEgressProfiles pins the PRD #1906 M1 read client: the list envelope, the
// show envelope, a name path-escaped into one segment (so an argument cannot address a
// different route), and the server's 404 surfacing as exit 4.
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
	_, err = c.AdminGetEgressProfile(context.Background(), "../users")
	if ExitCodeFor(err) != ExitNotFound {
		t.Fatalf("show ../users exit = %d, want %d", ExitCodeFor(err), ExitNotFound)
	}
	if last := gotPaths[len(gotPaths)-1]; last != "/api/admin/egress-profiles/..%2Fusers" {
		t.Fatalf("escaped path = %q, want the name as one escaped segment", last)
	}
}
