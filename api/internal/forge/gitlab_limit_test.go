package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// TestListIssuesLimitSemantics pins the PRD #158 m2a contract on ListIssuesOptions.Limit
// against a paginated GitLab mock (5 issues across two pages, 3 then 2):
//
//   - Limit == 0 (every pre-#158 caller) means NO CAP: the driver paginates and
//     returns the COMPLETE set — the regression that must not break, since callers
//     like FullSync rely on getting every issue.
//   - Limit == N > 0 returns exactly N when more exist, stopping pagination as soon
//     as N are collected (so the worker forge read bounds a list without walking the
//     whole project). N crossing a page boundary still stops at N.
//
// Only the gitlab driver proves the Limit semantics here (the forge-neutral option
// is driver-agnostic; one driver is enough to pin the pagination behaviour).
func TestListIssuesLimitSemantics(t *testing.T) {
	page2Requested := false
	issue := func(iid int) map[string]any {
		return map[string]any{
			"id": 1000 + iid, "iid": iid, "title": "i" + strconv.Itoa(iid), "state": "opened",
			"labels": []string{}, "web_url": "https://gl/grp/a/-/issues/" + strconv.Itoa(iid),
			"updated_at": "2026-07-03T10:00:00Z",
		}
	}
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/issues": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "", "1":
				w.Header().Set("X-Next-Page", "2")
				_ = json.NewEncoder(w).Encode([]map[string]any{issue(1), issue(2), issue(3)})
			case "2":
				page2Requested = true
				w.Header().Set("X-Next-Page", "")
				_ = json.NewEncoder(w).Encode([]map[string]any{issue(4), issue(5)})
			default:
				t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			}
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")
	ctx := context.Background()

	// Limit == 0 → complete set across both pages.
	page2Requested = false
	all, err := d.ListIssues(ctx, 7, ListIssuesOptions{})
	if err != nil {
		t.Fatalf("ListIssues(Limit=0): %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("Limit==0 must return the COMPLETE set (5), got %d", len(all))
	}
	if !page2Requested {
		t.Error("Limit==0 must paginate to page 2 to gather the complete set")
	}

	// Limit == 2 → exactly 2, and pagination stops before page 2 is even fetched.
	page2Requested = false
	two, err := d.ListIssues(ctx, 7, ListIssuesOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListIssues(Limit=2): %v", err)
	}
	if len(two) != 2 {
		t.Fatalf("Limit==2 must return exactly 2 when more exist, got %d", len(two))
	}
	if page2Requested {
		t.Error("Limit==2 is satisfied on page 1; it must not fetch page 2")
	}

	// Limit == 4 → crosses the page boundary and stops at 4 (3 from page 1, 1 from page 2).
	page2Requested = false
	four, err := d.ListIssues(ctx, 7, ListIssuesOptions{Limit: 4})
	if err != nil {
		t.Fatalf("ListIssues(Limit=4): %v", err)
	}
	if len(four) != 4 {
		t.Fatalf("Limit==4 must return exactly 4 across the page boundary, got %d", len(four))
	}
	if !page2Requested {
		t.Error("Limit==4 needs page 2 to reach 4 items; page 2 must be fetched")
	}
}
