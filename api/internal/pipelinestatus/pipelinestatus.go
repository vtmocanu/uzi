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
// "failed" tone is exactly {failed, failure, error, timed_out, startup_failure} and
// its "passed" tone is {success}. TestMirrorsWebPipelineBadge pins that
// correspondence so the two cannot drift silently.
//
// But note (PRD #238 D8): the fix-trigger set here is DELIBERATELY narrower than "the
// badge's failed tone". IsFailed answers "should uzi spend a run trying to FIX this
// build?", not "is this red?". The web map's `attention`/`neutral` GitHub conclusions
// (`action_required`, `stale`, `neutral`) are NOT failures, so they are correctly
// absent below and web-only — TestMirrorsWebPipelineBadge only pins the failed/passed
// tones, so those entries are not cross-checked against IsFailed (which is correct).
package pipelinestatus

// failedStatuses are the terminal-FAILURE statuses across all three forges — the
// pipelineBadge.ts "failed" tone: GitLab "failed"; Forgejo Actions run status
// "failure"; Forgejo CommitStatusState "error" (an errored status is a failure,
// never benign); GitHub Actions conclusions "timed_out" and "startup_failure"
// ("failure" is shared with Forgejo, already present). DELIBERATELY excluded (D8):
// "cancelled" (a human cancelled — not uzi's to fix), "action_required" (a human must
// approve — attention, not a code failure), "neutral"/"skipped"/"stale" (not
// failures). Folding those into the fix trigger would launch a fix run at every
// cancelled or approval-pending build.
var failedStatuses = map[string]struct{}{
	"failed":          {}, // GitLab
	"failure":         {}, // Forgejo Actions run status / GitHub Actions conclusion
	"error":           {}, // Forgejo CommitStatusState
	"timed_out":       {}, // GitHub Actions conclusion
	"startup_failure": {}, // GitHub Actions conclusion (observed; see D8 fact-check N1)
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
