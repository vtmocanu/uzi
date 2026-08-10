package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// ciAutofixStore is the subset of *store.Queries the CI-autofix detector reads and
// writes: the candidate enumeration plus the loop-guard ledger (PRD #71 M4). Kept
// as an interface for the same reason autopilotStore is — the poller unit tests
// drive it with an in-memory fake, the live SQL is pinned separately.
type ciAutofixStore interface {
	ListCIAutofixCandidateRefs(ctx context.Context, repoID uuid.UUID) ([]store.ListCIAutofixCandidateRefsRow, error)
	GetCIAutofixAttempt(ctx context.Context, arg store.GetCIAutofixAttemptParams) (store.CiAutofixAttempt, error)
	UpsertCIAutofixAttempt(ctx context.Context, arg store.UpsertCIAutofixAttemptParams) error
	RecordCIAutofixPipeline(ctx context.Context, arg store.RecordCIAutofixPipelineParams) error
	SetCIAutofixHaltNotified(ctx context.Context, arg store.SetCIAutofixHaltNotifiedParams) error
	GetActiveCIFixTargetForRef(ctx context.Context, arg store.GetActiveCIFixTargetForRefParams) (pgtype.Int8, error)
}

// CIAutoFixRunStarter creates an automatic ci_fix run through workersvc's shared
// create path (PRD #71 M4). *workersvc.Service satisfies it. Keeping run creation
// on the workersvc side (rather than re-implementing it here) is what makes an
// automatic fix and a manual "Fix CI" click share one state machine and one set of
// guards — the one-active-ci_fix-per-ref index and the cross-kind same-branch
// exclusion — so the detector receives ErrActiveFixExists / ErrBranchInUse to
// swallow on a race with the manual button.
type CIAutoFixRunStarter interface {
	CreateAutoCIFixRun(ctx context.Context, userID, repoID uuid.UUID, ref, title, description string, snapshot workersvc.FailureSnapshot, ciConfigPaths []string) (store.Run, error)
}

// CIAutofixNotifier lands an inbox notification for the ref's owner (PRD #71 M6).
// *notifysvc.Service satisfies it. Optional (nil-safe): a detector built without a
// notifier still starts/halts runs and posts issue comments, it just skips the
// inbox row.
type CIAutofixNotifier interface {
	Notify(ctx context.Context, n notifysvc.Notification) (store.Notification, error)
}

// ciAutofixNotifyPayload is the small jsonb the inbox renders for a ci_autofix_*
// notification. Reason is set only on the halted kind.
type ciAutofixNotifyPayload struct {
	Ref            string `json:"ref"`
	PipelineWebURL string `json:"pipeline_web_url"`
	IssueIID       int64  `json:"issue_iid"`
	Reason         string `json:"reason,omitempty"`
}

// CIAutoFix is the poller's post-pipeline-sync CI-autofix detector (PRD #71 M6),
// the sibling of the autopilot detector. It is stateless and safe for concurrent
// use across the per-repo sync goroutines: it only reads its injected collaborators
// and touches distinct (repo, ref) ledger rows. Detection lives HERE, never in
// forgesvc, whose sync methods are shared with the manual board Refresh and must
// never spawn runs.
type CIAutoFix struct {
	q        ciAutofixStore
	runs     CIAutoFixRunStarter
	notifier CIAutofixNotifier

	maxJobs      int
	logTailBytes int
	maxAttempts  int
	configPaths  []string
}

// NewCIAutoFix builds a detector. q is the store, runs creates the automatic ci_fix
// runs (workersvc), notifier lands the inbox rows (notifysvc, nil-safe). maxJobs /
// logTailBytes bound the failure snapshot; maxAttempts is the auto-fix cap;
// configPaths is the guard's default CI-config watch set.
func NewCIAutoFix(q ciAutofixStore, runs CIAutoFixRunStarter, notifier CIAutofixNotifier, maxJobs, logTailBytes, maxAttempts int, configPaths []string) *CIAutoFix {
	return &CIAutoFix{
		q:            q,
		runs:         runs,
		notifier:     notifier,
		maxJobs:      maxJobs,
		logTailBytes: logTailBytes,
		maxAttempts:  maxAttempts,
		configPaths:  configPaths,
	}
}

// detect is the post-pipeline-sync CI-autofix hook. It runs after a repo's
// pipeline-status cache is fresh (only when the pipeline watch is on), a sibling of
// the autopilot detector. For each eligible failing agent-MR branch it decides,
// through the loop-guard state machine, whether to start an automatic ci_fix run,
// halt with one explanatory comment, or silently record the pipeline.
//
// Every per-candidate failure is log-and-skipped (the poller convention): a forge
// blip or a DB error on one ref must not stall the tick or the other refs, and an
// unrecorded pipeline simply re-evaluates next tick.
func (d *CIAutoFix) detect(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge) {
	cands, err := d.q.ListCIAutofixCandidateRefs(ctx, r.ID)
	if err != nil {
		slog.Error("poller: list ci-autofix candidates", "repo", r.PathWithNamespace, "error", err)
		return
	}
	if len(cands) == 0 {
		return
	}
	for _, cand := range cands {
		if ctx.Err() != nil {
			return
		}
		d.detectOne(ctx, r, f, cand)
	}
}

// detectOne runs the loop-guard state machine for one candidate ref (PRD #71
// sequence diagram): dedup, active-run swallow, no-progress / cap halt, or proceed.
func (d *CIAutoFix) detectOne(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge, cand store.ListCIAutofixCandidateRefsRow) {
	ref := cand.Ref.String

	// The candidate query already guarantees an agent MR branch, but never post on a
	// ref that does not parse to an issue iid (defensive — the issue comment target is
	// derived from it). An unparseable ref is skipped entirely.
	iid, ok := issueIIDFromBranch(ref)
	if !ok {
		slog.Warn("poller: ci-autofix unparseable ref", "repo", r.PathWithNamespace, "ref", ref)
		return
	}

	// 1. Attempt state. CARRY-FORWARD FROM M4: GetCIAutofixAttempt returns a row with
	// attempt_count=0 for a ref that was only ever RecordCIAutofixPipeline'd (the
	// swallow path), so every gate below keys on attempt_count, NEVER on row-existence.
	// pgx.ErrNoRows is the zero attempt (count=0, no signature, no pipeline, not
	// halt-notified).
	attempt, err := d.q.GetCIAutofixAttempt(ctx, store.GetCIAutofixAttemptParams{RepoID: r.ID, Ref: ref})
	switch {
	case err == nil, errors.Is(err, pgx.ErrNoRows):
		// zero attempt on ErrNoRows (the generated :one returns a zero-value struct).
	default:
		slog.Error("poller: ci-autofix get attempt", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}
	count := int(attempt.AttemptCount)
	lastPipelineValid := attempt.LastPipelineID.Valid
	lastPipelineID := attempt.LastPipelineID.Int64
	lastSigValid := attempt.LastSignature.Valid
	lastSig := attempt.LastSignature.String
	haltNotified := attempt.HaltNotified

	// 2. DEDUP: this pipeline was already the last one handled for this ref. Nothing to do.
	if lastPipelineValid && lastPipelineID == cand.PipelineID {
		return
	}

	// 3. ACTIVE-RUN SWALLOW (M-b): a ci_fix run is already in flight for this ref.
	// Record this pipeline WITHOUT advancing the counter, but cap the recorded
	// last_pipeline_id at the active run's TARGET (its failing pipeline) — so the
	// fix's OWN result pipeline, which is newer than the target, is not deduped away
	// and gets evaluated as attempt N+1 on a later tick.
	target, err := d.q.GetActiveCIFixTargetForRef(ctx, store.GetActiveCIFixTargetForRefParams{
		RepoID: r.ID,
		Ref:    pgtype.Text{String: ref, Valid: true},
	})
	switch {
	case err == nil:
		recordID := cand.PipelineID
		if target.Valid && target.Int64 < recordID {
			recordID = target.Int64
		}
		d.recordPipeline(ctx, r, ref, recordID)
		return
	case errors.Is(err, pgx.ErrNoRows):
		// No active fix on this ref: fall through.
	default:
		slog.Error("poller: ci-autofix active-fix target", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}

	// 4. Build the snapshot + signature NOW: the no-progress check needs the current
	// signature, and the proceed path needs the snapshot to freeze onto the run. A
	// transient forge error leaves the ref for the next tick (unrecorded).
	ps := store.PipelineStatus{
		PipelineID: cand.PipelineID,
		Ref:        ref,
		Sha:        cand.Sha,
		WebUrl:     cand.PipelineWebUrl,
	}
	snap, err := workersvc.BuildFailureSnapshot(ctx, f, r.ForgeProjectID, ps, d.maxJobs, d.logTailBytes)
	if err != nil {
		// Already PAT-redacted by the driver.
		slog.Warn("poller: ci-autofix build snapshot", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}
	sig := workersvc.FailureSignature(snap)

	// 5. HALT decision. capped: we have spent the attempt budget. noProgress: the last
	// auto attempt targeted the identical failure signature, so another try would loop.
	noProgress := count >= 1 && lastSigValid && lastSig == sig
	capped := count >= d.maxAttempts
	if capped || noProgress {
		reason := "the last fix made no progress (same failure)"
		if capped {
			reason = fmt.Sprintf("reached the %d-attempt limit", d.maxAttempts)
		}
		if !haltNotified {
			// RECORD-THEN-COMMENT: latch halt_notified FIRST (it also stamps this
			// pipeline). If the latch does not persist, do not comment — a comment
			// without the latch could re-post next tick.
			if err := d.q.SetCIAutofixHaltNotified(ctx, store.SetCIAutofixHaltNotifiedParams{
				LastPipelineID: pgtype.Int8{Int64: cand.PipelineID, Valid: true},
				RepoID:         r.ID,
				Ref:            ref,
			}); err != nil {
				slog.Error("poller: ci-autofix set halt-notified", "repo", r.PathWithNamespace, "ref", ref, "error", err)
				return
			}
			// The issue comment is the primary outward halt signal (best-effort).
			if _, err := f.CreateIssueNote(ctx, r.ForgeProjectID, iid, haltCommentBody(reason, count, cand.MrIid)); err != nil {
				// Already PAT-redacted by the driver; the latch is set, so the comment is lost, not retried.
				slog.Warn("poller: ci-autofix halt comment", "repo", r.PathWithNamespace, "ref", ref, "error", err)
			}
			d.notifyHalt(ctx, cand, iid, reason)
		} else {
			// Already notified: silently move last_pipeline_id past this pipeline so it
			// is not re-evaluated. No second comment.
			d.recordPipeline(ctx, r, ref, cand.PipelineID)
		}
		return
	}

	// 6. PROCEED: start an automatic ci_fix run. Resolve the guard's watch set (default
	// globs unioned with the project's own ci_config_path); a fetch failure degrades to
	// the defaults.
	projectPath, err := f.ProjectCIConfigPath(ctx, r.ForgeProjectID)
	if err != nil {
		slog.Warn("poller: ci-autofix ci_config_path", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		projectPath = ""
	}
	ciConfigPaths := workersvc.MergeCIConfigPaths(d.configPaths, projectPath)

	// Mirror the manual Fix-CI handler's title/description (handler/ci_fix.go).
	title := fmt.Sprintf("Fix CI: %s pipeline #%d", ref, cand.PipelineID)
	description := fmt.Sprintf("Diagnose and fix the failed pipeline for `%s`.\n\nFailing pipeline: %s", ref, cand.PipelineWebUrl)

	// CREATE-THEN-COMMENT: create the run first so a race with the manual button never
	// draws a spurious "started" comment. On success, UPSERT-THEN-COMMENT — the ledger
	// write (which increments the counter) is what makes the next tick's active-run
	// swallow prevent a double run if a crash follows; a lost comment is the accepted trade.
	run, err := d.runs.CreateAutoCIFixRun(ctx, cand.UserID, r.ID, ref, title, description, snap, ciConfigPaths)
	switch {
	case err == nil:
		if err := d.q.UpsertCIAutofixAttempt(ctx, store.UpsertCIAutofixAttemptParams{
			RepoID:         r.ID,
			Ref:            ref,
			LastSignature:  pgtype.Text{String: sig, Valid: true},
			LastPipelineID: pgtype.Int8{Int64: cand.PipelineID, Valid: true},
		}); err != nil {
			// The run is active; the next tick's active-run swallow keeps it from doubling.
			slog.Error("poller: ci-autofix upsert attempt", "repo", r.PathWithNamespace, "ref", ref, "error", err)
			return
		}
		if _, err := f.CreateIssueNote(ctx, r.ForgeProjectID, iid, startCommentBody(ref, cand.PipelineWebUrl)); err != nil {
			slog.Warn("poller: ci-autofix start comment", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		}
		d.notifyStarted(ctx, cand, iid, run.ID)
	case errors.Is(err, workersvc.ErrActiveFixExists), errors.Is(err, workersvc.ErrBranchInUse):
		// A race with the manual Fix-CI button (or an issue run on the branch): swallow,
		// no comment, do not advance the counter.
		d.recordPipeline(ctx, r, ref, cand.PipelineID)
	default:
		// ErrRepoNotFound / a transient DB error: leave unrecorded so the next tick retries.
		slog.Error("poller: ci-autofix create run", "repo", r.PathWithNamespace, "ref", ref, "error", err)
	}
}

// recordPipeline moves last_pipeline_id WITHOUT advancing the attempt counter (the
// swallow / silent path). Best-effort: a write failure only re-evaluates the ref
// next tick, which the guards make a no-op.
func (d *CIAutoFix) recordPipeline(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, ref string, pipelineID int64) {
	if err := d.q.RecordCIAutofixPipeline(ctx, store.RecordCIAutofixPipelineParams{
		RepoID:         r.ID,
		Ref:            ref,
		LastPipelineID: pgtype.Int8{Int64: pipelineID, Valid: true},
	}); err != nil {
		slog.Error("poller: ci-autofix record pipeline", "repo", r.PathWithNamespace, "ref", ref, "error", err)
	}
}

// notifyStarted lands the ci_autofix_started inbox row for the ref's owner
// (best-effort, nil-safe). RunID anchors it to the started run. Slack is nil
// (INBOX-ONLY): the run-failure notifier already DMs the last attempt's failure on
// PublishState, so a Slack halt/start DM would double up; the issue comment is the
// primary outward signal.
func (d *CIAutoFix) notifyStarted(ctx context.Context, cand store.ListCIAutofixCandidateRefsRow, iid int64, runID uuid.UUID) {
	if d.notifier == nil {
		return
	}
	if _, err := d.notifier.Notify(ctx, notifysvc.Notification{
		UserID:  cand.UserID,
		Kind:    "ci_autofix_started",
		Payload: ciAutofixNotifyPayload{Ref: cand.Ref.String, PipelineWebURL: cand.PipelineWebUrl, IssueIID: iid},
		RunID:   &runID,
		Slack:   nil,
	}); err != nil {
		slog.Warn("poller: ci-autofix notify started", "user", cand.UserID.String(), "error", err)
	}
}

// notifyHalt lands the ci_autofix_halted inbox row for the ref's owner
// (best-effort, nil-safe). No RunID (no run was started). Slack is nil for the same
// reason notifyStarted's is — see there.
func (d *CIAutoFix) notifyHalt(ctx context.Context, cand store.ListCIAutofixCandidateRefsRow, iid int64, reason string) {
	if d.notifier == nil {
		return
	}
	if _, err := d.notifier.Notify(ctx, notifysvc.Notification{
		UserID:  cand.UserID,
		Kind:    "ci_autofix_halted",
		Payload: ciAutofixNotifyPayload{Ref: cand.Ref.String, PipelineWebURL: cand.PipelineWebUrl, IssueIID: iid, Reason: reason},
		RunID:   nil,
		Slack:   nil,
	}); err != nil {
		slog.Warn("poller: ci-autofix notify halt", "user", cand.UserID.String(), "error", err)
	}
}

// issueIIDFromBranch parses the issue iid out of an agent MR branch. It is the
// inverse of the hard contract at workersvc/ci_fix.go — agentIssueBranch formats
// `agent/issue-<iid>` — and returns false for anything that does not match, so the
// detector never posts on a ref it cannot attribute to an issue.
func issueIIDFromBranch(ref string) (int64, bool) {
	const prefix = "agent/issue-"
	rest, ok := strings.CutPrefix(ref, prefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// Comment bodies. Posted to the forge (user-facing), so they avoid em dashes and
// name the manual "Fix CI" button as the escape hatch after a halt.

func startCommentBody(ref, pipelineWebURL string) string {
	return fmt.Sprintf(
		"**Automatic CI fix started.**\n\n"+
			"uzi noticed the pipeline for `%s` failed and started an automatic fix run. "+
			"Failing pipeline: %s\n\n"+
			"Follow it on the run in uzi. To take over instead, stop the run and use the \"Fix CI\" button to run it yourself.",
		ref, pipelineWebURL)
}

func haltCommentBody(reason string, count int, mrIid pgtype.Int8) string {
	attempts := "attempt"
	if count != 1 {
		attempts = "attempts"
	}
	mrRef := ""
	if mrIid.Valid {
		mrRef = fmt.Sprintf(" on merge request !%d", mrIid.Int64)
	}
	return fmt.Sprintf(
		"**Automatic CI fix stopped.**\n\n"+
			"uzi tried to fix this branch's failing pipeline%s automatically but stopped: %s. "+
			"It made %d %s.\n\n"+
			"Nothing else will happen automatically. If you still want a fix, use the \"Fix CI\" button in uzi to start one manually.",
		mrRef, reason, count, attempts)
}
