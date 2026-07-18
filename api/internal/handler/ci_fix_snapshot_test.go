package handler

import (
	"context"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fixSnapForge overrides only the two pipeline methods snapshotFailedPipeline
// calls; the rest are fakeUserForge's no-op stubs.
type fixSnapForge struct {
	fakeUserForge
	jobs []forge.Job
	log  string
}

func (f *fixSnapForge) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return f.jobs, nil
}
func (f *fixSnapForge) JobLogTail(context.Context, int64, int64, int) (string, error) {
	return f.log, nil
}

// TestSnapshotFailedPipelineIncludesForgejoFailureJobs pins the ci_fix.go:144 site:
// a Forgejo Actions job reports "failure", not GitLab's "failed", so a bare
// == "failed" filter dropped every failed Forgejo job and the fix agent got no
// failure context. The failed job must be snapshotted; a passing job must not.
// (The sibling ci_fix.go:88 trigger gate uses the identical IsFailed call; it has
// no live-Postgres handler harness in this repo — see forge_test.go — so it is
// covered by the pipelinestatus unit tests plus the PRD #65 M9 e2e.)
func TestSnapshotFailedPipelineIncludesForgejoFailureJobs(t *testing.T) {
	h := &Handler{cfg: config.Config{CIFixMaxJobs: 10, CIFixLogTailBytes: 4096}}
	f := &fixSnapForge{
		jobs: []forge.Job{
			{ID: 1, Name: "build", Status: "failure"}, // Forgejo Actions failure
			{ID: 2, Name: "test", Status: "success"},  // a passing job must be excluded
		},
		log: "boom at line 5\nFAIL",
	}

	snap, err := h.snapshotFailedPipeline(context.Background(), f, 7, store.PipelineStatus{PipelineID: 4300})
	if err != nil {
		t.Fatalf("snapshotFailedPipeline: %v", err)
	}
	if len(snap.FailedJobs) != 1 {
		t.Fatalf("expected exactly the one Forgejo 'failure' job, got %d: %+v", len(snap.FailedJobs), snap.FailedJobs)
	}
	if snap.FailedJobs[0].Name != "build" {
		t.Fatalf("the 'failure' job must be snapshotted and the 'success' job excluded, got %+v", snap.FailedJobs)
	}
	if snap.FailedJobs[0].LogTail == "" {
		t.Error("the failed job's log tail must be captured")
	}
}
