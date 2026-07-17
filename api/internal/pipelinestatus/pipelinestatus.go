// Package pipelinestatus is the ONE Go-side classifier for a raw forge pipeline /
// job status, the domain-side twin of web/src/lib/pipelineBadge.ts. It exists
// because the driver stores each forge's status VERBATIM (the neutral
// Pipeline/Job.Status is the raw Forgejo Actions enum or GitLab status, never
// normalized — see forge/forgejo_pipelines.go), so every consumer that classifies
// one must fold both forges' vocabularies itself or it silently mis-reads the
// forge it was not written against.
//
// That is not hypothetical: the CI-fix loop keyed its start gate, its failed-job
// snapshot, and its fix verdict on GitLab's "failed", and Forgejo Actions spells a
// failure "failure" — so a Forgejo Fix CI could never start, no failed job was ever
// snapshotted, and a re-failed fix never stamped fix_failed. A Forgejo e2e forced
// the cross-vocabulary path and caught all three (PRD #65).
//
// Keep the sets below IN SYNC with pipelineBadge.ts's PIPELINE_TONES: the web map's
// "failed" tone is exactly {failed, failure, error} and its "passed" tone is
// {success}. TestMirrorsWebPipelineBadge pins that correspondence so the two cannot
// drift silently.
package pipelinestatus

// failedStatuses are the terminal-FAILURE statuses across both forges — the
// pipelineBadge.ts "failed" tone: GitLab "failed"; Forgejo Actions run status
// "failure"; Forgejo CommitStatusState "error" (an errored status is a failure,
// never benign).
var failedStatuses = map[string]struct{}{
	"failed":  {}, // GitLab
	"failure": {}, // Forgejo Actions run status
	"error":   {}, // Forgejo CommitStatusState
}

// IsFailed reports whether a raw forge pipeline OR job status is a genuine terminal
// failure. It is the CI-fix loop's trigger gate and failed-job filter: a literal
// `== "failed"` check silently refuses every Forgejo pipeline/job, which reports
// "failure".
func IsFailed(status string) bool {
	_, ok := failedStatuses[status]
	return ok
}

// IsSuccess reports a terminal PASS. Both forges spell it "success", so this needs
// no per-forge fold — but it lives here beside IsFailed so the fix-verdict
// classification reads from one place, not a bare literal.
func IsSuccess(status string) bool {
	return status == "success"
}
