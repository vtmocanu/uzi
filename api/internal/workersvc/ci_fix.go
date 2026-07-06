package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Run kinds (PRD #6), mirroring the runs.kind CHECK.
const (
	RunKindIssue = "issue"
	RunKindCIFix = "ci_fix"
)

// clampWireFixVerdict permits ONLY 'not_code' from a worker's completed state
// report (PRD #6 integrity). verified / fix_failed are stamped server-side by the
// pipeline sync from the actual post-fix pipeline and are NOT worker-reportable —
// so a compromised or buggy worker cannot forge a 'verified' badge by reporting
// it on the wire. Any other value (verified, fix_failed, garbage, or absent) is
// dropped to NULL.
func clampWireFixVerdict(v *string) pgtype.Text {
	if v != nil && *v == "not_code" {
		return pgtype.Text{String: "not_code", Valid: true}
	}
	return pgtype.Text{}
}

// agentIssueBranch is the worktree/branch an issue run uses, mirroring the
// worker's naming (agent/src/git.ts: `agent/issue-${iid}`). The server needs it
// only for the cross-kind same-branch exclusion; it is not otherwise coupled to
// the worker's git layout.
func agentIssueBranch(issueIID int64) string {
	return fmt.Sprintf("agent/issue-%d", issueIID)
}

// ErrActiveFixExists is returned when a ci_fix run already exists for the ref
// (backed by the uq_runs_one_active_ci_fix partial index). The handler maps it to
// 409.
var ErrActiveFixExists = errors.New("an active CI-fix run already exists for this ref")

// ErrBranchInUse is returned when an active run of any kind already occupies the
// ref's branch/worktree (the cross-kind same-branch exclusion). Mapped to 409.
var ErrBranchInUse = errors.New("an active run already occupies this branch")

// FailureSnapshot is the frozen record of the failed pipeline a ci_fix run works
// from (PRD #6). It is captured at queue time — the pipeline_statuses cache row is
// overwritten by later syncs, so the run must carry its own copy — stored as
// runs.failure_snapshot jsonb, and delivered to the worker as ClaimPipeline. The
// log tails are the most attacker-influenceable text uzi feeds an agent; they are
// data, never instructions (the worker frames them as quoted evidence).
type FailureSnapshot struct {
	PipelineID int64         `json:"pipeline_id"`
	Ref        string        `json:"ref"`
	SHA        string        `json:"sha"`
	WebURL     string        `json:"web_url"`
	FailedJobs []SnapshotJob `json:"failed_jobs"`
}

// SnapshotJob is one failed job in a FailureSnapshot: identity + a bounded tail of
// its trace.
type SnapshotJob struct {
	Name    string `json:"name"`
	Stage   string `json:"stage"`
	WebURL  string `json:"web_url"`
	LogTail string `json:"log_tail"`
}

// ClaimPipeline is the ci_fix claim's pipeline payload — the FailureSnapshot as
// the worker receives it. Shapes match FailureSnapshot; kept as a distinct type so
// the wire contract is explicit at the claim boundary.
type ClaimPipeline struct {
	ID         int64            `json:"id"`
	Ref        string           `json:"ref"`
	SHA        string           `json:"sha"`
	WebURL     string           `json:"web_url"`
	FailedJobs []ClaimFailedJob `json:"failed_jobs"`
}

// ClaimFailedJob is one failed job delivered in the claim.
type ClaimFailedJob struct {
	Name    string `json:"name"`
	Stage   string `json:"stage"`
	WebURL  string `json:"web_url"`
	LogTail string `json:"log_tail"`
}

// claimPipelineFromSnapshot decodes a run's stored failure_snapshot jsonb into the
// claim's ClaimPipeline. A malformed/empty snapshot yields nil (the worker treats
// a ci_fix run with no pipeline as a failed claim, same as a missing credential).
func claimPipelineFromSnapshot(raw []byte) *ClaimPipeline {
	if len(raw) == 0 {
		return nil
	}
	var snap FailureSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil
	}
	jobs := make([]ClaimFailedJob, 0, len(snap.FailedJobs))
	for _, j := range snap.FailedJobs {
		jobs = append(jobs, ClaimFailedJob{Name: j.Name, Stage: j.Stage, WebURL: j.WebURL, LogTail: j.LogTail})
	}
	return &ClaimPipeline{ID: snap.PipelineID, Ref: snap.Ref, SHA: snap.SHA, WebURL: snap.WebURL, FailedJobs: jobs}
}

// CreateCIFixRun queues a ci_fix run for a failed pipeline (PRD #6). The caller
// (the handler) has already validated repo ownership and captured the failure
// snapshot from the forge; this method enforces the DB-level preconditions and
// inserts the run:
//   - the cross-kind same-branch exclusion (no active run of any kind occupying
//     the ref's branch — an issue run and a ci_fix would collide in one worktree);
//   - the one-active-ci_fix-per-ref index (a second Fix CI on the same ref → 409).
//
// title/description are the synthesized human summary stored on the run (issue_iid
// is NULL for ci_fix). snapshot is serialized to failure_snapshot jsonb.
func (s *Service) CreateCIFixRun(ctx context.Context, userID, repoID uuid.UUID, ref, title, description string, snapshot FailureSnapshot) (store.Run, error) {
	if _, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}

	// Cross-kind same-branch exclusion: refuse a fix on a ref an issue run already
	// occupies (the index below can't express this — the two partial indexes are
	// disjoint). Checked here AND at issue-run create time; git's "branch already
	// checked out" is the race-window backstop.
	active, err := s.q.CountActiveRunsWithBranch(ctx, store.CountActiveRunsWithBranchParams{
		RepoID: repoID,
		Branch: pgtype.Text{String: ref, Valid: true},
	})
	if err != nil {
		return store.Run{}, err
	}
	if active > 0 {
		return store.Run{}, ErrBranchInUse
	}

	snapJSON, err := json.Marshal(snapshot)
	if err != nil {
		return store.Run{}, fmt.Errorf("marshal failure snapshot: %w", err)
	}
	run, err := s.q.CreateCIFixRun(ctx, store.CreateCIFixRunParams{
		UserID:           userID,
		RepoID:           repoID,
		IssueTitle:       title,
		IssueDescription: description,
		PipelineID:       pgtype.Int8{Int64: snapshot.PipelineID, Valid: true},
		PipelineRef:      pgtype.Text{String: ref, Valid: true},
		FailureSnapshot:  snapJSON,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return store.Run{}, ErrActiveFixExists
		}
		return store.Run{}, err
	}
	// queued drives no column move for a ci_fix run (notifyOnce skips NULL issue_iid),
	// but firing the hook keeps the live status broadcast consistent with issue runs.
	s.notify(run.ID, "queued")
	return run, nil
}
