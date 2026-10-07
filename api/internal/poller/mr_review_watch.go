package poller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/pipelinestatus"
	"github.com/vtmocanu/uzi/api/internal/settings"
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
	RemoveMRReworkPendingIDs(ctx context.Context, arg store.RemoveMRReworkPendingIDsParams) error
	SetMRReworkHaltNotified(ctx context.Context, arg store.SetMRReworkHaltNotifiedParams) error
	DeleteMRReworkLedgerNotIn(ctx context.Context, repoID uuid.UUID) (int64, error)
	// The review-comment author verdict cache and queue reads (issue #2347).
	workersvc.ReviewAuthorStore
}

// MRReworkRunStarter creates an automatic mr_rework run through workersvc's shared
// create path (PRD #700 M3), together with the cycle's ledger advance in one transaction
// (issue #2347), after re-validating freshness and the cap under the branch lock.
// *workersvc.Service satisfies it. Keeping run creation on the workersvc side is what makes
// the run go through the SAME guards — the one-active-mr_rework-per-MR index and the
// create-time cross-kind branch guard — so the detector receives ErrActiveMRReworkExists /
// ErrBranchInUse to swallow on a race, and likewise swallows the under-lock refusals
// ErrMRReworkCapReached, ErrReworkNothingNew and ErrReworkPermissionUnknown.
type MRReworkRunStarter interface {
	CreateAutoMRReworkRunAndAdvance(ctx context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, description string, res *workersvc.ReviewSnapshotResult, capLimit int) (store.Run, error)
}

// MRReviewNotifier records the halt notification and sends its Slack DM to the MR
// owner (PRD #700 M3, PRD #1650 D3). *notifysvc.Service satisfies it. Optional
// (nil-safe): a detector built without a notifier still starts/halts runs and posts
// issue comments, it just sends no DM and records no row — same contract as
// CIAutofixNotifier.
type MRReviewNotifier interface {
	Notify(ctx context.Context, n notifysvc.Notification) (store.Notification, error)
}

// MRReworkSettings resolves the two admin gates the watcher needs (PRD #700 M5
// Decision 5): the global kill-switch and the per-MR capLimit, plus the public base URL
// the halt DM links from (PRD #1650 D3). *settings.Cache satisfies it. MrReworkEnabled
// is DELIBERATELY three-state and error-propagating (see its doc): the detector maps a
// non-nil error to OFF (fail closed), so a settings-read blip never fails OPEN into
// auto-reworking every MR.
type MRReworkSettings interface {
	MrReworkEnabled(ctx context.Context) (bool, error)
	MrReworkCap(ctx context.Context) (int, error)
	// MrReviewTrustedBots is the admin allowlist of review bots whose comments skip the author
	// lookup (issue #2347). Strict like MrReworkEnabled: the detector skips the repo's tick on
	// a read error rather than send every bot comment through the lookup.
	MrReviewTrustedBots(ctx context.Context) ([]settings.TrustedBot, error)
	// PublicBaseURL is the operator-set public base URL the mr_rework_halted Slack DM
	// builds its run deep link from (PRD #1650 D3). An empty value or an error yields a
	// DM with no link, never a dropped notification.
	PublicBaseURL(ctx context.Context) (string, error)
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
	queue    workersvc.ReviewQueueMutator
	notifier MRReviewNotifier
	set      MRReworkSettings

	maxAttemptsDefault int
	quietPeriod        time.Duration

	// lookupTimeout bounds one review-comment author lookup (issue #2347); now is the clock
	// the debounce and the verdict TTL read. Tests shrink the first and fake the second.
	lookupTimeout time.Duration
	now           func() time.Time
	// maxLookups lowers the per-tick cap on queued author lookups (zero keeps the assessor's
	// own cap); tests use it to exercise the fair-progress bound with few authors.
	maxLookups int
}

// NewMRReviewWatch builds a detector. q is the store, runs creates the automatic
// mr_rework runs (workersvc), queue runs the locked mutations of the review-author queue
// (workersvc.Service too), notifier sends the halt DM (notifysvc, nil-safe),
// set resolves the admin gate + capLimit (settings). maxAttemptsDefault is the fallback
// per-MR capLimit used when the admin capLimit read errors; quietPeriod is the review-landed
// debounce (fire only once the newest review comment has settled for this long).
func NewMRReviewWatch(q mrReviewWatchStore, runs MRReworkRunStarter, queue workersvc.ReviewQueueMutator, notifier MRReviewNotifier, set MRReworkSettings, maxAttemptsDefault int, quietPeriod time.Duration) *MRReviewWatch {
	return &MRReviewWatch{
		q:                  q,
		runs:               runs,
		queue:              queue,
		notifier:           notifier,
		set:                set,
		maxAttemptsDefault: maxAttemptsDefault,
		quietPeriod:        quietPeriod,
		lookupTimeout:      workersvc.DefaultReviewLookupTimeout,
		now:                time.Now,
	}
}

// assessor builds the author assessor over the watcher's collaborators.
func (d *MRReviewWatch) assessor() *workersvc.ReviewAssessor {
	return &workersvc.ReviewAssessor{Store: d.q, Queue: d.queue, Timeout: d.lookupTimeout, Now: d.now, MaxAttempts: d.maxLookups}
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

	// The trusted review-bot allowlist is read ONCE per repo and fails CLOSED like the gate
	// above: an unreadable list skips the repo's tick (issue #2347).
	trusted, err := d.set.MrReviewTrustedBots(ctx)
	if err != nil {
		slog.Error("poller: mr-rework trusted review bots read", "repo", r.PathWithNamespace, "error", err)
		return
	}

	cands, err := d.q.ListMRReworkCandidates(ctx, r.ID)
	if err != nil {
		slog.Error("poller: list mr-rework candidates", "repo", r.PathWithNamespace, "error", err)
		return
	}

	for _, cand := range cands {
		if ctx.Err() != nil {
			return
		}
		d.detectOne(ctx, r, f, cand, capLimit, trusted)
	}

	// Reconcile once per repo, best-effort: DeleteMRReworkLedgerNotIn retains rows
	// while any qualifying opened source remains, independently of eligibility.
	// A failure is logged and retried on the next tick; candidate processing above
	// is unaffected.
	if _, err := d.q.DeleteMRReworkLedgerNotIn(ctx, r.ID); err != nil {
		slog.Warn("poller: mr-rework ledger eviction", "repo", r.PathWithNamespace, "error", err)
	}
	// Same best-effort contract for the review-author housekeeping: expired verdicts and
	// week-old queue rows.
	if err := d.assessor().EvictStale(ctx, r.ID); err != nil {
		slog.Warn("poller: mr-rework review author eviction", "repo", r.PathWithNamespace, "error", err)
	}
}

// detectOne runs the loop-guard state machine for one candidate MR (PRD #700 M3). The
// gates fire in order, each a distinct NEGATIVE case: green head pipeline → an ELIGIBLE new
// actionable comment (issue #2347) → review landed (debounce + current HeadSHA) → under the
// capLimit → branch free (checked at create time inside CreateAutoMRReworkRunAndAdvance). Any gate
// that fails is a silent no-op (no ledger write, no comment) except the capLimit halt,
// which comments once.
func (d *MRReviewWatch) detectOne(ctx context.Context, r store.ListEnabledReposWithConnectionsRow, f forge.Forge, cand store.ListMRReworkCandidatesRow, capLimit int, trusted []settings.TrustedBot) {
	ref := cand.Ref.String
	mrIID := cand.MrIid.Int64

	// Scheduled-run branches (`uzi/prompt-…`, `uzi/self-improve/…`) do not parse to an
	// issue iid, and that is now EXPECTED (PRD #908): the rework still fires for them —
	// only the halt ISSUE COMMENT is suppressed, since there is no issue to comment on
	// and the Forge interface has no MR-note write. The halt Slack DM carries the
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

	// Fetch the MR review comments first (the assessment's deadline must not be spent on
	// the listing), then assess their authors. The detector builds the snapshot itself — it
	// needs the eligible comments to gate on high-water and review-landedness — then passes it
	// to CreateAutoMRReworkRunAndAdvance, mirroring ci-autofix's BuildFailureSnapshot.
	//
	// The queue's prune bound is read BEFORE the listing: a row at or below it was admitted
	// before the comments were fetched, so this list decides its candidacy; a row a concurrent
	// assessor admits later sits above the bound and survives this tick's keep set.
	assessor := d.assessor()
	queueBound, err := assessor.QueueBound(ctx, r.ID, ref)
	if err != nil {
		slog.Warn("poller: mr-rework review author queue read", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}
	// The ledger row. No row means this MR was never reworked: the generated :one
	// returns a zero-value struct alongside pgx.ErrNoRows (attempt_count=0,
	// high_water=0, halt_notified=false, no pending ids), so every gate below keys on those
	// values, NEVER on row-existence. Read BEFORE the comment listing (and so before the
	// assessment): every pending id in it existed before the list was fetched, so a pending id
	// absent from the list is gone, whatever the forge's comment-id order (GitHub and Forgejo
	// number comment types from separate sequences, so no id comparison can stand in for this).
	// Planning against this older row is safe: the ledger merge tolerates it (GREATEST high-water,
	// the prior_high_water filter, conditional supersession pairs). Which comments are NEW (above
	// the mark, or pending) decides which authors are worth a lookup.
	led, err := d.q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: r.ID, Ref: ref})
	switch {
	case err == nil, errors.Is(err, pgx.ErrNoRows):
	default:
		slog.Error("poller: mr-rework get ledger", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}
	// Halted at the cap and already notified: every path below returns without a run for this
	// MR, so skip the listing and the author assessment (its lookups share the connection
	// token's rate limit with every other MR). What is skipped is upkeep for a fire that cannot
	// happen while halted: queue admission and pruning, pending-id eviction, and caching new
	// not-eligible verdicts. It resumes once the MR is under the cap. The first halt still
	// runs the full path, because halt_notified is false until it has notified.
	if int(led.AttemptCount) >= capLimit && led.HaltNotified {
		return
	}

	comments, err := f.ListMergeRequestComments(ctx, r.ForgeProjectID, mrIID)
	if err != nil {
		// Already PAT-redacted by the driver.
		slog.Warn("poller: mr-rework list comments", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}

	as, err := assessor.Begin(ctx, workersvc.ReviewAssessParams{
		RepoID:         r.ID,
		Ref:            ref,
		ProjectID:      r.ForgeProjectID,
		BaseURL:        r.BaseUrl,
		BotForgeUserID: cand.BotForgeUserID,
		Lookup:         f,
		Trusted:        trusted,
		Comments:       comments,
		HighWater:      led.HighWater,
		Pending:        led.PendingUnknownIds,
		QueueBound:     queueBound,
	})
	if err != nil {
		// Fail closed: with the verdicts or the queue unreadable no author is assessed.
		slog.Warn("poller: mr-rework author assessment", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		return
	}
	if as == nil {
		// Nothing left after the bot self-filter (or an unknown bot id): no fire.
		return
	}
	defer as.Close()
	if as.Attempted > 0 {
		slog.Debug("poller: mr-rework author lookups", "repo", r.PathWithNamespace, "ref", ref, "attempted", as.Attempted)
	}

	// GATE — AN ELIGIBLE NEW ACTIONABLE COMMENT (issue #2347). Only a comment from an author
	// with repository access (or an allowlisted review bot) can fire a rework; an outsider's
	// comment is never a trigger and never gates, whatever its age or head SHA. When the only
	// new comments are from authors that could not be verified, the skip is recorded, loudly
	// and with its reason, instead of reading like "nothing new".
	if !as.HasTrigger() {
		if n := as.UnknownNewCount(); n > 0 {
			slog.Warn("poller: mr-rework withheld: permission unknown",
				"repo", r.PathWithNamespace, "ref", ref, "reason", issueinput.Unknown, "count", n, "attempted", as.Attempted)
		}
		return
	}
	// Cheap early debounce on the eligible comments decided so far: assessing more authors
	// can only make the newest comment newer, so one still inside the quiet period stays
	// inside it, and the context-only lookups below are skipped this tick.
	if newest, found := as.NewestEligible(); found && d.now().Sub(newest.CreatedAt) < d.quietPeriod {
		return
	}

	res := as.Snapshot(ctx)
	// The newest ELIGIBLE comment (the snapshot is oldest-first) drives the review-landed gate.
	newest, found := res.NewestEligible()
	if !found {
		return // unreachable while HasTrigger held; never fire an empty eligible snapshot
	}
	plan := res.PlanAssessed()

	// A pending id the snapshot caps evicted falls back to human review: drop it from the
	// pending set now, even when no run is created below. Left pending it would keep HasTrigger
	// (which ignores the caps) true while plan.HasNew stays false, so every tick would repeat
	// the full assessment. Best effort: a failed write is retried by the next tick.
	if len(plan.PendingEvicted) > 0 {
		if err := d.q.RemoveMRReworkPendingIDs(ctx, store.RemoveMRReworkPendingIDsParams{RepoID: r.ID, Ref: ref, Ids: plan.PendingEvicted}); err != nil {
			slog.Warn("poller: mr-rework drop evicted pending ids", "repo", r.PathWithNamespace, "ref", ref, "error", err)
		}
	}

	// GATE 2 — REVIEW LANDED (Decision 6). Two sub-gates: a quiet-period debounce (the
	// review must have settled — the newest comment is older than quietPeriod) AND a
	// staleness check (the comment was written against the CURRENT head SHA). Where the
	// driver cannot supply a per-comment head SHA (a top-level note, HeadSHA==""), fall
	// back to the debounce alone — do not assert a gate the driver cannot back.
	if d.now().Sub(newest.CreatedAt) < d.quietPeriod {
		return // review still in flight (not debounced)
	}
	if newest.HeadSHA != "" && newest.HeadSHA != headSHA {
		return // comment written against a superseded head SHA
	}

	// GATE 3 — NEW ACTIONABLE COMMENT PAST THE HIGH-WATER (Decision 2 / SC3, issue
	// #1142). Fire only when an eligible ACTIONABLE kept comment has id STRICTLY ABOVE the
	// consumed high-water, or is pending (an earlier permission-unknown comment whose author
	// has since resolved eligible, issue #2347). A comment at/below the mark is never
	// re-acted; a non-actionable note (a bot walkthrough/summary) never counts, and a
	// summary-only tick does not advance the high-water — leaving the mark unmoved is what
	// lets a later actionable comment with a lower forge id still fire (it also avoids
	// widening the scalar-high-water skip below rather than narrowing it).
	//
	// 🔴 KNOWN LIMITATION (documented decision, mirrored from the 00168 migration
	// comment, NOT an oversight): GitHub/Forgejo source comment ids from DISTINCT
	// sequences, so this SCALAR high-water can skip a genuinely-new comment whose id is
	// below a previously-consumed comment from another sequence. It is FAIL-SAFE (a
	// skipped comment falls back to human review, never a wrong write) and bounded by
	// the capLimit. A per-sequence high-water is the robust follow-up; the scalar mark is
	// what Decision 2 + SC3 specify.
	if !plan.HasNew {
		return
	}

	// GATE 4 — UNDER THE PER-MR CAP (Decision 2). At the capLimit we HALT: latch, comment
	// once, and notify once (halt_notified). NOTIFY-THEN-LATCH-THEN-COMMENT (issue #1675):
	// the durable Slack DM is recorded first (it may be the only halt signal on an issueless
	// branch); a notify that fails to persist returns before the latch and the comment, so
	// the next tick retries. A latch-write failure returns before the comment: the next tick
	// re-notifies (a duplicate DM, accepted) but a comment never lacks its latch. Net: the DM
	// is at-least-once, the comment at-most-once.
	//
	// This gate reads the ledger taken before the comment listing and is only an early exit:
	// the authoritative cap check runs under the creation lock (CreateAutoMRReworkRunAndAdvance
	// refuses with ErrMRReworkCapReached). This gate sits after the "nothing new" gate above, so
	// when the competing cycle consumed the same comments the next tick exits there; this halt
	// path runs only once a newer eligible comment remains.
	if int(led.AttemptCount) >= capLimit {
		if !led.HaltNotified {
			if err := d.notifyHalt(ctx, cand, issueIID, capLimit); err != nil {
				slog.Warn("poller: mr-rework notify halt (halt retried next tick)", "repo", r.PathWithNamespace, "ref", ref, "error", err)
				return
			}
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
		}
		return
	}

	// GATE 5 / PROCEED — start the automatic mr_rework run. The cross-kind branch guard
	// and the one-active-mr_rework-per-MR index are enforced inside
	// CreateAutoMRReworkRunAndAdvance; a race surfaces as ErrBranchInUse /
	// ErrActiveMRReworkExists, swallowed here.
	//
	// ATOMIC CREATE-AND-RECORD: the run INSERT and the ledger advance (high_water to the max
	// ACTIONABLE kept id, attempt_count +1, the pending delta) commit in ONE transaction under
	// the branch lock, so a returned run always has its cycle recorded and a refused create
	// writes nothing. Under that lock the create also re-reads the ledger: another request or
	// cycle that consumed these comments, or spent the cap, since the pre-listing read above
	// is refused (ErrReworkNothingNew / ErrReworkPermissionUnknown / ErrMRReworkCapReached),
	// swallowed here without a ledger write; the next tick re-evaluates from the new row.
	title := fmt.Sprintf("Rework MR review: %s (!%d)", ref, mrIID)
	description := fmt.Sprintf("Address the new review comments on merge request !%d for `%s`, folding the fixes onto the existing branch.", mrIID, ref)

	_, err = d.runs.CreateAutoMRReworkRunAndAdvance(ctx, cand.UserID, r.ID, ref, mrIID, cand.SourceRunID, title, description, res, capLimit)
	switch {
	case err == nil:
	case errors.Is(err, workersvc.ErrBranchInUse), errors.Is(err, workersvc.ErrActiveMRReworkExists),
		errors.Is(err, workersvc.ErrMRReworkCapReached), errors.Is(err, workersvc.ErrReworkNothingNew),
		errors.Is(err, workersvc.ErrReworkPermissionUnknown):
		// A race with a ci_fix on the branch, a concurrent rework on this MR, or a concurrent
		// cycle that consumed these comments or spent the cap: swallow, nothing was written,
		// retry next tick.
	case errors.Is(err, workersvc.ErrNoCredentialForHarness):
		// PRD #1429 M2 (D4): the automatic rework INHERITS the source run's harness explicitly; if
		// that harness is no longer usable, creation refuses with no fallback. Record a legible,
		// static, NONSECRET skip on the poller feed (never a raw error), do NOT advance the ledger,
		// and do not retry into a doomed create — a credential change re-enables it next tick.
		slog.Warn("poller: mr-rework skipped: source-run harness has no usable credential",
			"repo", r.PathWithNamespace, "ref", ref, "source_run", cand.SourceRunID.String())
	default:
		slog.Error("poller: mr-rework create run", "repo", r.PathWithNamespace, "ref", ref, "error", err)
	}
}

// notifyHalt lands the mr_rework_halted notification for the MR owner (nil-safe: a nil
// notifier returns nil). It anchors the row to the SOURCE run (PRD #1202 D10), where the owner can
// now press "Rework now" past the cap. The halt is actionable, so it also DMs on Slack
// (PRD #1650 D3), linking to that run page. The DM is DURABLE (issue #1675), redelivered
// by the notifysvc Redeliverer until posted. The caller notifies BEFORE setting the
// halt_notified latch and returns the error so the halt is retried next tick, so the DM is
// at-least-once (a latch-write failure repeats it); the latch then keeps a settled halt
// to one DM.
//
// The ref (a forge branch name) goes into Body RAW: the notifier's SlackMrkdwn owns its
// escaping, and escaping here too would double-escape it. The cap is an int, so it may
// ride in the trusted Facts. The run link is built from the operator-set public base
// URL; an unset base or a failed read drops the link, never the notification.
func (d *MRReviewWatch) notifyHalt(ctx context.Context, cand store.ListMRReworkCandidatesRow, issueIID int64, capLimit int) error {
	if d.notifier == nil {
		return nil
	}
	runID := cand.SourceRunID
	ref := cand.Ref.String
	base, err := d.set.PublicBaseURL(ctx)
	if err != nil {
		slog.Warn("poller: mr-rework halt DM public base URL read (sending without a link)", "error", err)
		base = ""
	}
	_, err = d.notifier.Notify(ctx, notifysvc.Notification{
		UserID: cand.UserID,
		Kind:   "mr_rework_halted",
		Payload: notifysvc.CIAutofixPayload{
			Ref:      ref,
			IssueIID: issueIID,
			Reason:   fmt.Sprintf("reached the %d-cycle MR rework limit", capLimit),
		},
		RunID: &runID,
		Slack: &notifysvc.SlackRender{
			Emoji: "✋",
			Title: "MR rework stopped",
			Body:  "uzi stopped reworking review comments automatically on " + ref + ". To run another cycle, press Rework now on the run page.",
			Link:  runPageLink(base, runID),
			Facts: []string{fmt.Sprintf("`%d`-cycle limit", capLimit)},
		},
		DurableSlack: true,
	})
	return err
}

// runPageLink builds the run page deep link from the operator-set public base URL,
// mirroring the handler's runDeepLink: an empty base yields "" (no link).
func runPageLink(baseURL string, runID uuid.UUID) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return ""
	}
	return baseURL + "/runs/" + runID.String()
}

// mrReworkHaltCommentBody is the user-facing forge comment posted on the issue when an
// MR reaches its rework-cycle capLimit. User-facing, so no em dashes; it names the capLimit and
// the MR and points the human back to the review as the escape hatch.
func mrReworkHaltCommentBody(capLimit int, mrIID int64) string {
	return fmt.Sprintf(
		"**Automatic MR rework stopped.**\n\n"+
			"uzi has reached the automatic rework-cycle limit (%d) for merge request !%d and will not rework the review comments automatically anymore. "+
			"Please review the remaining comments and resolve them yourself, or make the changes and push to the branch. "+
			"To run one more cycle yourself, open the run in uzi and press Rework now, or run `uzi run rework <run-id>`; on-demand cycles do not count against this limit.",
		capLimit, mrIID)
}
