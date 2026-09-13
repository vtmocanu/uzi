package forge

// github_forgeview_bounds_test.go pins the driver-level cost the handler's fan-out cap
// produces (PRD #1255 D4): a cold `pulls` list on GitHub costs at most
// 1 + 2×min(open PRs, 30) inbound requests — the "61 not 81" bound. It counts the
// pulls-route requests the driver makes (the list + a per-PR Get + a per-PR reviews
// read for each returned ref), NOT the one-time /repositories/{id} slug resolution
// (cached on the driver), which is why the count is 61 and not 62. The handler layer
// caps the fan-out at forgeViewPullsLimit=30 by passing Limit=30 to
// ListMergeRequestRefs; this test reproduces that call shape directly against
// newMockGitHub serving 40 open PRs.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitHubForgeViewColdListCallBoundIs61(t *testing.T) {
	const openPRs = 40 // more than the cap, to prove the cap bites
	const cap30 = 30

	var pullsRequests atomic.Int64 // list + per-PR Get + per-PR reviews (NOT the slug read)

	list := func(w http.ResponseWriter, _ *http.Request) {
		pullsRequests.Add(1)
		items := make([]map[string]any, openPRs)
		for i := range items {
			items[i] = map[string]any{
				"number":     i + 1,
				"state":      "open",
				"head":       map[string]any{"ref": "b" + strconv.Itoa(i+1), "sha": "sha" + strconv.Itoa(i+1)},
				"updated_at": "2024-03-02T00:00:00Z",
			}
		}
		_ = json.NewEncoder(w).Encode(items)
	}
	// One subtree handler serves both the per-PR detail Get and the per-PR reviews read
	// (/pulls/{n} and /pulls/{n}/reviews both fall under /pulls/); each is one counted
	// inbound request.
	perPR := func(w http.ResponseWriter, r *http.Request) {
		pullsRequests.Add(1)
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			_ = json.NewEncoder(w).Encode([]map[string]any{}) // no reviews → one page, one request
			return
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/repos/acme/widgets/pulls/"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": n, "state": "open",
			"head": map[string]any{"ref": "b", "sha": "sha"},
			"base": map[string]any{"ref": "main"},
		})
	}
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls":  list,
		"/repos/acme/widgets/pulls/": perPR,
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	ctx := context.Background()
	refs, err := d.ListMergeRequestRefs(ctx, 7, ListMergeRequestsOptions{State: MRStateOpened, Limit: cap30})
	if err != nil {
		t.Fatalf("ListMergeRequestRefs: %v", err)
	}
	if len(refs) != cap30 {
		t.Fatalf("Limit=%d must cap the refs to %d even with %d open PRs, got %d", cap30, cap30, openPRs, len(refs))
	}
	// The handler enriches each returned ref with one GetMergeRequestSummary; mirror that
	// loop here so the count is the cold-list cost the route actually pays.
	for _, ref := range refs {
		if _, err := d.GetMergeRequestSummary(ctx, 7, ref.IID); err != nil {
			t.Fatalf("GetMergeRequestSummary(%d): %v", ref.IID, err)
		}
	}

	want := int64(1 + 2*cap30) // 1 list + per-PR Get + per-PR reviews = 61, NOT 1 + 2*40 = 81
	if got := pullsRequests.Load(); got != want {
		t.Fatalf("cold pulls list issued %d pulls-route requests, want %d (1 list + %d×(Get+reviews))", got, want, cap30)
	}
}
