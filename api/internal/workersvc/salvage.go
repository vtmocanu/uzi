package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

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
	// call), so a slow forge can never hold the shared sweeper tick.
	salvagePassBudgetDefault = 20 * time.Second
	// salvageMaxItems caps the broker items handled per pass, round-robin between due
	// expiries and due pending rows so neither starves the other.
	salvageMaxItems = 5
	// salvageEnqueueLimit bounds the candidate rows one pass records.
	salvageEnqueueLimit = 50
	// salvageAttemptCap: a pending row with no salvage ref becomes 'failed' at this many
	// failed attempts (RecordSalvageAttemptFailed).
	salvageAttemptCap = 10
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
//     - pending, forge enabled: CreateSalvageRef; created records the creation and
//     expiry and marks the row promoted, unavailable/refused settle, anything else
//     (including an SSRF, claim-context or PAT failure) counts a failed attempt.
//     - pending, forge no longer enabled (a rollback): settles 'disabled'. A pending row
//     has no salvage ref, so there is nothing to delete.
//     - pending with a salvage ref already recorded (the pass stopped between
//     RecordSalvageCreated and MarkSalvagePromoted): marked promoted, no broker call.
//
// Every persisted or logged error is secretscrub-scrubbed and cut to 512 runes. A panic
// in one item is recovered, logged and counted as that item's failed attempt. DB errors
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
	pending, err := s.q.ListSalvageDuePending(ctx, store.ListSalvageDuePendingParams{Now: salvageTS(now), Lim: salvageMaxItems})
	if err != nil {
		slog.Error("salvage: list due pending", "error", err)
		return 0, fmt.Errorf("salvage: list due pending: %w", err)
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
			n, err = s.recordSalvageFailure(ctx, it, msg)
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
	if !slices.Contains(s.p.SalvageForges, row.ForgeType) {
		// Rollback: the forge left UZI_SALVAGE_FORGES before this row was salvaged. No
		// salvage ref exists, so nothing is deleted.
		return s.settleSalvage(ctx, row.RunID, salvageStateDisabled)
	}

	remote, err := s.salvageRemote(ctx, row.RunID)
	if err != nil {
		return s.recordSalvageFailure(ctx, salvageItem{row: row}, salvageErrText(err, ""))
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
		return s.recordSalvageFailure(ctx, salvageItem{row: row}, salvageErrText(berr, remote.pat))
	}
}

func (s *Service) expireSalvage(ctx context.Context, row store.RunSalvage, pat *string) (int64, error) {
	remote, err := s.salvageRemote(ctx, row.RunID)
	if err != nil {
		return s.recordSalvageFailure(ctx, salvageItem{row: row, expiry: true}, salvageErrText(err, ""))
	}
	*pat = remote.pat
	if derr := s.deleteSalvageFn(ctx, pushbroker.DeleteRefOptions{
		CloneURL:       remote.cloneURL,
		Ref:            pushbroker.SalvageRef(row.RunID),
		Username:       remote.username,
		PAT:            remote.pat,
		ExpectedOldTip: row.Tip,
	}); derr != nil {
		return s.recordSalvageFailure(ctx, salvageItem{row: row, expiry: true}, salvageErrText(derr, remote.pat))
	}
	return s.settleSalvage(ctx, row.RunID, salvageStateExpired)
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

// recordSalvageFailure records msg (already scrubbed and bounded) as a failed attempt:
// an expiry retry for an expiry item, a capped create attempt for a pending one.
func (s *Service) recordSalvageFailure(ctx context.Context, it salvageItem, msg string) (int64, error) {
	slog.Warn("salvage: attempt failed", "run", it.row.RunID, "expiry", it.expiry, "error", msg)
	wctx, cancel := salvageWriteCtx(ctx)
	defer cancel()
	if it.expiry {
		return s.q.RecordSalvageExpireFailed(wctx, store.RecordSalvageExpireFailedParams{LastError: msg, RunID: it.row.RunID})
	}
	return s.q.RecordSalvageAttemptFailed(wctx, store.RecordSalvageAttemptFailedParams{
		LastError: msg, AttemptCap: salvageAttemptCap, RunID: it.row.RunID,
	})
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

// salvageErrText makes err safe for a log or last_error: the exact PAT this item used
// (when known) is replaced first, since a forge PAT need not match any shape
// secretscrub knows, then secretscrub removes every known credential shape, then the
// text is cut to salvageErrMaxRunes.
func salvageErrText(err error, pat string) string {
	msg := err.Error()
	if pat != "" {
		msg = strings.ReplaceAll(msg, pat, "[redacted]")
	}
	return truncateRunes(secretscrub.Scrub(msg), salvageErrMaxRunes)
}

func isSalvageLiveRunFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == salvageLiveRunFK
}

func salvageTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
