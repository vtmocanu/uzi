package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// Weekly usage reserve, running-run side (PRD #3522 M4a). The sweeper's reserve pause pass
// (reservePausePass) is the one writer of everything below:
//
//   - REQUEST: a running run whose recorded spent credential reads Held on a FRESH reading gets the
//     system pause request pause_mode = 'reserve' (served to the worker as a 'milestone' request,
//     servedPauseMode), exactly when reserveEnforced applies. An Unknown reading never pauses; a run
//     with no recorded credential is left alone; a pending owner or wall request is never overwritten
//     (RequestReservePauses' pause_requested_at IS NULL).
//   - WITHDRAW: a pending reserve request is withdrawn when the threshold is cleared, the pause
//     toggle is off (both regardless of reading freshness, so a poller outage cannot strand them) or
//     the spent credential now reads OK on a fresh reading.
//   - RELEASE: a run parked by the reserve (paused, hold_reason weekly_reserve) is resumed through
//     ResumePausedRun (the owner's resume path: wall budget banked, started_at kept, worker fencing and
//     health reset) when the threshold is cleared, the toggle is off, or reserveRunDecision admits it
//     on a fresh OK reading. It never touches an owner pause, a wall park or any other hold, because the
//     list is keyed on the hold and ResumePausedRun is called with required_hold_reason.
//   - EXPIRY: a manual-resume exemption the clock or a reading has ended is dropped (all four
//     columns) so the display and ClaimRun agree.

// reservePauseQueries is the optional store surface of the pass. *store.Queries satisfies it; a fake
// Store does not, and the pass is then a no-op.
type reservePauseQueries interface {
	reserveMarkQueries
	ListReservePauseOwners(ctx context.Context) ([]uuid.UUID, error)
	ListReservePauseRunsForOwner(ctx context.Context, userID uuid.UUID) ([]store.Run, error)
	RequestReservePauses(ctx context.Context, ids []uuid.UUID) ([]store.RequestReservePausesRow, error)
	WithdrawReservePausesRunning(ctx context.Context, ids []uuid.UUID) ([]store.WithdrawReservePausesRunningRow, error)
	WithdrawReservePausesIdle(ctx context.Context, ids []uuid.UUID) ([]store.WithdrawReservePausesIdleRow, error)
	ClearReserveOverride(ctx context.Context, arg store.ClearReserveOverrideParams) (int64, error)
}

// reserveOverrideQueries is the optional read behind reserveEndedOverrides.
type reserveOverrideQueries interface {
	ListReserveOverrideRuns(ctx context.Context, userID uuid.UUID) ([]store.Run, error)
}

// reserveWithdrawQueries is the optional write behind the claim-time withdrawal.
type reserveWithdrawQueries interface {
	WithdrawReservePausesIdle(ctx context.Context, ids []uuid.UUID) ([]store.WithdrawReservePausesIdleRow, error)
}

// holdWeeklyReserve is the runs.hold_reason of a run the reserve parked.
const holdWeeklyReserve = "weekly_reserve"

// servedPauseMode is the pause mode a worker is told. The reserve request is a milestone request to
// both harnesses (they honour the server's boundary decision), so 'reserve' never reaches a worker.
func servedPauseMode(mode string) string {
	if mode == "reserve" {
		return "milestone"
	}
	return mode
}

// reserveEndedOverrides lists the owner's queued runs whose persisted exemption is still within its
// clock window but which the reading-aware rule (overrideActive) has ended early. ClaimRun's time-only
// test cannot see those, so they ride its @reserve_override_ended list.
func (s *Service) reserveEndedOverrides(ctx context.Context, snap *reserveSnap, now time.Time) ([]uuid.UUID, error) {
	out := []uuid.UUID{}
	if !snap.on() {
		return out, nil
	}
	q, ok := s.q.(reserveOverrideQueries)
	if !ok {
		return out, nil
	}
	runs, err := q.ListReserveOverrideRuns(ctx, snap.userID)
	if err != nil {
		return nil, fmt.Errorf("weekly reserve override runs: %w", err)
	}
	for _, run := range runs {
		if run.ReserveExemptUntil.Valid && run.ReserveExemptUntil.Time.After(now) && !overrideActive(run, snap, now) {
			out = append(out, run.ID)
		}
	}
	return out, nil
}

// withdrawStaleReserveRequest withdraws a pending reserve request on a freshly claimed run and
// returns the run as the worker should see it. Best effort: on a failure the request stands and the
// sweeper's own withdraw pass retries.
func (s *Service) withdrawStaleReserveRequest(ctx context.Context, run store.Run) store.Run {
	if !run.PauseMode.Valid || run.PauseMode.String != "reserve" {
		return run
	}
	q, ok := s.q.(reserveWithdrawQueries)
	if !ok {
		return run
	}
	rows, err := q.WithdrawReservePausesIdle(ctx, []uuid.UUID{run.ID})
	if err != nil {
		slog.Warn("claim: could not withdraw a stale weekly reserve pause request", "run_id", run.ID.String(), "error", err)
		return run
	}
	if len(rows) > 0 {
		run.PauseRequestedAt = pgtype.Timestamptz{}
		run.PauseMode = pgtype.Text{}
		run.PauseAfterCount = pgtype.Int4{}
	}
	return run
}

// reservePausable is the allowlist of the milestone pause (CreatePauseInput): non-interactive
// issue / task / prompt / self_improve runs.
func reservePausable(run store.Run) bool {
	if run.Interactive {
		return false
	}
	switch run.Kind {
	case runkind.Issue, runkind.Task, runkind.Prompt, runkind.SelfImprove:
		return true
	}
	return false
}

// reservePausePass is the sweeper pass for the running-run side of the reserve. It returns the
// number of runs it changed. One owner's failure is logged and skipped, never blocking a sibling
// owner; the loop is bounded by the owners with reserve state or a threshold and a running run, one
// tick at a time.
func (s *Service) reservePausePass(ctx context.Context, now time.Time) (int64, error) {
	q, ok := s.q.(reservePauseQueries)
	if !ok {
		return 0, nil
	}
	if _, ok := s.q.(reserveQueries); !ok {
		return 0, nil
	}
	owners, err := q.ListReservePauseOwners(ctx)
	if err != nil {
		return 0, err
	}
	var changed int64
	for _, uid := range owners {
		n, oerr := s.reservePauseForOwner(ctx, q, uid, now)
		changed += n
		if oerr != nil {
			slog.Warn("sweeper: weekly reserve pause pass failed for an owner", "user_id", uid.String(), "error", oerr)
		}
	}
	return changed, nil
}

func (s *Service) reservePauseForOwner(ctx context.Context, q reservePauseQueries, uid uuid.UUID, now time.Time) (int64, error) {
	snap, err := s.reserveSnapshot(ctx, uid, now)
	if err != nil {
		return 0, err
	}
	runs, err := q.ListReservePauseRunsForOwner(ctx, uid)
	if err != nil {
		return 0, err
	}
	var changed int64

	// 1. Drop ended exemptions first, so the decisions below see the run as the claim gate does.
	for i := range runs {
		run := runs[i]
		if !run.ReserveExemptUntil.Valid || overrideActive(run, snap, now) {
			continue
		}
		n, cerr := q.ClearReserveOverride(ctx, store.ClearReserveOverrideParams{ID: run.ID, ExpectedGrantedAt: run.ReserveExemptGrantedAt})
		if cerr != nil {
			return changed, cerr
		}
		if n > 0 {
			changed++
		}
		runs[i].ReserveExemptUntil = pgtype.Timestamptz{}
		runs[i].ReserveExemptGrantedAt = pgtype.Timestamptz{}
		runs[i].ReserveExemptCredentialID = pgtype.UUID{}
		runs[i].ReserveExemptWindowReset = pgtype.Timestamptz{}
		runs[i].ReserveExemptPriorPct = pgtype.Int2{}
	}

	// 2. Classify.
	var withdrawRunning, withdrawIdle, request []uuid.UUID
	var release []store.Run
	for _, run := range runs {
		switch {
		case run.Status == "paused" && run.HoldReason.Valid && run.HoldReason.String == holdWeeklyReserve:
			release = append(release, run)
		case run.PauseMode.Valid && run.PauseMode.String == "reserve" && run.PauseRequestedAt.Valid:
			if !reserveRequestStillWanted(run, snap) {
				if run.Status == "running" {
					withdrawRunning = append(withdrawRunning, run.ID)
				} else if run.Status != "paused" {
					withdrawIdle = append(withdrawIdle, run.ID)
				}
			}
		case run.Status == "running" && !run.PauseRequestedAt.Valid && reservePausable(run):
			if v, ok := snap.spentVerdict(run); ok && v.State == usagereserve.Held && v.Fresh &&
				reserveEnforced(run, snap, now) {
				request = append(request, run.ID)
			}
		}
	}

	// 3. Withdraw.
	if len(withdrawRunning) > 0 {
		rows, werr := q.WithdrawReservePausesRunning(ctx, withdrawRunning)
		if werr != nil {
			return changed, werr
		}
		for _, r := range rows {
			changed++
			s.publishSwept(r.ID, r.Status)
		}
	}
	if len(withdrawIdle) > 0 {
		rows, werr := q.WithdrawReservePausesIdle(ctx, withdrawIdle)
		if werr != nil {
			return changed, werr
		}
		for _, r := range rows {
			changed++
			s.publishSwept(r.ID, r.Status)
		}
	}

	// 4. Release.
	if len(release) > 0 {
		n, rerr := s.releaseReservePauses(ctx, q, uid, snap, release, now)
		changed += n
		if rerr != nil {
			return changed, rerr
		}
	}

	// 5. Request.
	if len(request) > 0 {
		rows, qerr := q.RequestReservePauses(ctx, request)
		if qerr != nil {
			return changed, qerr
		}
		for _, r := range rows {
			changed++
			// The run stays 'running' (the worker parks it at its boundary); publish so live surfaces
			// show the pending pause.
			s.publishSwept(r.ID, r.Status)
		}
	}
	return changed, nil
}

// reserveRequestStillWanted reports whether a pending reserve request should stand: the reserve is on
// with the pause toggle, and the recorded credential has not been seen to read OK on a fresh reading.
// An Unknown reading, a missing credential or a hold keep it (a poller outage never withdraws on
// freshness; only clearing the threshold or the toggle does that).
func reserveRequestStillWanted(run store.Run, snap *reserveSnap) bool {
	if !snap.on() || !snap.pauseRunning {
		return false
	}
	if v, ok := snap.spentVerdict(run); ok && v.State == usagereserve.OK && v.Fresh {
		return false
	}
	return true
}

// reserveReleaseHasCapacity: an enforced reserve never releases a run whose recorded credential is gone
// (deleted or disabled while it was paused): no reading can show it below the threshold, and a
// release would only turn the pause into a failed claim. With no lane that could claim the run, the
// lane loop proves nothing, so an enforced reserve releases only on a fresh OK reading of the
// credential the run spent.
func (s *Service) reserveReleaseHasCapacity(run store.Run, snap *reserveSnap, workers []store.ListReserveWorkersRow, hasPool, ephemeral bool, now time.Time) bool {
	if !reserveEnforced(run, snap, now) {
		return true
	}
	v, ok := snap.spentVerdict(run)
	if !ok {
		return false
	}
	if len(s.reserveCandidateLanes(run, workers, hasPool, ephemeral, now)) > 0 {
		return true
	}
	return v.Fresh && v.State == usagereserve.OK
}

// releaseReservePauses resumes the owner's weekly_reserve pauses the reserve no longer holds. A
// cleared threshold or a toggle turned off releases regardless of freshness; otherwise a run is
// released only when reserveRunDecision (affinity-aware, like the marker) admits it, which for an
// enforced reserve means a fresh OK reading on the credential it would spend.
func (s *Service) releaseReservePauses(ctx context.Context, q reservePauseQueries, uid uuid.UUID, snap *reserveSnap, runs []store.Run, now time.Time) (int64, error) {
	var workers []store.ListReserveWorkersRow
	hasPool, ownerEphemeral := false, false
	if snap.on() && snap.pauseRunning {
		var err error
		if workers, err = q.ListReserveWorkers(ctx, uid); err != nil {
			return 0, err
		}
		if hasPool, err = q.UserHasAutoEligibleAnthropicToken(ctx, uid); err != nil {
			return 0, err
		}
		ownerEphemeral = s.ownerEphemeralEligible(ctx, q, uid)
	}
	var released int64
	for _, run := range runs {
		if snap.on() && snap.pauseRunning {
			// Resuming refreshes updated_at, which is what the affinity ceiling measures, so the
			// resumed run is pinned to its worker again: evaluate it as it will be queued.
			queued := run
			queued.UpdatedAt = pgconv.Time(now)
			ephemeral := ownerEphemeral || run.EgressProfileID.Valid
			if s.reserveRunDecision(queued, snap, workers, hasPool, ephemeral, now).held ||
				!s.reserveReleaseHasCapacity(queued, snap, workers, hasPool, ephemeral, now) {
				continue
			}
		}
		ok, err := s.resumeReservePause(ctx, run)
		if err != nil {
			return released, err
		}
		if ok {
			released++
		}
	}
	return released, nil
}

// reserveReleaseAuditBody is the body of the kind='resume' audit row the sweeper's release writes.
const reserveReleaseAuditBody = "weekly usage reserve released this run"

// resumeReservePause resumes one weekly_reserve pause through ResumePausedRun, the owner's resume
// statement, guarded on the hold the caller read. False (no error) means the hold changed under us
// or the resume was refused; nothing was written.
func (s *Service) resumeReservePause(ctx context.Context, run store.Run) (bool, error) {
	_, err := s.q.ResumePausedRun(ctx, store.ResumePausedRunParams{
		ID:                         run.ID,
		UserID:                     run.UserID,
		AllowCompletionBlockedHold: false,
		GlobalTimeoutSeconds:       int32(s.p.RunTimeout.Seconds()),
		StaleCutoff:                s.staleWorkerCutoff(),
		RequiredHoldReason:         pgconv.Text(holdWeeklyReserve),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("release weekly reserve pause: %w", err)
	}
	if _, aerr := s.q.CreateRunInput(ctx, store.CreateRunInputParams{
		RunID: run.ID, Kind: "resume", Body: pgconv.Text(reserveReleaseAuditBody),
	}); aerr != nil {
		slog.Warn("weekly reserve release: write resume audit row", "run", run.ID.String(), "error", aerr)
	}
	s.publishSwept(run.ID, "queued")
	return true, nil
}

// ReserveOverrideWindow is the longest a manual-resume override lasts when the reading carried no
// usable weekly reset, and the cap on one that did: one week.
const ReserveOverrideWindow = 7 * 24 * time.Hour

// ReserveOverrideGrant is the manual-resume override ResumeRunNow records on a weekly_reserve pause
// (PRD #3522 M4a, D9): the run is exempt from the reserve for the credential it spent until the next
// observed 7-day reset. WindowReset is the anchor the observed-reset end compares against (nil when the
// owner's reading carried no usable reset time); Until is the anchor, or GrantedAt plus
// ReserveOverrideWindow when there is none (never later than that cap).
type ReserveOverrideGrant struct {
	GrantedAt    time.Time
	CredentialID *uuid.UUID
	WindowReset  *time.Time
	// PriorPct is the 7-day utilization the grant read, recorded with WindowReset: the reset
	// detector's prior, so a later threshold change cannot hide an observed reset.
	PriorPct *int16
	Until    time.Time
}

// ComputeReserveOverride computes the override an owner's manual resume of the given weekly_reserve
// pause records, from the owner's CURRENT reading of the credential the run spent. It returns nil when
// the reserve is off (the threshold was cleared), where there is nothing to override.
func ComputeReserveOverride(ctx context.Context, q *store.Queries, run store.Run, iv ReserveIntervals, now time.Time) (*ReserveOverrideGrant, error) {
	snap, err := buildReserveSnapshot(ctx, q, run.UserID, now, reserveIntervals{claude: iv.Claude, codex: iv.Codex})
	if err != nil {
		return nil, err
	}
	if !snap.on() {
		return nil, nil
	}
	g := &ReserveOverrideGrant{GrantedAt: now, Until: now.Add(ReserveOverrideWindow)}
	switch {
	case run.Harness == harnessCodex && run.CodexSecretID.Valid:
		id := uuid.UUID(run.CodexSecretID.Bytes)
		g.CredentialID = &id
	case run.Harness != harnessCodex && run.AnthropicSecretID.Valid:
		id := uuid.UUID(run.AnthropicSecretID.Bytes)
		g.CredentialID = &id
	}
	// The anchor says "the run read at or above the threshold at the grant", which overrideEndedByReading
	// relies on, so it is recorded only from a fresh held verdict; anything else keeps the seven-day cap.
	if v, ok := snap.spentVerdict(run); ok && v.Fresh && v.State == usagereserve.Held && v.ResetsAt != nil && v.ResetsAt.After(now) {
		anchor := v.ResetsAt.UTC()
		g.WindowReset = &anchor
		prior := int16(v.Pct) //nolint:gosec // G115: Verdict.Pct is a 7-day percentage, floored and clamped to 0..100 by both evaluators
		g.PriorPct = &prior
		if anchor.Before(g.Until) {
			g.Until = anchor
		}
	}
	return g, nil
}

// WorkerVisibleHoldReason is the ONE mapping of a run's hold_reason to what a worker is told (every
// worker-facing emission goes through it: the ownership probe, the state/pause ACK run bodies and the
// other worker run responses). The agent treats a paused run with a null hold as an ordinary pause
// (retain it, wait for the owner) and any other hold as a recovery or refusal path, so the reserve's
// weekly_reserve hold reads as null to a worker. Owner-facing DTOs keep the real value.
func WorkerVisibleHoldReason(reason *string) *string {
	if reason != nil && *reason == holdWeeklyReserve {
		return nil
	}
	return reason
}

// WorkerVisiblePauseMode is servedPauseMode for a worker-facing run body's pending pause mode: the
// reserve's 'reserve' request reads as an ordinary 'milestone' request to a worker.
func WorkerVisiblePauseMode(mode *string) *string {
	if mode == nil {
		return nil
	}
	served := servedPauseMode(*mode)
	return &served
}
