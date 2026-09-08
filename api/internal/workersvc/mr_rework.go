package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ErrActiveMRReworkExists is returned when an mr_rework run already exists for the MR
// (backed by the uq_runs_one_active_mr_rework partial index on (repo_id, mr_iid)).
// The detector swallows it and retries next tick; a handler would map it to 409.
var ErrActiveMRReworkExists = errors.New("an active MR-rework run already exists for this merge request")

// CreateAutoMRReworkRun queues an AUTOMATIC mr_rework run for a completed run's MR that
// gained new review comments on a green pipeline (PRD #700 M3, the sibling of
// CreateAutoCIFixRun). It always sets auto_approve=true (the worker resolves its own plan
// gate, Decision 1) and stamps trigger_source='mr_rework'.
//
// It is a thin wrapper over the shared createMRReworkRun body (the createCIFixRun shape):
// PRD #1202 added an on-demand (manual) sibling, CreateManualMRReworkRun, and the
// trigger_source argument is now the ONLY thing that differs between the two callers —
// everything else (auto_approve, the snapshot, and the create-time guards below) is shared,
// so the two paths cannot drift. Its exported signature is unchanged (the poller detector
// calls it verbatim).
//
// ref is the agent/issue-N branch (also the pipeline_ref branch-guard key, written AT
// INSERT so the cross-kind guard below is never create-time-NULL — Decision 6). mrIID
// is the MR the rework folds onto; sourceRunID is the completed run whose MR is
// watched (stored as target_run_id, mirroring judge). snapshot is the MR review
// snapshot the DETECTOR already built and filtered — it rides this create path
// explicitly (Decision 8), NOT via CreateRun's issue-comment fetch — and may be nil
// (stored NULL).
//
// A single atomic guard runs AS the insert: the create query is an
// INSERT … WHERE NOT EXISTS whose predicate matches an active ci_fix run on the same
// pipeline_ref (Decision 6, the create-time CROSS-KIND branch guard — narrowed to the
// cross-kind case only), so a ci_fix and an mr_rework never share one worktree (they
// fire on opposite CI states, so this only bites a genuine race). It reports the branch
// is occupied by the other kind two ways:
//   - a committed ci_fix → zero rows inserted → pgx.ErrNoRows → ErrBranchInUse (the
//     sequential path);
//   - a concurrent-window ci_fix the INSERT's snapshot could not see → the durable
//     uq_runs_one_active_branch_ref spanning index arbitrates and the losing insert
//     raises 23505 on it → ErrBranchInUse.
//
// A second active rework on the SAME MR is a distinct constraint: because the predicate
// above no longer matches mr_rework, a same-MR duplicate proceeds past WHERE NOT EXISTS
// and reaches the uq_runs_one_active_mr_rework unique index (23505 →
// ErrActiveMRReworkExists) — previously that path was shadowed by the broader predicate,
// which returned ErrBranchInUse for a same-MR duplicate.
//
// The detector swallows all of these and retries next tick, exactly as ci-autofix does.
func (s *Service) CreateAutoMRReworkRun(ctx context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, description string, snapshot *ReviewCommentsSnapshot) (store.Run, error) {
	return s.createMRReworkRun(ctx, userID, repoID, ref, mrIID, sourceRunID, title, description, snapshot, "mr_rework", nil)
}

// CreateManualMRReworkRun is the ON-DEMAND sibling of CreateAutoMRReworkRun (PRD #1202): the
// owner presses "Rework now" (or runs `uzi run rework`) past the automatic cap. It is
// identical to the automatic path except it stamps trigger_source='manual', so the two
// kinds of rework are distinguishable in every listing (Decision 7) while running the SAME
// correctness guards (the create-time cross-kind branch guard and the
// one-active-mr_rework-per-MR index). auto_approve stays true — a rework has no CI-config
// dimension and the owner has just read the findings (Decision 4).
//
// The non-counting ledger high-water advance is now folded into the create as ONE atomic
// statement (CreateManualMRReworkRunAndAdvance): highWater is the max actionable comment id
// StartMRReworkForRun computed, and Postgres commits the run INSERT and the advance together
// or rolls both back. Previously the advance was a separate best-effort call whose failure
// was only logged — a review finding, since a create that returned success with an
// unadvanced ledger let the automatic watcher re-fire on the same comments (see
// StartMRReworkForRun).
func (s *Service) CreateManualMRReworkRun(ctx context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, description string, snapshot *ReviewCommentsSnapshot, highWater int64) (store.Run, error) {
	return s.createMRReworkRun(ctx, userID, repoID, ref, mrIID, sourceRunID, title, description, snapshot, "manual", &highWater)
}

// createMRReworkRun is the shared body of the automatic and manual mr_rework create paths.
// Everything except which store query runs (the repo-ownership check, the snapshot marshal,
// the atomic INSERT … WHERE NOT EXISTS cross-kind guard, the 23505 mappings, the queued
// notify) is identical, so the two paths cannot drift.
//
// highWater discriminates the two callers:
//   - nil (automatic path): CreateAutoMRReworkRun, stamping trigger_source=triggerSource; the
//     ledger is advanced separately by the poller's proceed step.
//   - non-nil (manual path): CreateManualMRReworkRunAndAdvance, which stamps
//     trigger_source='manual' in SQL AND folds the non-counting high-water advance
//     (GREATEST(*highWater), halt_notified reset) into the SAME atomic statement, so the run
//     and the ledger commit together or not at all. triggerSource is unused in this branch.
func (s *Service) createMRReworkRun(ctx context.Context, userID, repoID uuid.UUID, ref string, mrIID int64, sourceRunID uuid.UUID, title, description string, snapshot *ReviewCommentsSnapshot, triggerSource string, highWater *int64) (store.Run, error) {
	// Repo-ownership / existence check, mirroring createCIFixRun so an unknown repo is
	// a clean ErrRepoNotFound rather than an FK error at INSERT.
	if _, err := s.q.GetRepoForUser(ctx, store.GetRepoForUserParams{ID: repoID, UserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRepoNotFound
		}
		return store.Run{}, err
	}

	// The MR review snapshot rides the create path explicitly (Decision 8). A nil
	// snapshot stores NULL (sqlc.narg): the detector only proceeds with a non-nil
	// snapshot, but the marshal stays nil-safe so a future caller cannot ship a
	// half-populated column.
	var reviewJSON []byte
	if snapshot != nil {
		b, err := json.Marshal(snapshot)
		if err != nil {
			return store.Run{}, fmt.Errorf("marshal review snapshot: %w", err)
		}
		reviewJSON = b
	}

	// PRD #35: the OWNER's default. An automatic mr_rework run is created by the poller with
	// no user in the loop; the on-demand path is the owner acting, and either way there is no
	// per-run wait_on_limit request to honour.
	waitOnLimit := s.resolveWaitOnLimit(ctx, userID, nil)

	var (
		run store.Run
		err error
	)
	if highWater == nil {
		// Automatic path: the poller advances the ledger separately in its proceed step.
		// PRD #1202: trigger_source is 'mr_rework' from the poller detector. kind stays
		// 'mr_rework' either way; only trigger_source discriminates them (D7).
		run, err = s.q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
			UserID:           userID,
			RepoID:           repoID,
			IssueTitle:       title,
			IssueDescription: description,
			PipelineRef:      pgtype.Text{String: ref, Valid: true},
			MrIid:            pgtype.Int8{Int64: mrIID, Valid: true},
			TargetRunID:      pgtype.UUID{Bytes: sourceRunID, Valid: true},
			ReviewComments:   reviewJSON,
			WaitOnLimit:      waitOnLimit,
			TriggerSource:    triggerSource,
		})
	} else {
		// Manual (on-demand) path: the run INSERT and the non-counting high-water advance
		// commit atomically as ONE statement — trigger_source='manual' is hard-coded in the
		// query, so it is not a param here (PRD #1202 review-finding hardening).
		run, err = s.q.CreateManualMRReworkRunAndAdvance(ctx, store.CreateManualMRReworkRunAndAdvanceParams{
			UserID:           userID,
			RepoID:           repoID,
			IssueTitle:       title,
			IssueDescription: description,
			PipelineRef:      pgtype.Text{String: ref, Valid: true},
			MrIid:            pgtype.Int8{Int64: mrIID, Valid: true},
			TargetRunID:      pgtype.UUID{Bytes: sourceRunID, Valid: true},
			ReviewComments:   reviewJSON,
			WaitOnLimit:      waitOnLimit,
			HighWater:        *highWater,
		})
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// WHERE NOT EXISTS matched an active cross-kind ci_fix sibling on this pipeline_ref:
			// the branch is occupied. (Sequential path — a committed sibling is visible.)
			return store.Run{}, ErrBranchInUse
		}
		if uniqueViolationOn(err, "uq_runs_one_active_branch_ref") {
			// Concurrent-window race: the durable cross-kind spanning index arbitrated and
			// this insert lost. A ci_fix (or another mr_rework) holds the branch → ErrBranchInUse.
			return store.Run{}, ErrBranchInUse
		}
		if isUniqueViolation(err) {
			// uq_runs_one_active_mr_rework: a second active rework on the same MR. A same-MR
			// duplicate actually trips BOTH partial indexes at once (same MR ⇒ same
			// pipeline_ref), so this branch is reached only because Postgres reports the
			// violation on uq_runs_one_active_mr_rework FIRST — it is created before
			// uq_runs_one_active_branch_ref in migration 00167 (lower OID), so the
			// uq_runs_one_active_branch_ref check above does not match. That ordering is
			// pinned by TestCreateAutoMRReworkRunSameMRDuplicateIsActiveExistsLiveDB; a
			// migration that recreated the two indexes in reverse order would regress this.
			return store.Run{}, ErrActiveMRReworkExists
		}
		return store.Run{}, err
	}
	// queued drives no board-column move for an mr_rework run (issue_iid is NULL, so
	// notifyOnce skips it), but firing the hook keeps the live status broadcast
	// consistent with issue runs.
	s.notify(run.ID, "queued")
	logRunCreated(run)
	return run, nil
}

// The on-demand mr_rework validation sentinels (PRD #1202). Each is a DISTINCT typed error
// so StartMRReworkForRun's caller (the handler) can map it to a specific status + message;
// their Error() text is the user-facing reason the handler surfaces verbatim on a 409. They
// are the CORRECTNESS guards the manual path keeps (Decision 2) — the policy gates (cap,
// debounce, staleness, green pipeline) are deliberately skipped.
var (
	// ErrReworkKindUnsupported / ErrReworkRunNotCompleted: the run is not a completed
	// issue/prompt/self_improve run. Two sentinels (the handler test distinguishes the kind
	// and status classes) sharing one user message, since both mean "not a completed
	// reworkable run".
	ErrReworkKindUnsupported = errors.New("only a completed issue, prompt or self-improvement run can be reworked")
	ErrReworkRunNotCompleted = errors.New("only a completed issue, prompt or self-improvement run can be reworked")
	ErrReworkNoMR            = errors.New("this run has no merge request")
	ErrReworkMRNotOpen       = errors.New("the merge request is not open")
	ErrReworkNoToken         = errors.New("add an Anthropic token first")
	ErrReworkNothingNew      = errors.New("nothing to rework: no new review comments since the last cycle, and no guidance given")
)

// StartMRReworkForRun mints an ON-DEMAND mr_rework run for a completed run's still-open MR,
// past the automatic cap, with optional owner guidance (PRD #1202). It OWNS the run-state
// checks and the create so the web and CLI (which both reach it through one endpoint) cannot
// drift, mirroring how createCIFixRun centralises the ci-autofix guards.
//
// It skips the four POLICY gates the automatic detector applies (the cap, the quiet-period
// debounce, the head-SHA staleness check, and the green-pipeline requirement — a human
// pressing the button is attended spend on their own token, Decision 1/2) and keeps every
// CORRECTNESS guard: the run must be a completed issue/prompt/self_improve run whose MR is
// open, its owner must hold an Anthropic token, and the create-time cross-kind branch guard
// + one-active-mr_rework index still run inside CreateManualMRReworkRun. The admin
// kill-switch is enforced by the handler (the settings cache is out of workersvc's reach).
//
// snapshot is the FULL MR review-comment snapshot the handler read from the forge (nil is
// fine — a guidance-only trigger still proceeds). On success it advances the consumed
// high-water WITHOUT spending an automatic cycle (GREATEST/advance-only, attempt_count
// untouched) and resets the halt latch (halt_notified→false, unconditional even on a
// guidance-only cycle) — Decision 1/9 — so the automatic watcher never re-fires on the same
// comments and a genuinely-new comment that later hits the cap is announced once more.
//
// That advance is now performed ATOMICALLY with the create, in ONE SQL statement
// (CreateManualMRReworkRunAndAdvance): Postgres commits both the run and the ledger advance
// or neither. Previously the create and the advance were two non-atomic steps and an advance
// failure was only logged while the create returned success — leaving an unadvanced ledger
// that let the automatic watcher fire a DUPLICATE cycle on the same comments once the manual
// run went terminal. Folding them into one statement closes that window (the review finding
// this hardening addresses).
func (s *Service) StartMRReworkForRun(ctx context.Context, userID, runID uuid.UUID, guidance string, snapshot *ReviewCommentsSnapshot) (store.Run, error) {
	// Owner-scoped read — never trust a handler-supplied row. A foreign/missing run is
	// ErrRunNotFound, which the handler maps to 404 (never 403).
	run, err := s.q.GetRunByIDForUser(ctx, store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Run{}, ErrRunNotFound
		}
		return store.Run{}, err
	}

	// Correctness guards (Decision 2), each a distinct 409 reason.
	switch run.Kind {
	case runkind.Issue, runkind.Prompt, runkind.SelfImprove:
	default:
		return store.Run{}, ErrReworkKindUnsupported
	}
	if run.Status != "completed" {
		return store.Run{}, ErrReworkRunNotCompleted
	}
	if !run.MrIid.Valid {
		return store.Run{}, ErrReworkNoMR
	}
	if run.MrState.String != "opened" {
		return store.Run{}, ErrReworkMRNotOpen
	}

	repoID := uuid.UUID(run.RepoID.Bytes)
	ref := run.Branch.String
	mrIID := run.MrIid.Int64

	// The owner must be able to pay for the run this would mint (the candidate query's
	// token gate, applied at the door instead of burning a worker on a doomed run).
	hasToken, err := s.q.UserHasAnthropicToken(ctx, userID)
	if err != nil {
		return store.Run{}, fmt.Errorf("check anthropic token: %w", err)
	}
	if !hasToken {
		return store.Run{}, ErrReworkNoToken
	}

	// The loop-guard ledger. No row = zero values (never reworked), exactly as the detector
	// reads it: the generated :one returns a zero-value struct alongside pgx.ErrNoRows.
	led, err := s.q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: ref})
	switch {
	case err == nil, errors.Is(err, pgx.ErrNoRows):
	default:
		return store.Run{}, fmt.Errorf("read mr_rework ledger: %w", err)
	}

	// maxActionableID over the snapshot, computed EXACTLY as the detector does
	// (mr_review_watch.go): only an actionable kept comment counts, 0 when the snapshot is
	// nil/empty. isNew is the "there is something the automatic watcher would fire on" test.
	var maxActionableID int64
	if snapshot != nil {
		for _, c := range snapshot.Comments {
			if IsActionableReviewComment(c) && c.ID > maxActionableID {
				maxActionableID = c.ID
			}
		}
	}
	isNew := maxActionableID > led.HighWater

	// A bare trigger with nothing new and no guidance is refused — there is nothing to do
	// (Decision 3: guidance alone is a valid trigger).
	if !isNew && strings.TrimSpace(guidance) == "" {
		return store.Run{}, ErrReworkNothingNew
	}

	// Compose the description: the automatic path's sentence (mirrored from the detector)
	// plus the owner's guidance section via the one shared composer. Title names the
	// on-demand origin so it is distinguishable at a glance from an automatic cycle.
	body := fmt.Sprintf("Address the new review comments on merge request !%d for `%s`, folding the fixes onto the existing branch.", mrIID, ref)
	description := ComposeRunDescription(body, guidance)
	title := fmt.Sprintf("Rework MR review (on demand): %s (!%d)", ref, mrIID)

	// The create and the non-counting high-water advance are ONE atomic statement
	// (CreateManualMRReworkRunAndAdvance): maxActionableID is the mark to advance to
	// (UNCONDITIONAL — GREATEST keeps it where it is on a guidance-only trigger, and the
	// advance is what resets halt_notified for the new halt episode, Decision 9). Postgres
	// commits the run and the advance together or rolls both back, so a returned run can
	// never leave the ledger unadvanced.
	run, err = s.CreateManualMRReworkRun(ctx, userID, repoID, ref, mrIID, run.ID, title, description, snapshot, maxActionableID)
	if err != nil {
		// ErrBranchInUse / ErrActiveMRReworkExists map straight through to the handler's 409s.
		return store.Run{}, err
	}

	return run, nil
}
