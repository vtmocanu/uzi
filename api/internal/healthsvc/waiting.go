package healthsvc

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type waitOutcome struct {
	eligibility store.CountOnlineWorkersClaimableForRunRow
	timely      bool
}

// waitConfirmations belongs to one evaluation. The row loops bound attempts to 200,
// each call to two seconds and all calls to four seconds. Failures are memoized;
// they leave sibling rows on their genuine-wait path without stopping evaluation.
// Callbacks must honor their context; no background goroutine is created here.
type waitConfirmations struct {
	ctx      context.Context
	cancel   context.CancelFunc
	outcomes map[uuid.UUID]waitOutcome
}

func (w *waitConfirmations) close() {
	if w.cancel != nil {
		w.cancel()
	}
}

func validWaitTime(t pgtype.Timestamptz, now time.Time) bool {
	return t.Valid && t.InfinityModifier == pgtype.Finite && !t.Time.IsZero() && !t.Time.After(now)
}

func (w *waitConfirmations) confirm(s *Service, ctx context.Context, now time.Time, id uuid.UUID, reason pgtype.Text, since pgtype.Timestamptz) (bool, time.Time) {
	if !reason.Valid || reason.String != workersvc.ReasonWorkersUpgrading || !validWaitTime(since, now) || s.cfg.WorkerEligibilityForHealth == nil {
		return false, time.Time{}
	}
	if w.outcomes == nil {
		w.outcomes = make(map[uuid.UUID]waitOutcome)
	}
	result, found := w.outcomes[id]
	if !found {
		if len(w.outcomes) >= fleetCapacityMaxRollConfirmations {
			return false, time.Time{}
		}
		if w.ctx == nil {
			w.ctx, w.cancel = context.WithTimeout(ctx, fleetCapacityConfirmBudget)
		}
		if w.ctx.Err() != nil {
			return false, time.Time{}
		}
		callCtx, cancel := context.WithTimeout(w.ctx, fleetCapacityConfirmTimeout)
		e, err := s.cfg.WorkerEligibilityForHealth(callCtx, now, id)
		deadline, _ := callCtx.Deadline()
		result = waitOutcome{eligibility: e, timely: err == nil && callCtx.Err() == nil && time.Now().Before(deadline)}
		cancel()
		w.outcomes[id] = result
	}
	e := result.eligibility
	if !result.timely || e.DrainingEligible <= 0 || e.NonDrainingEligible != 0 || e.SuitableOwnDraining != 0 || !validWaitTime(e.LatestSuitableDrainingSince, now) {
		return false, time.Time{}
	}
	overlap := since.Time
	if e.LatestSuitableDrainingSince.Time.After(overlap) {
		overlap = e.LatestSuitableDrainingSince.Time
	}
	return true, overlap
}

const waitingEvidenceLimit = 5

// waitEvidence retains the first rows in store order, reserving the last slot for
// the first unknown age if it appears after the cap. Stored reasons cannot affect retention.
type waitEvidence struct {
	rows            []apitypes.HealthEvidenceDTO
	unknownRetained bool
	total           int
}

func (e *waitEvidence) append(id, owner uuid.UUID, reason pgtype.Text, since pgtype.Timestamptz, now time.Time) {
	e.total++
	unknown := !validWaitTime(since, now)
	if len(e.rows) >= waitingEvidenceLimit {
		if !unknown || e.unknownRetained {
			return
		}
		e.rows = e.rows[:waitingEvidenceLimit-1]
	}
	e.unknownRetained = e.unknownRetained || unknown
	age := "unavailable"
	if validWaitTime(since, now) {
		age = humanDur(now.Sub(since.Time))
	}
	stored := "unavailable"
	if reason.Valid {
		stored = safe(reason.String)
	}
	e.rows = append(e.rows, apitypes.HealthEvidenceDTO{Label: "Waiting run", Value: fmt.Sprintf("run %s; owner %s; waited %s; stored reason %s", safe(id.String()), safe(owner.String()), age, stored)})
}

func (e *waitEvidence) finish() []apitypes.HealthEvidenceDTO {
	if e.total > len(e.rows) {
		e.rows = append(e.rows, apitypes.HealthEvidenceDTO{Label: "Waiting runs omitted", Value: fmt.Sprint(e.total - len(e.rows))})
	}
	return e.rows
}
