package workersvc

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
	"github.com/vtmocanu/uzi/api/internal/autoselectrow"
	"github.com/vtmocanu/uzi/api/internal/codexselect"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// harness_automatic.go is the "Default · automatic" creation decision (#3845): a user whose
// default_harness is NULL, with both harnesses usable, gets the harness whose weekly allowance
// resets soonest, so the allowance about to expire unused is spent first. The decision runs
// under the shared secret-mutation lock, ignores the overflow toggle, and freezes at creation.

// autoSideState is one harness's standing for the automatic decision.
type autoSideState string

const (
	// autoUnknown: no usable reading (stale, missing, structurally unusable, API key).
	autoUnknown autoSideState = "unknown"
	// autoDepleted: below the headroom floor, capacity-held, or only a best-of-pool pick.
	autoDepleted autoSideState = "depleted"
	// autoQualified: fresh, measured and at or above the floor.
	autoQualified autoSideState = "qualified"
)

// autoSide is the evidence one harness contributes to the decision. Headroom is the
// comparison value and Measured the raw reading; both are NULL when nothing was measured.
type autoSide struct {
	State       autoSideState
	Headroom    pgtype.Float8
	Measured    pgtype.Float8
	Held        bool
	WeeklyReset *time.Time
}

func (a autoSide) known() bool { return a.State != "" && a.State != autoUnknown }

// evidence is the decision-record form: the score is zero while held, else the measured
// headroom (NULL when unmeasured); the measurement stays raw.
func (a autoSide) evidence() overflowEvidence {
	e := overflowEvidence{Measured: a.Measured, Held: a.Held, hasValue: true}
	if a.Held {
		e.value = pgtype.Float8{Float64: 0, Valid: true}
	} else {
		e.value = a.Headroom
	}
	return e
}

func isAutomaticReason(reason string) bool {
	switch reason {
	case harnessReasonAutoSoonestReset, harnessReasonAutoOnlyQualified, harnessReasonAutoFallback:
		return true
	}
	return false
}

// clearsInheritOverride reports whether an inherit credential override is normalised away for
// the creation decision: an overflow, or an automatic decision that chose Codex. Every other
// decision keeps the shared validator's Codex refusal.
func clearsInheritOverride(resolved resolvedHarness) bool {
	d := resolved.Decision
	if d == nil {
		return false
	}
	switch d.Reason {
	case harnessReasonOverflow:
		return true
	case harnessReasonAutoSoonestReset, harnessReasonAutoOnlyQualified:
		return resolved.Harness == HarnessCodex
	}
	return false
}

// chooseAutomaticHarness is the pure decision. Either side unknown, or both depleted, keeps
// Claude (auto_fallback). Both qualified: a known future weekly reset beats a missing or past
// one, then the earlier reset, then more headroom, then Claude (auto_soonest_reset). Exactly one
// qualified side wins (auto_only_qualified).
func chooseAutomaticHarness(claude, codex autoSide, now time.Time) (Harness, string) {
	if !claude.known() || !codex.known() {
		return HarnessClaude, harnessReasonAutoFallback
	}
	cq, xq := claude.State == autoQualified, codex.State == autoQualified
	switch {
	case cq && xq:
		rc := autoselect.FutureWeeklyReset(claude.WeeklyReset, now)
		rx := autoselect.FutureWeeklyReset(codex.WeeklyReset, now)
		switch {
		case (rc != nil) != (rx != nil):
			if rx != nil {
				return HarnessCodex, harnessReasonAutoSoonestReset
			}
		case rc != nil && !rc.Equal(*rx):
			if rx.Before(*rc) {
				return HarnessCodex, harnessReasonAutoSoonestReset
			}
		case codex.Headroom.Valid && (!claude.Headroom.Valid || codex.Headroom.Float64 > claude.Headroom.Float64):
			return HarnessCodex, harnessReasonAutoSoonestReset
		}
		return HarnessClaude, harnessReasonAutoSoonestReset
	case xq:
		return HarnessCodex, harnessReasonAutoOnlyQualified
	case cq:
		return HarnessClaude, harnessReasonAutoOnlyQualified
	}
	return HarnessClaude, harnessReasonAutoFallback
}

// applyAutomatic records both sides and the decision they produce on d.
func (d *harnessCreationDecision) applyAutomatic(claude, codex autoSide, now time.Time) {
	d.Preferred = HarnessClaude
	d.ClaudeSide, d.CodexSide, d.decidedAt = claude, codex, now
	d.Chosen, d.Reason = chooseAutomaticHarness(claude, codex, now)
	d.PreferredEvidence, d.OtherEvidence = claude.evidence(), codex.evidence()
	d.PreferredWeeklyReset, d.OtherWeeklyReset = claude.WeeklyReset, codex.WeeklyReset
}

func floatOf(v float64) pgtype.Float8 { return pgtype.Float8{Float64: v, Valid: true} }

// codexSideFromCandidate maps a selected Codex candidate to its side. reason is the selector's
// reason for a pooled pick, or the default/pool_empty binding for a default account.
func codexSideFromCandidate(reason codexselect.Reason, c codexselect.Candidate, minHeadroom int) autoSide {
	side := autoSide{State: autoUnknown, WeeklyReset: c.WeeklyResetAt}
	if c.ScoreKnown {
		side.Headroom, side.Measured = floatOf(c.Score), floatOf(c.Score)
	}
	switch {
	case reason == codexselect.ReasonPoolExhausted || c.CapacityExcluded:
		side.State, side.Held = autoDepleted, true
	case reason == codexselect.ReasonPoolStale:
	case reason == codexselect.ReasonAuto:
		side.State = autoQualified
	case reason == codexselect.ReasonBestOfPool:
		side.State = autoDepleted
	case !c.ScoreKnown:
	case c.Score < float64(minHeadroom):
		side.State = autoDepleted
	default:
		side.State = autoQualified
	}
	return side
}

// claudeAutoSide evaluates the Claude side over the user's enabled tokens. A pooled
// (auto-eligible, unrejected) token set is the lane: every pooled token reserve-held is
// depleted with no fallthrough to the default token; otherwise held tokens drop out and the
// selector ranks the rest. With no pooled token the owner default decides.
func claudeAutoSide(cands []autoselect.Candidate, defaultID uuid.UUID, blocked func(uuid.UUID) bool, p autoselect.Policy, now time.Time) autoSide {
	var pool, open []autoselect.Candidate
	for _, c := range cands {
		if !c.AutoEligible || c.Rejected {
			continue
		}
		pool = append(pool, c)
		if !blocked(c.SecretID) {
			open = append(open, c)
		}
	}
	if len(pool) > 0 {
		if len(open) == 0 {
			return autoSide{State: autoDepleted, Held: true}
		}
		out := autoselect.Select(open, uuid.Nil, p, now)
		if !out.Picked {
			return autoSide{State: autoUnknown}
		}
		side := autoSide{Headroom: floatOf(float64(out.Headroom)), Measured: floatOf(float64(out.Headroom))}
		for _, c := range open {
			if c.SecretID == out.SecretID {
				side.WeeklyReset = c.WeeklyResetsAt
				break
			}
		}
		switch out.Reason {
		case autoselect.ReasonAuto:
			side.State = autoQualified
		case autoselect.ReasonBestOfPool:
			side.State = autoDepleted
		default:
			return autoSide{State: autoUnknown}
		}
		return side
	}
	for _, c := range cands {
		if c.SecretID != defaultID || defaultID == uuid.Nil || c.Rejected {
			continue
		}
		if blocked(c.SecretID) {
			return autoSide{State: autoDepleted, Held: true}
		}
		c.AutoEligible = true
		e := autoselect.Classify(c, p, now)
		side := autoSide{State: autoUnknown, WeeklyReset: c.WeeklyResetsAt}
		switch e.Status {
		case autoselect.StatusEligible:
			side.State = autoQualified
		case autoselect.StatusBelowThreshold:
			side.State = autoDepleted
		default:
			return side
		}
		side.Headroom, side.Measured = floatOf(float64(e.Headroom)), floatOf(float64(e.Headroom))
		return side
	}
	return autoSide{State: autoUnknown}
}

func (s *Service) claudeAutoSideDB(ctx context.Context, q *store.Queries, userID uuid.UUID, now time.Time) (autoSide, error) {
	snap, err := buildReserveSnapshot(ctx, q, userID, now, s.reserveIntervals())
	if err != nil {
		return autoSide{}, err
	}
	rows, err := q.ListAutoSelectCandidates(ctx, userID)
	if err != nil {
		return autoSide{}, err
	}
	cands := make([]autoselect.Candidate, 0, len(rows))
	for _, r := range rows {
		cands = append(cands, autoselectrow.FromCandidateRow(r))
	}
	defaultID := uuid.Nil
	meta, err := q.GetDefaultUserSecretMeta(ctx, store.GetDefaultUserSecretMetaParams{UserID: userID, Kind: store.KindAnthropicToken})
	switch {
	case err == nil:
		if !meta.Disabled {
			defaultID = meta.ID
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return autoSide{}, err
	}
	return claudeAutoSide(cands, defaultID, snap.claudeBlocked, s.p.Autoselect, now), nil
}

// codexAutoEval is the Codex side with the pick that backs it (nil when none was made) and the
// Codex credential choice carrying the picked alias.
type codexAutoEval struct {
	side   autoSide
	pick   *codexPoolPick
	choice *codexCredentialChoice
}

// codexAutoSideDB evaluates Codex from the same source overflow uses: the pool selector in
// 'auto' mode with a pool, else the default subscription account. Expected absences (no
// credential, no pool, no row, a non-structural or API-key default) are unknown with no pick.
func (s *Service) codexAutoSideDB(ctx context.Context, q *store.Queries, userID uuid.UUID) (codexAutoEval, error) {
	unknown := codexAutoEval{side: autoSide{State: autoUnknown}}
	codexH := HarnessCodex
	other, err := s.resolveRunHarnessQ(ctx, userID, &codexH, q)
	if err != nil {
		if errors.Is(err, errNoCredentialForHarness) || errors.Is(err, errNoUsableCredential) ||
			errors.Is(err, errCodexPoolUnavailable) || errors.Is(err, errHarnessCredentialDisabled) {
			return unknown, nil
		}
		return codexAutoEval{}, err
	}
	if other.Codex == nil {
		return unknown, nil
	}
	choice := *other.Codex
	floor := s.p.Autoselect.MinHeadroom
	switch {
	case choice.Select == codexChoicePool:
		picked, e := s.selectCodexPoolAlias(ctx, q, userID, nil)
		if e != nil {
			if errors.Is(e, errCodexPoolUnavailable) {
				return unknown, nil
			}
			return codexAutoEval{}, e
		}
		choice.SecretID = picked.AliasID
		return codexAutoEval{side: codexSideFromCandidate(picked.Reason, picked.Candidate, floor), pick: &picked, choice: &choice}, nil
	case choice.AuthMode == codexAuthModeSubscription:
		row, e := q.GetCodexDefaultSelectionCandidate(ctx, store.GetCodexDefaultSelectionCandidateParams{UserID: userID, AliasID: choice.SecretID})
		if e != nil {
			if errors.Is(e, pgx.ErrNoRows) {
				return unknown, nil
			}
			return codexAutoEval{}, e
		}
		r := store.ListCodexPoolAliasesRow(row)
		if !codexPoolRowStructural(r) {
			// A non-structural default would always fail the default-mode fence.
			return unknown, nil
		}
		cs, e := s.scoreCodexCandidates(ctx, q, userID, []store.ListCodexPoolAliasesRow{r}, s.selectionNow())
		if e != nil {
			return codexAutoEval{}, e
		}
		picked := codexPoolPick{AliasID: r.AliasID, AccountID: uuid.UUID(r.ProviderAccountID.Bytes), ProviderUserID: r.ProviderUserID.String, WorkspaceAccountID: r.WorkspaceAccountID.String, Reason: codexselect.Reason(choice.Select), Candidate: cs[0]}
		picked.Candidate.Structural = true
		return codexAutoEval{side: codexSideFromCandidate(picked.Reason, picked.Candidate, floor), pick: &picked, choice: &choice}, nil
	}
	return unknown, nil
}

// evaluateAutomaticHarness decides an automatic creation. It returns the Codex pick it made
// (even when Claude wins) so the creation fence can re-check it before the INSERT commits.
func (s *Service) evaluateAutomaticHarness(ctx context.Context, q *store.Queries, userID uuid.UUID, res resolvedHarness) (resolvedHarness, *codexPoolPick, error) {
	now := s.selectionNow()
	ce, err := s.codexAutoSideDB(ctx, q, userID)
	if err != nil {
		return res, nil, err
	}
	claude, err := s.claudeAutoSideDB(ctx, q, userID, now)
	if err != nil {
		return res, nil, err
	}
	d := res.Decision
	d.applyAutomatic(claude, ce.side, now)
	if d.Chosen == HarnessCodex && ce.choice != nil {
		res.Codex = ce.choice
	} else {
		res.Codex = nil
	}
	res.Harness = d.Chosen
	return res, ce.pick, nil
}

// fenceCreationPick re-checks the Codex pick made before the INSERT. An overflow decision
// refuses on a failed default-account check (errHarnessOverflowFence) and returns a pool fence
// error as is. An automatic decision treats a Codex state change as ok=false (Codex unknown);
// other errors still fail creation.
func (s *Service) fenceCreationPick(ctx context.Context, q *store.Queries, userID uuid.UUID, d *harnessCreationDecision, pick codexPoolPick) (codexPoolPick, bool, error) {
	automatic := isAutomaticReason(d.Reason)
	if pick.Reason == codexselect.Reason(codexChoiceDefault) || pick.Reason == codexselect.Reason(codexChoicePoolEmpty) {
		row, e := q.LockCodexPoolAliasForSelection(ctx, store.LockCodexPoolAliasForSelectionParams{ID: pick.AliasID, UserID: userID})
		if e != nil {
			if automatic && errors.Is(e, pgx.ErrNoRows) {
				return codexPoolPick{}, false, nil
			}
			return codexPoolPick{}, false, e
		}
		if row.Kind != store.KindCodexAuth || !row.Enabled || !row.Status.Valid || row.Status.String != codexStatusLinked ||
			!row.ProviderAccountID.Valid || uuid.UUID(row.ProviderAccountID.Bytes) != pick.AccountID ||
			((d.Preferred != HarnessCodex || pick.Candidate.Structural) && !codexPoolAccountUsable(row.CoordState, row.ReauthRequired)) {
			if automatic {
				return codexPoolPick{}, false, nil
			}
			return codexPoolPick{}, false, errHarnessOverflowFence
		}
		return pick, true, nil
	}
	fenced, err := s.fenceCodexPoolAlias(ctx, q, userID, pick)
	if err != nil {
		if automatic && errors.Is(err, errCodexPoolUnavailable) {
			return codexPoolPick{}, false, nil
		}
		return codexPoolPick{}, false, err
	}
	return fenced, true, nil
}

// recheckAutomaticDecision recomputes an automatic decision with the final fenced Codex side
// (unknown when the pick no longer holds) and the earlier Claude side. A change of harness is
// refused so the caller's rollback undoes the INSERT; otherwise the recomputed record persists.
func (s *Service) recheckAutomaticDecision(d *harnessCreationDecision, res resolvedHarness, pick codexPoolPick, ok bool) error {
	codex := autoSide{State: autoUnknown}
	if ok {
		codex = codexSideFromCandidate(pick.Reason, pick.Candidate, s.p.Autoselect.MinHeadroom)
	}
	d.applyAutomatic(d.ClaudeSide, codex, d.decidedAt)
	if d.Chosen != res.Harness {
		return errHarnessOverflowFence
	}
	return nil
}

func timestamptzOf(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
