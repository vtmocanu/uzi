package forge

import "context"

// This file holds the Forgejo driver's pipeline / CI-fix methods. M2 lands them
// as compiling stubs so M5 (which fills them against the gitea SDK's Actions
// surface — ListRepoActionRuns, ListRepoActionRunJobs, GetRepoActionJobLogs, all
// present only on Forgejo >= 16.0.0, which the VerifyToken gate guarantees) owns
// this file alone and cannot collide with M4's work in forgejo.go. None is
// reachable from the API until the M6b gate flip, and no caller exercises them
// before M5 lands. Each returns errForgejoNotImplemented (defined in forgejo.go).

// LatestPipeline is filled by M5.
func (f *forgejo) LatestPipeline(ctx context.Context, projectID int64, ref string) (Pipeline, error) {
	return Pipeline{}, errForgejoNotImplemented
}

// LatestMRPipeline is filled by M5.
func (f *forgejo) LatestMRPipeline(ctx context.Context, projectID, mrIID int64) (Pipeline, error) {
	return Pipeline{}, errForgejoNotImplemented
}

// ListPipelineJobs is filled by M5.
func (f *forgejo) ListPipelineJobs(ctx context.Context, projectID, pipelineID int64) ([]Job, error) {
	return nil, errForgejoNotImplemented
}

// JobLogTail is filled by M5.
func (f *forgejo) JobLogTail(ctx context.Context, projectID, jobID int64, maxBytes int) (string, error) {
	return "", errForgejoNotImplemented
}
