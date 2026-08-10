package forgesvc

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/pipelinestatus"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// PipelineSyncOptions configures one repo's pipeline-status sync pass (PRD #6).
type PipelineSyncOptions struct {
	// DefaultBranch is the repo's default branch, watched via LatestPipeline. Empty
	// (no default branch recorded) skips the default-branch step.
	DefaultBranch string
	// Window bounds how long a terminal-with-MR run's branch stays watched after it
	// finishes (CI_WATCH_RUN_WINDOW). Non-terminal runs are watched regardless.
	Window time.Duration
	// MaxRefs caps how many run branches are watched (newest first). <= 0 disables
	// pipeline sync entirely, preserving pre-PRD-6 behaviour (no badges, no cache).
	MaxRefs int
	// Evict, set on reconcile ticks, drops cache rows for refs no longer watched —
	// the pipeline analogue of the issues-cache reconcile eviction.
	Evict bool
}

// SyncPipelines refreshes the pipeline-status cache for one repo (PRD #6). The
// poller calls it once per repo per tick, AFTER the issue sync, so run-branch
// watching reads the freshest run set. It resolves the watched refs (the default
// branch + the windowed, capped set of agent run branches), fetches the latest
// pipeline for each — LatestMRPipeline when the run has an MR, to catch detached
// and merged-results pipelines the branch-ref query misses; LatestPipeline
// otherwise — and upserts them. On a reconcile tick it then evicts cache rows for
// refs no longer watched.
//
// Per-ref forge errors are log-and-skipped (the poller's convention): one
// unreachable ref must neither stall the tick nor wipe a good cache row (a ref
// whose fetch failed is kept in the eviction keep-set so its stale row survives
// and is retried next tick). ErrNoPipeline is not an error — it is "no CI for
// this ref", so the ref is not cached and any stale row for it is allowed to
// evict, which is how a default branch honestly flips to "no CI".
func (s *Service) SyncPipelines(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge, opts PipelineSyncOptions) error {
	if opts.MaxRefs <= 0 {
		return nil // pipeline sync disabled (CI_WATCH_MAX_REFS=0)
	}

	// keep accumulates every watched ref that should survive the reconcile eviction:
	// a ref freshly resolved to a pipeline, or one whose fetch failed (preserve its
	// stale row). Refs that returned ErrNoPipeline are deliberately omitted so they
	// age out of the cache.
	var keep []string

	// 1. Default branch. Branch pipelines only: an MR-only project's default branch
	//    honestly shows "no CI" (documented, not fabricated) because its pipelines
	//    live on refs/merge-requests/:iid/head, invisible to LatestPipeline.
	if opts.DefaultBranch != "" {
		if s.syncOneRef(ctx, repoID, forgeProjectID, opts.DefaultBranch, pgtype.Int8{}, f) {
			keep = append(keep, opts.DefaultBranch)
		}
	}

	// 2. Watched agent run branches (windowed + capped, newest first).
	refs, err := s.q.ListWatchedRunRefsForRepo(ctx, store.ListWatchedRunRefsForRepoParams{
		RepoID:        repoID,
		FinishedAfter: pgtype.Timestamptz{Time: time.Now().Add(-opts.Window), Valid: true},
		MaxRefs:       int32(opts.MaxRefs),
	})
	if err != nil {
		return err
	}
	if len(refs) >= opts.MaxRefs {
		// LIMIT returned a full page: there may be older watched branches we dropped.
		// Logged, not silent (PRD #6 Risks: "hitting the cap logs which refs were dropped").
		slog.Warn("forgesvc: pipeline watch hit the ref cap; older run branches are not watched",
			"repo", repoID, "cap", opts.MaxRefs)
	}
	for _, ref := range refs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !ref.Branch.Valid || ref.Branch.String == "" {
			continue // defensive: the query already excludes blank branches
		}
		// The run's branch is the WATCHED ref even when the pipeline is fetched via
		// the MR (detached/merged-results), so the cache — and later verification —
		// key on the branch, not the pipeline's own internal ref.
		if s.syncOneRef(ctx, repoID, forgeProjectID, ref.Branch.String, ref.MrIid, f) {
			keep = append(keep, ref.Branch.String)
		}
	}

	// 3. Reconcile eviction (reconcile ticks only), mirroring the issues cache.
	if opts.Evict {
		if _, err := s.q.DeletePipelineStatusesNotIn(ctx, store.DeletePipelineStatusesNotInParams{
			RepoID:   repoID,
			KeepRefs: keep,
		}); err != nil {
			return err
		}
		// PRD #71 M4: evict ci-autofix ledger rows for refs that left the watch set,
		// with the SAME keep-set — so a reused agent/issue-N branch never inherits a
		// stale attempt count. Best-effort: a failure here must not stall the tick or
		// undo the pipeline-cache eviction that already committed above.
		if n, err := s.q.DeleteCIAutofixAttemptsNotIn(ctx, store.DeleteCIAutofixAttemptsNotInParams{
			RepoID:   repoID,
			KeepRefs: keep,
		}); err != nil {
			slog.Warn("forgesvc: ci_autofix ledger reconcile eviction failed", "repo", repoID, "error", err)
		} else if n > 0 {
			slog.Debug("forgesvc: ci_autofix ledger rows evicted", "repo", repoID, "rows", n)
		}
	}
	return nil
}

// syncOneRef fetches and caches the latest pipeline for a single watched ref.
// When mrIID is valid the pipeline is read from the MR (catching detached and
// merged-results pipelines the branch-ref query misses); otherwise from the
// branch ref. It never returns an error — a per-ref failure is contained here.
//
// The returned keep bool says whether the ref should survive the reconcile
// eviction: true when a pipeline was cached OR the fetch/upsert failed (preserve
// the existing row and retry next tick), false only on ErrNoPipeline (genuinely
// no CI, so any stale row is allowed to evict).
func (s *Service) syncOneRef(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, ref string, mrIID pgtype.Int8, f forge.Forge) (keep bool) {
	var (
		p   forge.Pipeline
		err error
	)
	if mrIID.Valid {
		p, err = f.LatestMRPipeline(ctx, forgeProjectID, mrIID.Int64)
	} else {
		p, err = f.LatestPipeline(ctx, forgeProjectID, ref)
	}
	if err != nil {
		if errors.Is(err, forge.ErrNoPipeline) {
			return false // no CI for this ref — allow any stale row to evict
		}
		slog.Warn("forgesvc: pipeline fetch failed", "repo", repoID, "ref", ref, "error", err)
		return true // preserve the existing row; the poller cadence is the retry loop
	}

	forgeUpdated := pgtype.Timestamptz{}
	if !p.UpdatedAt.IsZero() {
		forgeUpdated = pgtype.Timestamptz{Time: p.UpdatedAt, Valid: true}
	}
	if _, err := s.q.UpsertPipelineStatus(ctx, store.UpsertPipelineStatusParams{
		RepoID:         repoID,
		Ref:            ref,
		PipelineID:     p.ID,
		Sha:            p.SHA,
		Status:         p.Status,
		WebUrl:         p.WebURL,
		ForgeUpdatedAt: forgeUpdated,
	}); err != nil {
		slog.Warn("forgesvc: pipeline upsert failed", "repo", repoID, "ref", ref, "error", err)
		return true // preserve the existing row
	}

	// "uzi verifies it's work" (PRD #6): if this ref is a ci_fix run's fix branch and
	// the observed pipeline concluded, stamp the run's verdict. A cheap column update
	// inside the sync — no worker involvement, no second loop. Returns the run it
	// stamped VERIFIED (green), so the reset-on-green path below can notify its owner.
	verifiedTarget, verified := s.maybeStampFixVerdict(ctx, repoID, ref, p)

	// Reset-on-green (PRD #71 M4): a SUCCESS pipeline for this ref clears its
	// ci-autofix attempt ledger so the next failure starts from a fresh count. It
	// fires on ANY green pipeline for the ref, independent of whether a ci_fix run is
	// stamped above — the ledger row only exists for refs that had auto attempts, so
	// the delete is a cheap no-op otherwise. Best-effort; a failure must not stall the
	// sync.
	if pipelinestatus.IsSuccess(p.Status) {
		if n, err := s.q.DeleteCIAutofixAttempt(ctx, store.DeleteCIAutofixAttemptParams{
			RepoID: repoID,
			Ref:    ref,
		}); err != nil {
			slog.Warn("forgesvc: ci_autofix ledger reset-on-green failed", "repo", repoID, "ref", ref, "error", err)
		} else if n > 0 {
			slog.Debug("forgesvc: ci_autofix ledger reset on green", "repo", repoID, "ref", ref, "rows", n)
			// Landed notification (PRD #71 M6): the ref HAD an auto-fix ledger (n>0, i.e.
			// uzi's automation attempted a fix here) AND this green pipeline verified a
			// ci_fix run — tell the owner the fix landed. Nil-safe + best-effort; inbox
			// only (Slack nil) since the run notifier already covers run outcomes.
			if verified {
				s.notifyAutofixLanded(ctx, verifiedTarget, ref, p)
			}
		}
	}
	return true
}

// maybeStampFixVerdict closes the verification loop for a ci_fix run whose fix
// branch is ref (PRD #6). It keys on runs.branch (the fix branch), NOT pipeline_ref
// (the failed ref) — for a default-branch fix these differ, and keying on the
// failed ref would false-stamp from unrelated commits. It stamps only when the
// observed pipeline is TERMINAL and newer than the failure that spawned the run
// (id > the run's snapshot pipeline id — see FindCIFixStampTarget), so a fix
// branch's own original failing pipeline never triggers a stamp. A terminal PASS →
// verified, a terminal FAILURE → fix_failed (classified via pipelinestatus, so both
// GitLab's "failed" and Forgejo Actions' "failure"/"error" count — a bare == "failed"
// here never stamped a re-failed Forgejo fix); cancelled/skipped/in-flight leave the
// run unverified (NULL). All errors are contained: verification is best-effort and
// must not stall the sync.
//
// It returns the stamped run and whether the stamp was VERIFIED (a terminal PASS),
// so the reset-on-green caller can land a "your auto-fix landed" notification for
// the run's owner. A false second return (fix_failed, no target, or an error) means
// the caller notifies nobody.
func (s *Service) maybeStampFixVerdict(ctx context.Context, repoID uuid.UUID, ref string, p forge.Pipeline) (store.Run, bool) {
	var verdict string
	switch {
	case pipelinestatus.IsSuccess(p.Status):
		verdict = "verified"
	case pipelinestatus.IsFailed(p.Status):
		verdict = "fix_failed"
	default:
		return store.Run{}, false // not a terminal pass/fail — nothing to stamp yet
	}
	target, err := s.q.FindCIFixStampTarget(ctx, store.FindCIFixStampTargetParams{
		RepoID:             repoID,
		Branch:             pgtype.Text{String: ref, Valid: true},
		ObservedPipelineID: pgtype.Int8{Int64: p.ID, Valid: true},
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("forgesvc: find ci_fix stamp target", "repo", repoID, "ref", ref, "error", err)
		}
		return store.Run{}, false // no ci_fix run awaiting verification on this ref
	}
	if _, err := s.q.StampFixVerdict(ctx, store.StampFixVerdictParams{
		ID:         target.ID,
		FixVerdict: pgtype.Text{String: verdict, Valid: true},
	}); err != nil {
		slog.Warn("forgesvc: stamp fix verdict", "run", target.ID, "verdict", verdict, "error", err)
		return store.Run{}, false
	}
	slog.Info("forgesvc: ci_fix verified", "run", target.ID, "ref", ref, "verdict", verdict, "pipeline", p.ID)
	return target, verdict == "verified"
}

// notifyAutofixLanded lands the ci_autofix_landed inbox row for the verified fix
// run's owner (PRD #71 M6). Best-effort and nil-safe; Slack is nil (inbox-only) —
// the run notifier already DMs run outcomes, so a Slack DM here would double up.
func (s *Service) notifyAutofixLanded(ctx context.Context, target store.Run, ref string, p forge.Pipeline) {
	if s.notifier == nil {
		return
	}
	runID := target.ID
	if _, err := s.notifier.Notify(ctx, notifysvc.Notification{
		UserID:  target.UserID,
		Kind:    "ci_autofix_landed",
		Payload: map[string]any{"ref": ref, "pipeline_web_url": p.WebURL},
		RunID:   &runID,
		Slack:   nil,
	}); err != nil {
		slog.Warn("forgesvc: ci_autofix landed notify", "run", target.ID, "ref", ref, "error", err)
	}
}
