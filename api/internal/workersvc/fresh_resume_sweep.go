package workersvc

import (
	"context"
	"log/slog"
	"time"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// sweepFreshResumes is the sweep's fresh resume arm (issue #4628, plan section 5). A pending transfer is
// refused when its admission deadline passed or the instance gate is off; a claimed one only on positive
// loss evidence (ListFreshResumeSweepCandidates: the destination's heartbeat is stale, it re-registered,
// its row is gone, the transfer deadline passed, or an accepted snapshot past the grace omits the run).
// Each row is one transaction that takes the locks in RefuseFreshResumeInTx's order and re-evaluates the
// same selector narrowed to that row under them, so a row that moved on between the unlocked read and the
// locks is left alone. The arm holds no worker lock of its own, so the predecessor lock may block.
//
// It is bounded by the selector's LIMIT (one attempt per row per tick) and a failing row is logged and
// skipped: it never blocks its siblings or the rest of the sweep. claimedEnabled is false inside the
// boot grace, when every heartbeat is transiently stale and a loss cannot be told from the outage.
// It returns the number of rows refused.
func (s *Service) sweepFreshResumes(ctx context.Context, staleCutoff time.Time, claimedEnabled bool) int64 {
	if s.txBeginner == nil {
		return 0
	}
	base := store.ListFreshResumeSweepCandidatesParams{
		GateOn: s.p.FreshResume, ClaimedEnabled: claimedEnabled, StaleCutoff: pgconv.Time(staleCutoff),
	}
	cands, err := s.listFreshResumeSweepCandidates(ctx, base)
	if err != nil {
		slog.Warn("sweeper: fresh resume candidates", "error", err)
		return 0
	}
	var refused int64
	for _, c := range cands {
		if ctx.Err() != nil {
			break
		}
		outcome, err := s.refuseFreshResumeForSweep(ctx, base, c)
		if err != nil {
			slog.Warn("sweeper: fresh resume refusal", "transfer", c.ID.String(), "run", c.RunID.String(), "reason", c.Reason, "error", err)
			continue
		}
		switch outcome {
		case FreshRefusedPending, FreshRefusedRestored, FreshRefusedRunMoved:
			refused++
			if outcome == FreshRefusedRestored {
				// The run is back at its held recovery_wait.
				s.publishSwept(c.RunID, "recovery_wait")
			}
		}
	}
	return refused
}

func (s *Service) listFreshResumeSweepCandidates(ctx context.Context, p store.ListFreshResumeSweepCandidatesParams) ([]store.ListFreshResumeSweepCandidatesRow, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return store.New(tx).ListFreshResumeSweepCandidates(ctx, p)
}

// refuseFreshResumeForSweep refuses one candidate in its own READ COMMITTED transaction.
func (s *Service) refuseFreshResumeForSweep(ctx context.Context, base store.ListFreshResumeSweepCandidatesParams, c store.ListFreshResumeSweepCandidatesRow) (FreshRefuseOutcome, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	holds := func(ctx context.Context, q *store.Queries) (bool, error) {
		p := base
		p.OnlyID = pgconv.UUID(c.ID)
		rows, err := q.ListFreshResumeSweepCandidates(ctx, p)
		if err != nil {
			return false, err
		}
		return len(rows) == 1 && rows[0].ID == c.ID && rows[0].Reason == c.Reason, nil
	}
	outcome, err := RefuseFreshResumeInTx(ctx, tx, c.ID, c.Reason, RefuseWhenHolds(holds))
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return outcome, nil
}
