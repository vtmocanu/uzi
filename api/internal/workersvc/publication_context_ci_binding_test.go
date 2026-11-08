package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestResolvePublicationSnapshotCIFixBinding(t *testing.T) {
	w := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	repo, reqID, srcID := uuid.New(), uuid.New(), uuid.New()
	branch := "agent/issue-9"
	conn := uuid.New()
	one := func(v int64) *int64 { return &v }
	build := func(bound *int64) publicationSnapshot {
		return publicationSnapshot{
			Request: publicationAuthority{ID: reqID, UserID: w.UserID, RepoID: &repo, WorkerID: &w.ID, Kind: "ci_fix",
				Status: "running", ClaimGeneration: 1, PipelineRef: &branch, CIFixMRIID: bound},
			ConnectionID: &conn, ProjectID: 5,
			Nodes: []publicationNode{{Seed: srcID, Run: publicationAuthority{ID: srcID, UserID: w.UserID, RepoID: &repo,
				Kind: "prompt", Status: "completed", Branch: &branch, MRIID: one(7)}}},
		}
	}
	req := PublicationContextRequest{ClaimGeneration: 1, Branch: branch, CandidateHead: strings.Repeat("c", 40)}
	for _, tc := range []struct {
		name   string
		bound  *int64
		status string
		reason string
	}{
		{"unbound", nil, "not_applicable", ""},
		{"bound to the source PR", one(7), "not_applicable", ""},
		{"bound to another PR", one(5), "unknown", "association_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := resolvePublicationSnapshot(build(tc.bound), w, reqID, req)
			if got.Status != tc.status || got.Reason != tc.reason {
				t.Fatalf("want %s/%q got %+v", tc.status, tc.reason, got)
			}
		})
	}
}
