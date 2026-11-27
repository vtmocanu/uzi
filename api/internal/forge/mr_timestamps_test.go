package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// mrTimestampCase drives the merged_at/closed_at mapping for every driver with
// the same raw JSON values. extra is merged into the PR payload verbatim.
type mrTimestampCase struct {
	name       string
	extra      map[string]any
	wantMerged *time.Time
	wantClosed *time.Time
	wantState  string
}

func mrTimeP(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &t
}

func mrTimeEq(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func checkMRTimestamps(t *testing.T, mr MergeRequest, tc mrTimestampCase) {
	t.Helper()
	if mr.State != tc.wantState {
		t.Errorf("state = %q, want %q", mr.State, tc.wantState)
	}
	if !mrTimeEq(mr.MergedAt, tc.wantMerged) {
		t.Errorf("MergedAt = %v, want %v", mr.MergedAt, tc.wantMerged)
	}
	if !mrTimeEq(mr.ClosedAt, tc.wantClosed) {
		t.Errorf("ClosedAt = %v, want %v", mr.ClosedAt, tc.wantClosed)
	}
}

const (
	mrTsMerged = "2026-03-04T05:06:07Z"
	mrTsClosed = "2026-03-04T05:06:08Z"
	mrTsZero   = "0001-01-01T00:00:00Z"
)

func TestGitHubGetMergeRequestTimestamps(t *testing.T) {
	for _, tc := range []mrTimestampCase{
		{"absent", map[string]any{"state": "open"}, nil, nil, MRStateOpened},
		{"explicit-zero", map[string]any{"state": "closed", "merged_at": mrTsZero, "closed_at": mrTsZero}, mrTimeP(mrTsZero), mrTimeP(mrTsZero), MRStateClosed},
		{"closed-unmerged", map[string]any{"state": "closed", "closed_at": mrTsClosed}, nil, mrTimeP(mrTsClosed), MRStateClosed},
		{"merged-with-closed", map[string]any{"state": "closed", "merged": true, "merged_at": mrTsMerged, "closed_at": mrTsClosed}, mrTimeP(mrTsMerged), mrTimeP(mrTsClosed), MRStateMerged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"number": 3, "html_url": "https://github.com/acme/widgets/pull/3"}
			for k, v := range tc.extra {
				body[k] = v
			}
			m := newMockGitHub(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/pulls/3": func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(body) },
			})
			d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")
			mr, err := d.GetMergeRequest(context.Background(), 7, 3)
			if err != nil {
				t.Fatalf("GetMergeRequest: %v", err)
			}
			checkMRTimestamps(t, mr, tc)
		})
	}
}

func TestGitLabGetMergeRequestTimestamps(t *testing.T) {
	for _, tc := range []mrTimestampCase{
		{"absent", map[string]any{"state": "opened"}, nil, nil, "opened"},
		{"explicit-zero", map[string]any{"state": "closed", "merged_at": mrTsZero, "closed_at": mrTsZero}, mrTimeP(mrTsZero), mrTimeP(mrTsZero), "closed"},
		{"closed-unmerged", map[string]any{"state": "closed", "closed_at": mrTsClosed}, nil, mrTimeP(mrTsClosed), "closed"},
		{"merged-with-closed", map[string]any{"state": "merged", "merged_at": mrTsMerged, "closed_at": mrTsClosed}, mrTimeP(mrTsMerged), mrTimeP(mrTsClosed), "merged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"id": 5005, "iid": 13, "web_url": "https://gl/grp/a/-/merge_requests/13"}
			for k, v := range tc.extra {
				body[k] = v
			}
			m := newMockGitLab(t, map[string]http.HandlerFunc{
				"/api/v4/projects/7/merge_requests/13": func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(body) },
			})
			d := newTestDriver(t, m, "glpat-abcdefabcdef")
			mr, err := d.GetMergeRequest(context.Background(), 7, 13)
			if err != nil {
				t.Fatalf("GetMergeRequest: %v", err)
			}
			checkMRTimestamps(t, mr, tc)
		})
	}
}

func TestForgejoGetMergeRequestTimestamps(t *testing.T) {
	for _, tc := range []mrTimestampCase{
		{"absent", map[string]any{"state": "open"}, nil, nil, MRStateOpened},
		{"explicit-zero", map[string]any{"state": "closed", "merged_at": mrTsZero, "closed_at": mrTsZero}, mrTimeP(mrTsZero), mrTimeP(mrTsZero), MRStateClosed},
		{"closed-unmerged", map[string]any{"state": "closed", "closed_at": mrTsClosed}, nil, mrTimeP(mrTsClosed), MRStateClosed},
		{"merged-with-closed", map[string]any{"state": "closed", "merged": true, "merged_at": mrTsMerged, "closed_at": mrTsClosed}, mrTimeP(mrTsMerged), mrTimeP(mrTsClosed), MRStateMerged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"id": 5005, "number": 13, "html_url": "https://fj/acme/widgets/pulls/13"}
			for k, v := range tc.extra {
				body[k] = v
			}
			m := newMockForgejo(t, map[string]http.HandlerFunc{
				"/repos/acme/widgets/pulls/13": func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(body) },
			})
			d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")
			mr, err := d.GetMergeRequest(context.Background(), 7, 13)
			if err != nil {
				t.Fatalf("GetMergeRequest: %v", err)
			}
			checkMRTimestamps(t, mr, tc)
		})
	}
}
