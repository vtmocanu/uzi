package workersvc

import (
	"context"
	"errors"
	"fmt"
)

// CustodyHoldLimitReader supplies the per-owner custody admission limit (PRD #1296 M1
// D2/D4, #3150): the maximum admission-counted open recovery custody holds one owner may
// accumulate before new code-run admission PAUSES for that owner. The one production
// implementation is *settings.Cache (the custody_hold_limit admin setting, 1..1000; its
// default is the only copy of the number). It is passed to ClaimRun as @custody_hold_limit
// and to GetCustodyAdmissionForRun by the health pass, so the claim and the queued-reason
// pill compare the same value.
//
// A reader error and a nil reader are both "no usable value": the claim refuses (the run
// stays queued; the handler answers 500) and the pill says the setting could not be read.
// Neither ever substitutes a number, because a substituted default could silently raise a
// lower configured limit. The limit bounds unpublished work awaiting recovery disposition
// PER OWNER (never a global stop-claiming switch, D4). ADR-2445 excludes at most one valid
// live current claim's non-decision hold per run; total custody remains open and still
// protects cleanup, release and teardown.
//
// Issue #1751 / ADR-1751 continuation exemption: a requeued run that was claimed before
// (claim_generation >= 1) and still holds its OWN open custody hold is exempt from the cap,
// bounded per run: it holds only while the run's own open-hold count is below the limit.
// Concurrent claims can still overshoot the cap (#1318): it is an admission gate, not a
// strict ceiling.
//
// Every consumer reads it ONCE per claim / request / pass and reuses that value.
type CustodyHoldLimitReader interface {
	CustodyHoldLimit(ctx context.Context) (int32, error)
}

// errNoCustodyLimitReader is returned when Params.CustodyHoldLimit is unwired. It is a
// wiring error, not a configuration one, and is never papered over with a default.
var errNoCustodyLimitReader = errors.New("custody_hold_limit unavailable: no settings reader wired")

// custodyLimit reads the limit once. The error is wrapped so the handler's log line names
// the failed operation.
func (s *Service) custodyLimit(ctx context.Context) (int32, error) {
	if s.p.CustodyHoldLimit == nil {
		return 0, fmt.Errorf("read custody hold limit: %w", errNoCustodyLimitReader)
	}
	n, err := s.p.CustodyHoldLimit.CustodyHoldLimit(ctx)
	if err != nil {
		return 0, fmt.Errorf("read custody hold limit: %w", err)
	}
	return n, nil
}
