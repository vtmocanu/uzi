package workersvc

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// checkpointBranch returns the forge-checkpoint branch for a run whose kind is
// checkpoint-eligible, derived ENTIRELY from validated run-row fields (kind, the
// run's uuid, the issue iid) — never from a worker-supplied string. ok is false
// for a kind that carries no checkpoint or an issue run missing its iid; the
// caller then treats the publish/delete as unsupported (a benign skip / no-op).
//
// Kind is dispatched FIRST: a self_improve run also carries a valid issue_iid (a
// stable tracking-issue container), so gating on issueIid alone would misroute it
// to the issue branch. See PRD #1062 M3.
func checkpointBranch(kind string, runID uuid.UUID, issueIid pgtype.Int8) (string, bool) {
	switch kind {
	case runkind.Issue:
		if !issueIid.Valid {
			return "", false
		}
		return agentIssueBranch(issueIid.Int64), true
	case runkind.SelfImprove:
		return selfImproveBranch(runID), true
	case runkind.Prompt:
		return promptBranch(runID), true
	default:
		return "", false
	}
}

// selfImproveBranch mirrors the worker's agent/src/self-improve.ts selfImproveBranch:
// uzi/self-improve/<runId>, keyed on the run uuid (fresh per cycle). The worker's
// runId == this runID.String() (both canonical lowercase-hyphenated), pinned by the
// cross-language contract in checkpoint_branch_contract_test.go.
func selfImproveBranch(runID uuid.UUID) string {
	return "uzi/self-improve/" + runID.String()
}

// promptBranch mirrors the worker's agent/src/run-kind.ts deriveCloneKey prompt arm:
// uzi/prompt-<runId>, keyed on the run uuid (issue #4116). The SQL mirror is the prompt arm of
// fn_checkpoint_coordinates (migration 00422); fixtures/checkpoint-branch pins all three.
func promptBranch(runID uuid.UUID) string {
	return "uzi/prompt-" + runID.String()
}
