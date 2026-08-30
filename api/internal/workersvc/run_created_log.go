package workersvc

import (
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// logRunCreated emits the single structured provenance line for a freshly created
// run (issue #857): the "why did this fire, who/what started it" question answered
// from logs alone. Called once per run at each create entrypoint after the insert,
// only on the success path — never on a no-op (mr_rework's WHERE NOT EXISTS) or a
// duplicate (a 23505 treated as "already active/judged").
//
// The nullable columns (repo_id, issue_iid, schedule_id) are appended only when
// present: an issue-less kind (chat, judge) carries a NULL repo_id, and a non-schedule
// origin a NULL schedule_id, so guarding each keeps the line honest rather than logging
// a zero UUID.
func logRunCreated(run store.Run) {
	args := []any{
		"run_id", run.ID.String(),
		"kind", run.Kind,
		"trigger_source", run.TriggerSource,
		"user_id", run.UserID.String(),
		"auto_approve", run.AutoApprove,
	}
	if run.RepoID.Valid {
		args = append(args, "repo_id", uuid.UUID(run.RepoID.Bytes).String())
	}
	if run.IssueIid.Valid {
		args = append(args, "issue_iid", run.IssueIid.Int64)
	}
	if run.ScheduleID.Valid {
		args = append(args, "schedule_id", uuid.UUID(run.ScheduleID.Bytes).String())
	}
	slog.Info("workersvc: run created", args...)
}
