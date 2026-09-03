package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// mrReviewWatchStore is the subset of *store.Queries the MR-review watcher reads and
// writes: the candidate enumeration plus the loop-guard ledger (PRD #700 M3). Kept as
// an interface so the poller unit tests drive it with an in-memory fake, exactly like
// ciAutofixStore.
type mrReviewWatchStore interface {
	ListMRReworkCandidates(ctx context.Context, repoID uuid.UUID) ([]store.ListMRReworkCandidatesRow, error)
	GetMRReworkLedger(ctx context.Context, arg store.GetMRReworkLedgerParams) (store.MrReworkLedger, error)
	UpsertMRReworkLedger(ctx context.Context, arg store.UpsertMRReworkLedgerParams) error
	SetMRReworkHaltNotified(ctx context.Context, arg store.SetMRReworkHaltNotifiedParams) error
	DeleteMRReworkLedgerNotIn(ctx context.Context, arg store.DeleteMRReworkLedgerNotInParams) (int64, error)
}

// MRReworkRunStarter creates an automatic mr_rework run through workersvc's shared
// create path (PRD #700 M3). *workersvc.Service satisfies it. Keeping run creation on
// the workersvc side is what makes the run go through the SAME guards — the
// one-active-mr_rework-per-MR index and the create-time cross-kind branch guard — so
// the detector receives ErrActiveMRReworkExists / ErrBranchInUse to swallow on a race.
type MRReworkRunStarter interface {
	CreateAutoMRReworkRun(ctx context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, description string, snapshot *workersvc.ReviewCommentsSnapshot) (store.Run, error)
}

// MRReviewNotifier lands an inbox notification for the MR owner (PRD #700 M3).
// *notifysvc.Service satisfies it. Optional (nil-safe): a detector built without a
// notifier still starts/halts runs and posts issue comments, it just skips the inbox
// row — same contract as CIAutofixNotifier.
type MRReviewNotifier interface {
	Notify(ctx context.Context, n notifysvc.Notification) (store.Notification, error)
}

// MRReworkSettings resolves the two admin gates the watcher needs (PRD #700 M5
// Decision 5): the global kill-switch and the per-MR capLimit. *settings.Cache satisfies
// it. MrReworkEnabled is DELIBERATELY three-state and error-propagating (see its
// doc): the detector maps a non-nil error to OFF (fail closed), so a settings-read
// blip never fails OPEN into auto-reworking every MR.
type MRReworkSettings interface {
	MrReworkEnabled(ctx context.Context) (bool, error)
	MrReworkCap(ctx context.Context) (int, error)
}

// MRReviewWatch is the poller's post-SyncMRStates MR-review-rework detector (PRD #700
// M3), the sibling of the CI-autofix detector. It is stateless and safe for
// concurrent use across the per-repo sync goroutines: it only reads its injected
// collaborators and touches distinct (repo, ref) ledger rows. Detection lives HERE,
// never in forgesvc, whose sync methods are shared with the manual board Refresh and
// must never spawn runs.
type MRReviewWatch struct {
	q        mrReviewWatchStore
	runs     MRReworkRunStarter
	notifier MRReviewNotifier
	set      MRReworkSettings

	maxAttemptsDefault int
	quietPeriod        time.Duration
}

// NewMRReviewWatch builds a detector. q is the store, runs creates the automatic
// mr_rework runs (workersvc), notifier lands the inbox rows (notifysvc, nil-safe),
// set resolves the admin gate + capLimit (settings). maxAttemptsDefault is the fallback
// per-MR capLimit used when the admin capLimit read errors; quietPeriod is the review-landed
// debounce (fire only once the newest review comment has settled for this long).
func NewMRReviewWatch(q mrReviewWatchStore, runs MRReworkRunStarter, notifier MRReviewNotifier, set MRReworkSettings, maxAttemptsDefault int, quietPeriod time.Duration) *MRReviewWatch {
	return &MRReviewWatch{
		q:                  q,
		runs:               runs,
		notifier:           notifier,
		set:                set,
		maxAttemptsDefault: maxAttemptsDefault,
		quietPeriod:        quietPeriod,
	}
}

// detect is the post-SyncMRStates MR-review-rework hook. It runs AFTER PRD #24's
// close-edge watcher (SyncMRStates) so a fresh close/merge is authoritative — the
// candidate query gates on the watcher-owned mr_state, so a just-closed MR is already
// excluded and the watch merely halts (Decision 10). It is a sibling of the
// CI-autofix detector and reads the same pipeline-status cache.
//
// The admin global kill-switch is read ONCE per repo and fails CLOSED (Decision 5): a
// read error or a false value skips the repo entirely, so a settings blip never
// auto-reworks. Every per-candidate failure is log-and-skipped (the poller
// convention).
func (d *MRReviewWatch) detect(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge) {
	// Decision 5 fail-closed: MrReworkEnabled propagates its store error (three-state
	// read), and the detector — the caller that owns the fail-closed decision — maps a
	// non-nil error to OFF. Absent → ON is already resolved inside MrReworkEnabled.
	enabled, err := d.set.MrReworkEnabled(ctx)
	if err != nil {
		slog.Error("poller: mr-rework admin gate read", "repo", r.PathWithNamespace, "error", err)
		return // fail closed
	}
	if !enabled {
		return
	}
	// The capLimit read falls back to the compiled default on error (a junk row must not
	// silently disable the loop guard, but it also must not stall the whole feature).
	capLimit, err := d.set.MrReworkCap(ctx)
	if err != nil {
		slog.Warn("poller: mr-rework capLimit read (using default)", "repo", r.PathWithNamespace, "error", err, "default", d.maxAttemptsDefault)
		capLimit = d.maxAttemptsDefault
	}

	cands, err := d.q.ListMRReworkCandidates(ctx, r.ID)
	if err != nil {
		slog.Error("poller: list mr-rework candidates", "repo", r.PathWithNamespace, "error", err)
		return
	}

	keep := make([]string, 0, len(cands))
	for _, cand := range cands {
		if ctx.Err() != nil {
			return
		}
		keep = append(keep, cand.Ref.String)
		d.detectOne(ctx, r, f, cand, capLimit)
	}

	// Reconcile eviction (stop-on-merge / stop-on-close cleanup): drop ledger rows for
	// refs no longer in the opened-MR candidate set. A merged/closed MR left the
	// candidate set (mr_state != 'opened'), so its ledger row evicts here and a reused
	// agent/issue-N branch never inherits a stale count. Best-effort. An empty keep-set
	// clears the repo's ledger, which is correct when the last watched MR terminated.
	if _, err := d.q.DeleteMRReworkLedgerNotIn(ctx, store.DeleteMRReworkLedgerNotInParams{
		RepoID:   r.ID,
		KeepRefs: keep,
	}); err != nil {
		slog.Warn("poller: mr-rework ledger eviction", "repo", r.PathWithNamespace, "error", err)
	}
}

// detectOne runs the loop-guard state machine for one candidate MR (PRD #700 M3). The
// gates fire in order, each a distinct NEGATIVE case: green head pipeline → review
// landed (debounce + current HeadSHA) → a new comment past the high-water → under the
// capLimit → branch free (checked at create time inside CreateAutoMRReworkRun). Any gate
// that fails is a silent no-op (no ledger write, no comment) except the capLimit halt,
// which comments once.
func (d *MRReviewWatch) detectOne(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge, cand store.ListMRReworkCandidatesRow, capLimit int) {
	ref := cand.Ref.String
	mrIID := cand.MrIid.Int64

	// Scheduled-run branches (`uzi/prompt-…`, `uzi/self-improve/…`) do not parse to an
	// issue iid, and that is now EXPECTED (PRD #908): the rework still fires for them —
	// only the halt ISSUE COMMENT is suppressed, since there is no issue to comment on
	// and the Forge interface has no MR-note write. The inbox notification carries the
	// halt instead (notifyHalt fires for both branch shapes). When !ok, issueIID is 0,
	// which is fine for the notification payload. For an `agent/issue-N` branch the parse
	// still succeeds and the halt comment still posts (issue-run behavior unchanged).
	issueIID, ok := issueIIDFromBranch(ref)

	// GATE 1 — GREEN HEAD PIPELINE. The head pipeline for the branch (from the cache
	// SyncPipelines wrote this tick) must be green. A red, absent, or in-flight
	// pipeline is not green → no fire. The green pipeline's SHA IS the current MR head,
	// used for the staleness compare below.
	if !cand.PipelineStatus.Valid || !pipelinestatus.IsSuccess(cand.PipelineStatus.String) {
		return
	}
	headSHA := cand.PipelineSha.String

	// Fetch the MR review comments and build the filtered snapshot (the detector builds
	// it — it needs the kept comments to gate on high-water and review-landedness —
	// then passes it to CreateAutoMRReworkRun, mirroring ci-autofix's BuildFailureSnapshot).
	comments, err := f.ListMergeRequestComments(ctx, r.ForgeProjectID, mrIID)
	if err != nil {
		// Already PAT-redacted by the driver.
		slog.Warn("poller: mr-rework list comments", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}
	snap := workersvc.BuildReviewCommentsSnapshot(comments, cand.BotForgeUserID)
	if snap == nil || len(snap.Comments) == 0 {
		// Nothing left after the bot self-filter (or an unknown bot id): no fire.
		return
	}

	// The ledger row. No row means this MR was never reworked: the generated :one
	// returns a zero-value struct alongside pgx.ErrNoRows (attempt_count=0,
	// high_water=0, halt_notified=false), so every gate below keys on those values,
	// NEVER on row-existence.
	led, err := d.q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: r.ID, Ref: ref})
	switch {
	case err == nil, errors.Is(err, pgx.ErrNoRows):
	default:
		slog.Error("poller: mr-rework get ledger", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}

	// The newest kept comment (snapshot is oldest-first) drives the review-landed gate;
	// the max kept comment id is the advance-only high-water anchor.
	newest := snap.Comments[len(snap.Comments)-1]
	var maxID int64
	for _, c := range snap.Comments {
		if c.ID > maxID {
			maxID = c.ID
		}
	}

	// GATE 2 — REVIEW LANDED (Decision 6). Two sub-gates: a quiet-period debounce (the
	// review must have settled — the newest comment is older than quietPeriod) AND a
	// staleness check (the comment was written against the CURRENT head SHA). Where the
	// driver cannot supply a per-comment head SHA (a top-level note, HeadSHA==""), fall
	// back to the debounce alone — do not assert a gate the driver cannot back.
	if time.Since(newest.CreatedAt) < d.quietPeriod {
		return // review still in flight (not debounced)
	}
	if newest.HeadSHA != "" && newest.HeadSHA != headSHA {
		return // comment written against a superseded head SHA
	}

	// GATE 3 — NEW COMMENT PAST THE HIGH-WATER (Decision 2 / SC3). Fire only when a
	// kept comment has id STRICTLY ABOVE the consumed high-water. A comment at/below the
	// mark is never re-acted.
	//
	// 🔴 KNOWN LIMITATION (documented decision, mirrored from the 00168 migration
	// comment, NOT an oversight): GitHub/Forgejo source comment ids from DISTINCT
	// sequences, so this SCALAR high-water can skip a genuinely-new comment whose id is
	// below a previously-consumed comment from another sequence. It is FAIL-SAFE (a
	// skipped comment falls back to human review, never a wrong write) and bounded by
	// the capLimit. A per-sequence high-water is the robust follow-up; the scalar mark is
	// what Decision 2 + SC3 specify.
	if maxID <= led.HighWater {
		return
	}

	// GATE 4 — UNDER THE PER-MR CAP (Decision 2). At the capLimit we HALT: latch, comment
	// once, and notify once — never a second time (halt_notified). RECORD-THEN-COMMENT:
	// the latch write precedes the comment so a lost comment is not re-posted every tick.
	if int(led.AttemptCount) >= capLimit {
		if !led.HaltNotified {
			if err := d.q.SetMRReworkHaltNotified(ctx, store.SetMRReworkHaltNotifiedParams{RepoID: r.ID, Ref: ref}); err != nil {
				slog.Error("poller: mr-rework set halt-notified", "repo", r.PathWithNamespace, "ref", ref, "error", err)
				return
			}
			if ok {
				if _, err := f.CreateIssueNote(ctx, r.ForgeProjectID, issueIID, mrReworkHaltCommentBody(capLimit, mrIID)); err != nil {
					// Already PAT-redacted; the latch is set, so the comment is lost, not retried.
					slog.Warn("poller: mr-rework halt comment", "repo", r.PathWithNamespace, "ref", ref, "error", err)
				}
			}
			d.notifyHalt(ctx, cand, issueIID, capLimit)
		}
		return
	}

	// GATE 5 / PROCEED — start the automatic mr_rework run. The cross-kind branch guard
	// and the one-active-mr_rework-per-MR index are enforced inside CreateAutoMRReworkRun;
	// a race surfaces as ErrBranchInUse / ErrActiveMRReworkExists, swallowed here.
	//
	// CREATE-THEN-RECORD: create the run first, then advance the ledger (high_water to
	// the max kept id, attempt_count +1). A crash between the two re-evaluates next tick;
	// the same-kind index keeps it from doubling.
	title := fmt.Sprintf("Rework MR review: %s (!%d)", ref, mrIID)
	description := fmt.Sprintf("Address the new review comments on merge request !%d for `%s`, folding the fixes onto the existing branch.", mrIID, ref)

	_, err = d.runs.CreateAutoMRReworkRun(ctx, cand.UserID, r.ID, ref, mrIID, cand.SourceRunID, title, description, snap)
	switch {
	case err == nil:
		if err := d.q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{
			RepoID:    r.ID,
			Ref:       ref,
			HighWater: maxID,
		}); err != nil {
			// The run is active; the next tick's branch guard keeps it from doubling.
			slog.Error("poller: mr-rework upsert ledger", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		}
	case errors.Is(err, workersvc.ErrBranchInUse), errors.Is(err, workersvc.ErrActiveMRReworkExists):
		// A race with a ci_fix on the branch, or a concurrent rework on this MR: swallow,
		// do not advance the ledger, retry next tick.
	default:
		slog.Error("poller: mr-rework create run", "repo", r.PathWithNamespace, "ref", ref, "error", err)
	}
}

// notifyHalt lands the mr_rework_halted inbox row for the MR owner (best-effort,
// nil-safe). No RunID (no run was started). Slack is nil (inbox-only), mirroring the
// ci-autofix halt notification.
func (d *MRReviewWatch) notifyHalt(ctx context.Context, cand store.ListMRReworkCandidatesRow, issueIID int64, capLimit int) {
	if d.notifier == nil {
		return
	}
	if _, err := d.notifier.Notify(ctx, notifysvc.Notification{
		UserID: cand.UserID,
		Kind:   "mr_rework_halted",
		Payload: notifysvc.CIAutofixPayload{
			Ref:      cand.Ref.String,
			IssueIID: issueIID,
			Reason:   fmt.Sprintf("reached the %d-cycle MR rework limit", capLimit),
		},
		RunID: nil,
		Slack: nil,
	}); err != nil {
		slog.Warn("poller: mr-rework notify halt", "user", cand.UserID.String(), "error", err)
	}
}

// mrReworkHaltCommentBody is the user-facing forge comment posted on the issue when an
// MR reaches its rework-cycle capLimit. User-facing, so no em dashes; it names the capLimit and
// the MR and points the human back to the review as the escape hatch.
func mrReworkHaltCommentBody(capLimit int, mrIID int64) string {
	return fmt.Sprintf(
		"**Automatic MR rework stopped.**\n\n"+
			"uzi has reached the automatic rework-cycle limit (%d) for merge request !%d and will not rework the review comments automatically anymore. "+
			"Please review the remaining comments and resolve them yourself, or make the changes and push to the branch.",
		capLimit, mrIID)
}
