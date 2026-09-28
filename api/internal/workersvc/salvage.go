package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Failed-run checkpoint salvage (PRD #1867). SweepSalvage is a sweeper.Pass that keeps a
// bounded, run-scoped ARCHIVE copy of a failed run's last published checkpoint at
// refs/uzi-salvage/<run-id>, created only when origin still vouches for the recorded tip
// under the branch checkpoint ref or refs/uzi-recovery/<run-id>, and deleted again when
// UZI_RECOVERY_READY_RETENTION has passed.
//
// Salvage is CREATE-ONLY toward every ref it does not own. The branch checkpoint ref
// (refs/uzi-checkpoints/<branch>) and refs/uzi-recovery/<run-id> belong to the terminal
// path and PR #1819's checkpoint retention (PRD #1810): this file never deletes or moves
// them, never calls deleteCheckpointFn, and never changes a custody hold. The only ref it
// deletes is its own refs/uzi-salvage/<run-id>, CAS-bound to the recorded tip.
const (
	// salvagePassBudgetDefault bounds one whole SweepSalvage pass (DB reads and every broker
	// call), so a slow forge can never hold the shared sweeper tick. The pass runs serially
	// on that tick before the run-liveness sweep, and the item in flight when the budget
	// runs out may still spend up to salvageWriteTimeout on its detached outcome write, so
	// budget + salvageWriteTimeout (15s) stays within the default SWEEP_INTERVAL (15s).
	salvagePassBudgetDefault = 10 * time.Second
	// salvageMaxItems caps the broker items handled per pass, round-robin between due
	// expiries and due pending rows so neither starves the other.
	salvageMaxItems = 5
	// salvageEnqueueLimit bounds the candidate rows one pass records.
	salvageEnqueueLimit = 50
	// salvageAttemptCap: a pending row with no RECORDED salvage ref becomes 'failed' at this
	// many failed attempts (RecordSalvageAttemptFailed). The cap-reaching attempt makes no
	// create: it runs only the orphan cleanup (deleteUnrecordedSalvage), on the full pass
	// budget, and caps the row only once that cleanup has succeeded.
	salvageAttemptCap = 10
	// salvageRetryBackoff: a pending row with no recorded salvage ref that is already at or
	// past salvageAttemptCap (its cleanup keeps failing) is retried at most once per this
	// interval, measured from its updated_at.
	salvageRetryBackoff = time.Hour
	// salvageHardCeiling: a pending row with no recorded salvage ref that reaches this many
	// attempts is settled 'failed' WITHOUT the cleanup, so a permanently dead remote (a
	// revoked PAT, a changed box key, a host dropped from the allowlist, a deleted repo) can
	// neither hold the RESTRICT pointer nor spend forge calls forever. last_error and an
	// error log name the salvage ref that may remain on the forge.
	salvageHardCeiling = 3 * salvageAttemptCap
	// salvagePendingScanLimit bounds the pending rows one pass reads. Rows inside their
	// salvageRetryBackoff are filtered out in Go before the salvageMaxItems budget is
	// applied, so a backlog of backed-off rows does not starve the due ones.
	salvagePendingScanLimit = 50
	// salvageNoCap is the cap passed to RecordSalvageAttemptFailed when an attempt must be
	// counted and its error recorded WITHOUT settling the row 'failed': the orphan cleanup
	// failed (or a panic interrupted the item), so the row stays pending and live and the
	// next pass retries. attempts and last_error still move, so a stuck row stays visible.
	salvageNoCap = math.MaxInt32
	// salvageErrMaxRunes bounds the persisted last_error (the column CHECK is 512).
	salvageErrMaxRunes = 512
	// salvageMinWindow is the floor of the enqueue look-back window; the window is
	// max(RecoveryReadyRetention, salvageMinWindow).
	salvageMinWindow = 24 * time.Hour

	// salvageWriteTimeout bounds each outcome write. Outcome writes are detached from the
	// pass budget (salvageWriteCtx), so a broker call that used up the budget still has its
	// result recorded instead of losing it to an already-expired context.
	salvageWriteTimeout = 5 * time.Second

	salvageFailOriginSecretBlocked = "push_secret_blocked"

	salvageStatePending       = "pending"
	salvageStateSkippedSecret = "skipped_secret"
	salvageStateUnavailable   = "unavailable"
	salvageStateRefused       = "refused"
	salvageStateExpired       = "expired"
	salvageStateDisabled      = "disabled"

	// salvageLiveRunFK is the RESTRICT pointer's constraint (migration 00264): a 23503 on it
	// at insert means the run was deleted between the candidate read and the insert.
	salvageLiveRunFK = "run_salvage_live_run_id_fkey"
)

// SweepSalvage runs one salvage pass and returns the number of run_salvage rows it
// inserted or updated. In order, under one salvagePassBudgetDefault context:
//
//  1. Enqueue, only when SalvageForges is non-empty: failed, checkpoint-eligible runs with
//     a checkpoint tip on an enabled forge, finished within max(RecoveryReadyRetention,
//     24h), not plan_rejected and not yet recorded (the ListSalvageCandidates filter). A
//     push_secret_blocked run is recorded 'skipped_secret' (no pointer, never a broker
//     call); every other one 'pending'. A 23503 on the insert (the run was deleted first)
//     is a benign skip.
//  2. At most salvageMaxItems items, alternating due expiries and due pending rows:
//     - expiry: DeleteRef on refs/uzi-salvage/<run-id> at the recorded tip; success
//     (deleted, absent or moved) settles 'expired', an error is recorded and retried.
//     Expiry runs whatever SalvageForges says: a promoted row on a forge that has since
//     left the list is simply left to this normal expiry, which keeps its bounded
//     retention and needs no second delete path.
//     - pending, forge enabled, below the cap: CreateSalvageRef; created records the
//     creation and expiry and marks the row promoted, unavailable/refused settle, anything
//     else (including an SSRF, claim-context or PAT failure) counts a failed attempt.
//     - pending, forge enabled, the attempt that reaches salvageAttemptCap: NO create. It
//     runs only the orphan cleanup (deleteUnrecordedSalvage) and records the capped
//     failure ('failed') once that succeeds, so a create that burns the pass budget can
//     never starve the cleanup of the attempt meant to cap the row.
//     - pending, forge no longer enabled (a rollback): CAS-deletes any unrecorded salvage
//     ref (deleteUnrecordedSalvage), then settles 'disabled'. 'disabled' is reached ONLY
//     from a pending row with no recorded salvage ref; a promoted (or half-promoted) row
//     on a disabled forge keeps its bounded expiry above instead.
//     - pending with a salvage ref already recorded (the pass stopped between
//     RecordSalvageCreated and MarkSalvagePromoted): marked promoted, no broker call.
//
// A pending row with salvage_created_at NULL may STILL have a salvage ref on the forge: a
// create can land without being recorded (RecordSalvageCreated failed, or receive-pack
// applied the ref but the client timed out and the re-list failed too). So neither path
// that settles such a row without a create ('disabled', and 'failed' at the attempt cap)
// does so blind: each first CAS-deletes refs/uzi-salvage/<run-id> at the recorded tip
// (absent is success) and settles only once that succeeds. A failed cleanup is recorded
// as an uncapped attempt (last_error set, row still pending and live) and retried, so an
// orphan is not forgotten and a stuck row is never silent.
//
// That retry is bounded. Once a row with no recorded salvage ref is at or past
// salvageAttemptCap it is retried only when its updated_at is at least
// salvageRetryBackoff (1h) old; backed-off rows are skipped before the per-pass item
// budget. At salvageHardCeiling (3x the cap) attempts the row is settled 'failed' with no
// further forge call, which clears the RESTRICT pointer; last_error and an error log name
// refs/uzi-salvage/<run-id> and its tip as possibly left on the forge for manual
// deletion. So a permanently dead remote costs at most about 20 extra hourly attempts
// past the cap before the row stops blocking run, repo and connection removal.
//
// Every persisted or logged error is stripped of control, bidi and line-separator
// characters and invalid UTF-8, then has the item's exact PAT replaced, is
// secretscrub-scrubbed, and is cut to 512 runes. A panic in one item is recovered,
// logged and counted as that item's failed attempt; a panic never caps a pending row (it
// cannot have finished the orphan cleanup), so a row that keeps panicking stays pending
// with the panic in last_error until the backoff and hard ceiling settle it. DB errors
// are logged and returned joined; the sweeper logs them and carries on.
func (s *Service) SweepSalvage(ctx context.Context) (int64, error) {
	budget := s.salvagePassBudget
	if budget <= 0 {
		budget = salvagePassBudgetDefault
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	now := s.now()

	var touched int64
	var errs []error
	if len(s.p.SalvageForges) > 0 {
		n, err := s.enqueueSalvage(ctx, now)
		touched += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	n, err := s.processSalvage(ctx, now)
	touched += n
	if err != nil {
		errs = append(errs, err)
	}
	return touched, errors.Join(errs...)
}

func (s *Service) enqueueSalvage(ctx context.Context, now time.Time) (int64, error) {
	window := max(s.p.RecoveryReadyRetention, salvageMinWindow)
	cands, err := s.q.ListSalvageCandidates(ctx, store.ListSalvageCandidatesParams{
		Forges: s.p.SalvageForges,
		Since:  salvageTS(now.Add(-window)),
		Lim:    salvageEnqueueLimit,
	})
	if err != nil {
		slog.Error("salvage: list candidates", "error", err)
		return 0, fmt.Errorf("salvage: list candidates: %w", err)
	}
	var touched int64
	for _, c := range cands {
		// The query already restricts to checkpoint-eligible kinds; derive the branch the
		// same way Publish does (never from a worker string).
		branch, ok := checkpointBranch(c.Kind, c.ID, c.IssueIid)
		if !ok {
			continue
		}
		state, live := salvageStatePending, pgtype.UUID{Bytes: c.ID, Valid: true}
		if c.FailOrigin.Valid && c.FailOrigin.String == salvageFailOriginSecretBlocked {
			state, live = salvageStateSkippedSecret, pgtype.UUID{}
		}
		n, err := s.q.InsertRunSalvage(ctx, store.InsertRunSalvageParams{
			RunID:     c.ID,
			UserID:    c.UserID,
			RepoID:    c.RepoID,
			ForgeType: c.ForgeType,
			Branch:    branch,
			Tip:       c.CheckpointTip,
			LiveRunID: live,
			State:     state,
		})
		if err != nil {
			if isSalvageLiveRunFKViolation(err) {
				slog.Debug("salvage: run deleted before enqueue", "run", c.ID)
				continue
			}
			slog.Error("salvage: insert", "run", c.ID, "error", err)
			return touched, fmt.Errorf("salvage: insert %s: %w", c.ID, err)
		}
		touched += n
	}
	return touched, nil
}

// salvageItem is one row the broker phase handles; expiry distinguishes the two lists.
type salvageItem struct {
	row    store.RunSalvage
	expiry bool
}

func (s *Service) processSalvage(ctx context.Context, now time.Time) (int64, error) {
	expiry, err := s.q.ListSalvageDueExpiry(ctx, store.ListSalvageDueExpiryParams{Now: salvageTS(now), Lim: salvageMaxItems})
	if err != nil {
		slog.Error("salvage: list due expiry", "error", err)
		return 0, fmt.Errorf("salvage: list due expiry: %w", err)
	}
	scanned, err := s.q.ListSalvageDuePending(ctx, store.ListSalvageDuePendingParams{Now: salvageTS(now), Lim: salvagePendingScanLimit})
	if err != nil {
		slog.Error("salvage: list due pending", "error", err)
		return 0, fmt.Errorf("salvage: list due pending: %w", err)
	}
	pending := make([]store.RunSalvage, 0, len(scanned))
	for _, row := range scanned {
		if !salvageBackedOff(row, now) {
			pending = append(pending, row)
		}
	}

	var touched int64
	var errs []error
	for _, it := range roundRobinSalvage(expiry, pending, salvageMaxItems) {
		if ctx.Err() != nil {
			break // budget spent: the rest waits for the next tick
		}
		n, err := s.salvageOne(ctx, it, now)
		touched += n
		if err != nil {
			slog.Error("salvage: record outcome", "run", it.row.RunID, "error", err)
			errs = append(errs, err)
		}
	}
	return touched, errors.Join(errs...)
}

// salvageBackedOff reports a pending row that must wait: no salvage ref recorded, at or
// past salvageAttemptCap (its cap-reaching cleanup kept failing), and touched less than
// salvageRetryBackoff ago. A row with a recorded salvage ref is never backed off (its
// only pending step is MarkSalvagePromoted).
func salvageBackedOff(row store.RunSalvage, now time.Time) bool {
	return !row.SalvageCreatedAt.Valid && row.Attempts >= salvageAttemptCap &&
		row.UpdatedAt.Valid && row.UpdatedAt.Time.After(now.Add(-salvageRetryBackoff))
}

// roundRobinSalvage alternates expiry and pending rows (expiry first), at most limit.
func roundRobinSalvage(expiry, pending []store.RunSalvage, limit int) []salvageItem {
	out := make([]salvageItem, 0, limit)
	for i := 0; len(out) < limit && (i < len(expiry) || i < len(pending)); i++ {
		if i < len(expiry) {
			out = append(out, salvageItem{row: expiry[i], expiry: true})
		}
		if i < len(pending) && len(out) < limit {
			out = append(out, salvageItem{row: pending[i]})
		}
	}
	return out
}

// salvageOne handles one item, recovering a panic (go-git has nil-deref panic paths on
// malformed forge responses) into that item's failed attempt.
func (s *Service) salvageOne(ctx context.Context, it salvageItem, now time.Time) (n int64, err error) {
	var pat string // set once the item has decrypted its PAT, so the panic path redacts it too
	defer func() {
		if r := recover(); r != nil {
			msg := salvageErrText(fmt.Errorf("salvage: panic: %v", r), pat)
			slog.Error("salvage: item panicked", "run", it.row.RunID, "panic", msg)
			if it.expiry {
				n, err = s.recordSalvageExpireFailure(ctx, it.row, msg)
				return
			}
			n, err = s.recordSalvageAttempt(ctx, it.row, msg, salvageNoCap)
		}
	}()
	if it.expiry {
		return s.expireSalvage(ctx, it.row, &pat)
	}
	return s.advancePendingSalvage(ctx, it.row, now, &pat)
}

func (s *Service) advancePendingSalvage(ctx context.Context, row store.RunSalvage, now time.Time, pat *string) (int64, error) {
	if row.SalvageCreatedAt.Valid {
		// The salvage ref was confirmed and recorded, but the pass stopped before marking
		// the row promoted. Finish that step; the row then expires normally.
		wctx, cancel := salvageWriteCtx(ctx)
		defer cancel()
		return s.q.MarkSalvagePromoted(wctx, store.MarkSalvagePromotedParams{PromotedAt: salvageTS(now), RunID: row.RunID})
	}
	if row.Attempts >= salvageHardCeiling {
		return s.giveUpSalvage(ctx, row)
	}
	if !slices.Contains(s.p.SalvageForges, row.ForgeType) {
		// Rollback: the forge left UZI_SALVAGE_FORGES before this row's salvage ref was
		// recorded. 'disabled' is reached only here, from pending with no recorded salvage
		// ref; a promoted row on a disabled forge is left to its normal bounded expiry. A
		// create may still have landed unrecorded, so CAS-delete refs/uzi-salvage/<run-id>
		// at the tip first and settle only once that succeeds; otherwise record the error
		// (uncapped, the row stays pending and live) and retry next pass.
		if derr := s.deleteUnrecordedSalvage(ctx, row, pat); derr != nil {
			return s.recordSalvageAttempt(ctx, row, salvageErrText(derr, *pat), salvageNoCap)
		}
		return s.settleSalvage(ctx, row.RunID, salvageStateDisabled)
	}
	if row.Attempts+1 >= salvageAttemptCap {
		return s.capPendingSalvage(ctx, row, pat)
	}

	remote, err := s.salvageRemote(ctx, row.RunID)
	if err != nil {
		return s.recordSalvageAttempt(ctx, row, salvageErrText(err, ""), salvageAttemptCap)
	}
	*pat = remote.pat
	res, berr := s.createSalvageFn(ctx, pushbroker.CreateSalvageRefOptions{
		CloneURL: remote.cloneURL,
		Branch:   row.Branch,
		Username: remote.username,
		PAT:      remote.pat,
		Tip:      row.Tip,
		RunID:    row.RunID,
	})
	switch res {
	case pushbroker.SalvageCreated:
		expires := now
		if r := s.p.RecoveryReadyRetention; r > 0 {
			expires = now.Add(r)
		}
		wctx, cancel := salvageWriteCtx(ctx)
		defer cancel()
		n, err := s.q.RecordSalvageCreated(wctx, store.RecordSalvageCreatedParams{
			ExpiresAt: salvageTS(expires), CreatedAt: salvageTS(now), RunID: row.RunID,
		})
		if err != nil || n == 0 {
			return n, err
		}
		m, err := s.q.MarkSalvagePromoted(wctx, store.MarkSalvagePromotedParams{PromotedAt: salvageTS(now), RunID: row.RunID})
		return n + m, err
	case pushbroker.SalvageUnavailable:
		return s.settleSalvage(ctx, row.RunID, salvageStateUnavailable)
	case pushbroker.SalvageRefused:
		return s.settleSalvage(ctx, row.RunID, salvageStateRefused)
	default:
		if berr == nil {
			berr = fmt.Errorf("salvage: create returned %v", res)
		}
		// Below the cap by construction (the cap-reaching attempt never creates), so this
		// write only counts the attempt.
		return s.recordSalvageAttempt(ctx, row, salvageErrText(berr, remote.pat), salvageAttemptCap)
	}
}

func (s *Service) expireSalvage(ctx context.Context, row store.RunSalvage, pat *string) (int64, error) {
	remote, err := s.salvageRemote(ctx, row.RunID)
	if err != nil {
		return s.recordSalvageExpireFailure(ctx, row, salvageErrText(err, ""))
	}
	*pat = remote.pat
	if derr := s.deleteSalvageFn(ctx, pushbroker.DeleteRefOptions{
		CloneURL:       remote.cloneURL,
		Ref:            pushbroker.SalvageRef(row.RunID),
		Username:       remote.username,
		PAT:            remote.pat,
		ExpectedOldTip: row.Tip,
	}); derr != nil {
		return s.recordSalvageExpireFailure(ctx, row, salvageErrText(derr, remote.pat))
	}
	return s.settleSalvage(ctx, row.RunID, salvageStateExpired)
}

// deleteUnrecordedSalvage CAS-deletes refs/uzi-salvage/<run-id> at row.Tip for a pending
// row with no recorded salvage ref, before that row is settled without one ('disabled',
// or 'failed' at the attempt cap): a create may have landed on the forge unrecorded. It
// dials through salvageRemote (the SSRF gate before the PAT is decrypted) and touches only
// the run's own salvage ref; an absent or moved ref is DeleteRef's benign success. It
// stores the decrypted PAT in *pat so the caller's error text redacts it. The returned
// error is not yet scrubbed.
func (s *Service) deleteUnrecordedSalvage(ctx context.Context, row store.RunSalvage, pat *string) error {
	remote, err := s.salvageRemote(ctx, row.RunID)
	if err != nil {
		return fmt.Errorf("salvage: unrecorded salvage ref cleanup: %w", err)
	}
	*pat = remote.pat
	if err := s.deleteSalvageFn(ctx, pushbroker.DeleteRefOptions{
		CloneURL:       remote.cloneURL,
		Ref:            pushbroker.SalvageRef(row.RunID),
		Username:       remote.username,
		PAT:            remote.pat,
		ExpectedOldTip: row.Tip,
	}); err != nil {
		return fmt.Errorf("salvage: unrecorded salvage ref cleanup: %w", err)
	}
	return nil
}

func (s *Service) settleSalvage(ctx context.Context, runID uuid.UUID, state string) (int64, error) {
	wctx, cancel := salvageWriteCtx(ctx)
	defer cancel()
	return s.q.SettleSalvage(wctx, store.SettleSalvageParams{State: state, RunID: runID})
}

// salvageWriteCtx is the context an outcome write runs under: ctx's values without its
// cancellation or pass-budget deadline, bounded by salvageWriteTimeout.
func salvageWriteCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), salvageWriteTimeout)
}

// capPendingSalvage is the attempt that reaches salvageAttemptCap on a pending row with no
// recorded salvage ref. It makes NO create: a create could burn the pass budget and leave
// the cleanup a done context, so the row would never cap. It runs only
// deleteUnrecordedSalvage, then records the capped failure (the row settles 'failed' and
// drops its live pointer), keeping the previous last_error. If the cleanup fails, the
// attempt is recorded uncapped with the cleanup error leading last_error; the row stays
// pending and is retried under salvageRetryBackoff until salvageHardCeiling.
func (s *Service) capPendingSalvage(ctx context.Context, row store.RunSalvage, pat *string) (int64, error) {
	prev := ""
	if row.LastError.Valid {
		prev = row.LastError.String
	}
	if derr := s.deleteUnrecordedSalvage(ctx, row, pat); derr != nil {
		msg := salvageErrText(fmt.Errorf("attempt cap deferred: %w; last attempt: %s", derr, prev), *pat)
		return s.recordSalvageAttempt(ctx, row, msg, salvageNoCap)
	}
	msg := salvageErrText(fmt.Errorf("gave up at the attempt cap (%d); last attempt: %s", salvageAttemptCap, prev), *pat)
	return s.recordSalvageAttempt(ctx, row, msg, salvageAttemptCap)
}

// giveUpSalvage settles a pending row with no recorded salvage ref that reached
// salvageHardCeiling: its cleanup never succeeded (the remote is unreachable, or its
// credentials or allowlisting are gone), so it is settled 'failed' with NO forge call.
// RecordSalvageAttemptFailed with the normal salvageAttemptCap does that (attempts+1 is
// past the cap and salvage_created_at is NULL, so state becomes 'failed' and live_run_id
// NULL, which every run_salvage CHECK allows for an uncreated row). A salvage ref created
// but never recorded may remain, so it is named in last_error and logged at error level
// for manual deletion.
func (s *Service) giveUpSalvage(ctx context.Context, row store.RunSalvage) (int64, error) {
	ref := pushbroker.SalvageRef(row.RunID)
	slog.Error("salvage: gave up at the hard ceiling; the salvage ref may be orphaned on the forge",
		"run", row.RunID, "ref", ref, "tip", row.Tip, "attempts", row.Attempts)
	msg := truncateRunes(fmt.Sprintf("gave up after %d attempts; %s may remain on the forge at %s and can be deleted by hand",
		row.Attempts, ref, row.Tip), salvageErrMaxRunes)
	return s.recordSalvageAttempt(ctx, row, msg, salvageAttemptCap)
}

// recordSalvageAttempt writes one failed attempt on a pending row with the given cap
// (salvageAttemptCap, or salvageNoCap to keep the row pending).
func (s *Service) recordSalvageAttempt(ctx context.Context, row store.RunSalvage, msg string, attemptCap int32) (int64, error) {
	slog.Warn("salvage: attempt failed", "run", row.RunID, "attempts", row.Attempts+1, "capped", attemptCap != salvageNoCap, "error", msg)
	wctx, cancel := salvageWriteCtx(ctx)
	defer cancel()
	return s.q.RecordSalvageAttemptFailed(wctx, store.RecordSalvageAttemptFailedParams{
		LastError: msg, AttemptCap: attemptCap, RunID: row.RunID,
	})
}

// recordSalvageExpireFailure records msg (already scrubbed and bounded) as a failed
// expiry delete; the row keeps its state and live pointer and is retried.
func (s *Service) recordSalvageExpireFailure(ctx context.Context, row store.RunSalvage, msg string) (int64, error) {
	slog.Warn("salvage: expiry failed", "run", row.RunID, "error", msg)
	wctx, cancel := salvageWriteCtx(ctx)
	defer cancel()
	return s.q.RecordSalvageExpireFailed(wctx, store.RecordSalvageExpireFailedParams{LastError: msg, RunID: row.RunID})
}

// salvageRemoteInfo is the server-derived connection a salvage broker call dials.
type salvageRemoteInfo struct {
	cloneURL, username, pat string
}

// salvageRemote derives the clone URL, bot username and PAT for runID exactly as Publish
// and the checkpoint cleanup do: GetRunClaimContext, the SSRF gate on both the base URL
// and the dialed clone host (BEFORE decrypting the PAT), then box.Open. A missing gate or
// box is an error, never fail-open.
func (s *Service) salvageRemote(ctx context.Context, runID uuid.UUID) (salvageRemoteInfo, error) {
	if s.forgeBaseURLAllowed == nil {
		return salvageRemoteInfo{}, errors.New("salvage: forge base URL allowlist is not configured")
	}
	if s.box == nil {
		return salvageRemoteInfo{}, errors.New("salvage: secret box is not configured")
	}
	rc, err := s.q.GetRunClaimContext(ctx, runID)
	if err != nil {
		return salvageRemoteInfo{}, fmt.Errorf("salvage: claim context: %w", err)
	}
	cloneURL := rc.RepoWebUrl + ".git"
	if !s.forgeBaseURLAllowed(rc.BaseUrl) {
		return salvageRemoteInfo{}, errors.New("salvage: forge base URL is not allowlisted")
	}
	cloneHost, err := forgeHostFromURL(cloneURL)
	if err != nil || !s.forgeBaseURLAllowed(cloneHost) {
		return salvageRemoteInfo{}, errors.New("salvage: clone host is not allowlisted")
	}
	pat, err := s.box.Open(rc.TokenCiphertext)
	if err != nil {
		return salvageRemoteInfo{}, errors.New("salvage: bot PAT could not be decrypted")
	}
	return salvageRemoteInfo{cloneURL: cloneURL, username: rc.BotUsername, pat: string(pat)}, nil
}

// salvageErrText makes err safe for a log or last_error, in this order:
// sanitizeSalvageText first (dropping control and bidi characters and invalid UTF-8), so a
// credential split by a dropped character is rejoined BEFORE the redaction passes see it;
// then the exact PAT this item used (when known) is replaced, since a forge PAT need not
// match any shape secretscrub knows; then secretscrub removes every known credential
// shape; then the text is cut to salvageErrMaxRunes.
func salvageErrText(err error, pat string) string {
	msg := sanitizeSalvageText(err.Error())
	if pat != "" {
		msg = strings.ReplaceAll(msg, pat, "[redacted]")
	}
	return truncateRunes(secretscrub.Scrub(msg), salvageErrMaxRunes)
}

// sanitizeSalvageText replaces invalid UTF-8 with U+FFFD, turns tab/CR/LF and the Unicode
// line and paragraph separators (U+2028, U+2029) into a space, and drops every other C0/C1
// control character (NUL and ESC included) and every Unicode bidi control, so a
// forge-supplied error can carry neither a terminal escape sequence, a bidi-reordering
// payload, a line break, nor bytes Postgres rejects (NUL, invalid UTF-8) into last_error.
func sanitizeSalvageText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r', r == '\u2028', r == '\u2029':
			return ' '
		case unicode.IsControl(r), isBidiControl(r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "\uFFFD"))
}

// isBidiControl reports the Unicode bidirectional formatting characters: ALM, LRM, RLM,
// the embeddings/overrides LRE..RLO (U+202A-U+202E) and the isolates LRI..PDI
// (U+2066-U+2069).
func isBidiControl(r rune) bool {
	switch {
	case r == '\u061C', r == '\u200E', r == '\u200F':
		return true
	case r >= '\u202A' && r <= '\u202E', r >= '\u2066' && r <= '\u2069':
		return true
	}
	return false
}

func isSalvageLiveRunFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == salvageLiveRunFK
}

func salvageTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
