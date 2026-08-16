package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The pagination backstop is shared code in pagination.go wired into every driver's
// accumulating loop. These tests prove (1) the ITEM cap fires — one method per driver,
// so all three call sites are shown to reach the shared guard — and (2) the PAGE cap
// fires (gitlab is enough; the mechanism is shared). On exceed the driver MUST return a
// non-nil error and a nil slice, never a truncated slice with nil err: forgesvc.FullSync
// treats a clean short fetch as authoritative and would evict cached issues.
//
// Each test lowers maxForgeItems / maxForgePages and restores it in a defer so no other
// test in the package is affected.

// TestPaginationItemCapGitLab drives ListProjects against an endpoint that always
// returns a full page with an ever-advancing next page: the accumulator grows without
// bound until the item cap trips.
func TestPaginationItemCapGitLab(t *testing.T) {
	defer func(o int) { maxForgeItems = o }(maxForgeItems)
	maxForgeItems = 5

	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects": func(w http.ResponseWriter, _ *http.Request) {
			// Perpetually non-zero next page → the loop never terminates naturally.
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "path_with_namespace": "grp/a", "web_url": "https://gl/grp/a", "default_branch": "main"},
				{"id": 2, "path_with_namespace": "grp/b", "web_url": "https://gl/grp/b", "default_branch": "main"},
				{"id": 3, "path_with_namespace": "grp/c", "web_url": "https://gl/grp/c", "default_branch": "main"},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	out, err := d.ListProjects(context.Background())
	if err == nil {
		t.Fatalf("expected an item-cap error, got nil (returned %d projects)", len(out))
	}
	if out != nil {
		t.Errorf("on backstop-exceed the driver must return a nil slice, not a truncated one (got %d)", len(out))
	}
	if !strings.Contains(err.Error(), "backstop") {
		t.Errorf("error must name the backstop, got %q", err.Error())
	}
}

// TestPaginationItemCapForgejo drives the Forgejo ListProjects loop into the item cap
// via the Link-header pagination the SDK reads.
func TestPaginationItemCapForgejo(t *testing.T) {
	defer func(o int) { maxForgeItems = o }(maxForgeItems)
	maxForgeItems = 5

	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/user/repos": func(w http.ResponseWriter, _ *http.Request) {
			// Always advertise a next page; only the page= query is read from the URL.
			w.Header().Set("Link", `<http://x/api/v1/user/repos?page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "full_name": "grp/a", "html_url": "https://fj/grp/a", "default_branch": "main", "permissions": map[string]any{"push": true}},
				{"id": 2, "full_name": "grp/b", "html_url": "https://fj/grp/b", "default_branch": "main", "permissions": map[string]any{"push": true}},
				{"id": 3, "full_name": "grp/c", "html_url": "https://fj/grp/c", "default_branch": "main", "permissions": map[string]any{"push": true}},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	out, err := d.ListProjects(context.Background())
	if err == nil {
		t.Fatalf("expected an item-cap error, got nil (returned %d projects)", len(out))
	}
	if out != nil {
		t.Errorf("on backstop-exceed the driver must return a nil slice, not a truncated one (got %d)", len(out))
	}
	if !strings.Contains(err.Error(), "backstop") {
		t.Errorf("error must name the backstop, got %q", err.Error())
	}
}

// TestPaginationItemCapGitHub drives the GitHub ListProjects loop into the item cap
// via the Link-header pagination go-github reads.
func TestPaginationItemCapGitHub(t *testing.T) {
	defer func(o int) { maxForgeItems = o }(maxForgeItems)
	maxForgeItems = 5

	var srvURL string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/user/repos": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Link", `<`+srvURL+`/api/v3/user/repos?page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "full_name": "acme/a", "html_url": "https://github.com/acme/a", "default_branch": "main", "permissions": map[string]any{"push": true}},
				{"id": 2, "full_name": "acme/b", "html_url": "https://github.com/acme/b", "default_branch": "main", "permissions": map[string]any{"push": true}},
				{"id": 3, "full_name": "acme/c", "html_url": "https://github.com/acme/c", "default_branch": "main", "permissions": map[string]any{"push": true}},
			})
		},
	})
	srvURL = m.srv.URL
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	out, err := d.ListProjects(context.Background())
	if err == nil {
		t.Fatalf("expected an item-cap error, got nil (returned %d projects)", len(out))
	}
	if out != nil {
		t.Errorf("on backstop-exceed the driver must return a nil slice, not a truncated one (got %d)", len(out))
	}
	if !strings.Contains(err.Error(), "backstop") {
		t.Errorf("error must name the backstop, got %q", err.Error())
	}
}

// TestPaginationPageCapGitLab drives the shared page cap: the accumulator never grows
// (empty pages) but the forge keeps advertising a next page, so only the page cap can
// stop the spin. The item cap can never trip here.
func TestPaginationPageCapGitLab(t *testing.T) {
	defer func(o int) { maxForgePages = o }(maxForgePages)
	maxForgePages = 3

	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects": func(w http.ResponseWriter, _ *http.Request) {
			// Empty page, perpetually non-zero next page → the spin attack.
			w.Header().Set("X-Next-Page", "2")
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	out, err := d.ListProjects(context.Background())
	if err == nil {
		t.Fatalf("expected a page-cap error, got nil (returned %d projects)", len(out))
	}
	if out != nil {
		t.Errorf("on backstop-exceed the driver must return a nil slice, not a truncated one (got %d)", len(out))
	}
	if !strings.Contains(err.Error(), "backstop") {
		t.Errorf("error must name the backstop, got %q", err.Error())
	}
}
