package workersvc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/codexselect"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Creation thresholds are deliberately independent of the account selector's policy.
const (
	overflowPreferredBelow   = 20
	overflowOtherAtLeast     = 30
	overflowAdvantageAtLeast = 15
)

// harnessCreationOrigin is creation-only authority. Its zero value excludes every
// helper caller unless the issue service explicitly supplies its provenance.
// Direct chat's two SQL inserts leave the nullable decision columns absent.
type harnessCreationOrigin struct {
	Kind              string
	TriggerSource     string
	Explicit          *Harness
	Model             *string
	AnthropicOverride bool
}

func (o harnessCreationOrigin) eligible(p store.GetHarnessCreationPolicyRow) bool {
	if o.Kind != "issue" || o.Explicit != nil || o.AnthropicOverride ||
		(o.Model != nil && *o.Model != "") || p.PlanCrossCheckEnabled || p.CodeCrossCheckEnabled {
		return false
	}
	return o.TriggerSource == "manual" || o.TriggerSource == "autopilot" || o.TriggerSource == "schedule"
}

// overflowEvidence separates comparison capacity from measurement. A capacity hold
// makes comparison known zero even when the underlying measurement is unavailable.
type overflowEvidence struct {
	Measured    pgtype.Float8
	Held        bool
	Destination bool
	value       pgtype.Float8
	hasValue    bool
}

func (e overflowEvidence) comparison() pgtype.Float8 {
	if e.hasValue {
		return e.value
	}
	if e.Held {
		return pgtype.Float8{Float64: 0, Valid: true}
	}
	return e.Measured
}

func overflowReason(enabled, eligible bool, preferred, other overflowEvidence) string {
	if !enabled {
		return "off"
	}
	if !eligible {
		return "excluded"
	}
	p, o := preferred.comparison(), other.comparison()
	if !p.Valid || !o.Valid {
		return "unknown_usage"
	}
	if other.Destination && !other.Held && p.Float64 < overflowPreferredBelow &&
		o.Float64 >= overflowOtherAtLeast && o.Float64-p.Float64 >= overflowAdvantageAtLeast {
		return "overflow"
	}
	return "preferred"
}

type harnessCreationDecision struct {
	Preferred         Harness
	Reason            string
	PreferredEvidence overflowEvidence
	OtherEvidence     overflowEvidence
}

func (d *harnessCreationDecision) chosen() Harness {
	if d.Reason != "overflow" {
		return d.Preferred
	}
	if d.Preferred == HarnessClaude {
		return HarnessCodex
	}
	return HarnessClaude
}

func codexOverflowEvidence(c codexselect.Candidate, reason codexselect.Reason) overflowEvidence {
	return overflowEvidence{
		Measured:    pgtype.Float8{Float64: c.Score, Valid: c.ScoreKnown},
		Held:        c.CapacityExcluded,
		Destination: reason == codexselect.ReasonAuto || reason == codexselect.ReasonBestOfPool || string(reason) == "default",
	}
}

// claudeOverflowEvidence enumerates the complete enabled set, including unpooled
// defaults. The query joins gauges on enablement_rev; rejected rows do not count.
// Reserve loading is independent of gauge loading, including when reserve is off.
func (s *Service) claudeOverflowEvidence(ctx context.Context, q *store.Queries, userID uuid.UUID, now time.Time, destination bool) (overflowEvidence, error) {
	snap, err := buildReserveSnapshot(ctx, q, userID, now, s.reserveIntervals())
	if err != nil {
		return overflowEvidence{}, err
	}
	rows, err := q.ListAutoSelectCandidates(ctx, userID)
	if err != nil {
		return overflowEvidence{}, err
	}
	evidence := make([]overflowEvidence, 0, len(rows))
	for _, r := range rows {
		if r.Rejected {
			continue
		}
		e := overflowEvidence{Held: snap.claudeBlocked(r.UserSecretID), Destination: true}
		if r.SyncedAt.Valid && r.SevenDayPct.Valid && s.p.Autoselect.MaxStaleness > 0 &&
			now.Sub(r.SyncedAt.Time) <= s.p.Autoselect.MaxStaleness {
			score := 100 - float64(r.SevenDayPct.Int16)
			if r.FiveHourPct.Valid {
				score = math.Min(score, 100-float64(r.FiveHourPct.Int16))
			}
			e.Measured = pgtype.Float8{Float64: score, Valid: true}
		}
		evidence = append(evidence, e)
	}
	return aggregateClaudeOverflow(evidence, destination), nil
}

// Finite fold over the full query result; one unheld unknown invalidates the
// aggregate, and any held token blocks a destination. Measured aggregation remains
// independent of holds and is absent if any underlying measurement is missing.
func aggregateClaudeOverflow(rows []overflowEvidence, destination bool) overflowEvidence {
	out := overflowEvidence{Destination: len(rows) > 0}
	if len(rows) == 0 {
		return out
	}
	measuredKnown, comparisonKnown := true, true
	measured, comparison := float64(0), float64(0)
	if destination {
		measured, comparison = 100, 100
	}
	combine := math.Max
	if destination {
		combine = math.Min
	}
	for _, r := range rows {
		out.Held = out.Held || r.Held
		if !r.Measured.Valid {
			measuredKnown = false
		} else {
			measured = combine(measured, r.Measured.Float64)
		}
		c := r.comparison()
		if !c.Valid {
			comparisonKnown = false
		} else {
			comparison = combine(comparison, c.Float64)
		}
	}
	out.Measured = pgtype.Float8{Float64: measured, Valid: measuredKnown}
	// Mixed held/unknown must remain unknown, rather than the held-zero shortcut.
	// For preferred, a partial hold must not hide a healthy token's maximum.
	allHeld := true
	for _, r := range rows {
		allHeld = allHeld && r.Held
	}
	out.Held = allHeld
	if destination {
		out.Held = anyOverflowHeld(rows)
	}
	out.Destination = !destination || !anyOverflowHeld(rows)
	out.value = pgtype.Float8{Float64: comparison, Valid: comparisonKnown}
	out.hasValue = true
	return out
}

func anyOverflowHeld(rows []overflowEvidence) bool {
	for _, r := range rows {
		if r.Held {
			return true
		}
	}
	return false
}

var errHarnessOverflowRequiresTx = errors.New("harness overflow creation requires transaction")
var errHarnessOverflowFence = errors.New("harness overflow decision changed at credential fence")

// evaluateCreationOverflow runs under the shared secret-mutation lock before any
// run/branch/alias lock. It retains both the exact Codex pick and the alternate
// resolution so the final fence cannot silently change the selected harness.
func (s *Service) evaluateCreationOverflow(ctx context.Context, q *store.Queries, userID uuid.UUID, res resolvedHarness, p store.GetHarnessCreationPolicyRow, origin harnessCreationOrigin) (resolvedHarness, *codexPoolPick, error) {
	d := &harnessCreationDecision{Preferred: res.Harness}
	res.CreationPolicy = &p
	res.Decision = d
	eligible := origin.eligible(p)
	if !p.HarnessOverflowEnabled || !eligible {
		d.Reason = overflowReason(p.HarnessOverflowEnabled, eligible, overflowEvidence{}, overflowEvidence{})
		return res, nil, nil
	}
	otherHarness := HarnessClaude
	if res.Harness == HarnessClaude {
		otherHarness = HarnessCodex
	}
	other, err := s.resolveRunHarnessQ(ctx, userID, &otherHarness, q)
	if err != nil {
		if errors.Is(err, errNoCredentialForHarness) || errors.Is(err, errNoUsableCredential) || errors.Is(err, errCodexPoolUnavailable) {
			d.Reason = "excluded"
			return res, nil, nil
		}
		return res, nil, err
	}
	codex := res
	if res.Harness != HarnessCodex {
		codex = other
	}
	var pick *codexPoolPick
	ce := overflowEvidence{}
	if codex.Codex != nil && codex.Codex.Select == codexChoicePool {
		picked, e := s.selectCodexPoolAlias(ctx, q, userID, nil)
		if e != nil {
			return res, nil, e
		}
		pick = &picked
		ce = codexOverflowEvidence(picked.Candidate, picked.Reason)
		choice := *codex.Codex
		choice.SecretID = picked.AliasID
		codex.Codex = &choice
	} else if codex.Codex != nil && codex.Codex.AuthMode == codexAuthModeSubscription {
		row, e := q.GetCodexDefaultSelectionCandidate(ctx, store.GetCodexDefaultSelectionCandidateParams{UserID: userID, AliasID: codex.Codex.SecretID})
		if e != nil {
			return res, nil, e
		}
		r := store.ListCodexPoolAliasesRow(row)
		if !codexPoolRowStructural(r) {
			return res, nil, errHarnessOverflowFence
		}
		cs, e := s.scoreCodexCandidates(ctx, q, userID, []store.ListCodexPoolAliasesRow{r}, s.selectionNow())
		if e != nil {
			return res, nil, e
		}
		picked := codexPoolPick{AliasID: r.AliasID, AccountID: uuid.UUID(r.ProviderAccountID.Bytes), ProviderUserID: r.ProviderUserID.String, WorkspaceAccountID: r.WorkspaceAccountID.String, Reason: codexselect.Reason(codex.Codex.Select), Candidate: cs[0]}
		pick = &picked
		ce = codexOverflowEvidence(cs[0], picked.Reason)
	}
	claude, e := s.claudeOverflowEvidence(ctx, q, userID, s.selectionNow(), res.Harness == HarnessCodex)
	if e != nil {
		return res, nil, e
	}
	if res.Harness == HarnessCodex {
		d.PreferredEvidence, d.OtherEvidence = ce, claude
	} else {
		d.PreferredEvidence, d.OtherEvidence = claude, ce
	}
	d.Reason = overflowReason(true, true, d.PreferredEvidence, d.OtherEvidence)
	if d.chosen() == HarnessCodex {
		res.Codex = codex.Codex
	} else {
		res.Codex = nil
	}
	res.Harness = d.chosen()
	return res, pick, nil
}

func (s *Service) persistHarnessDecision(ctx context.Context, q *store.Queries, run *store.Run, d *harnessCreationDecision) error {
	if d == nil {
		return nil
	}
	p, o := d.PreferredEvidence, d.OtherEvidence
	n, err := q.SetRunHarnessDecision(ctx, store.SetRunHarnessDecisionParams{
		ID: run.ID, UserID: run.UserID, Preferred: string(d.Preferred), Reason: d.Reason,
		PreferredScore: p.comparison(), OtherScore: o.comparison(), PreferredMeasured: p.Measured, OtherMeasured: o.Measured,
		PreferredHeld: p.Held, OtherHeld: o.Held,
	})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("harness decision: wrote %d rows, want 1", n)
	}
	run.HarnessPreferred = pgtype.Text{String: string(d.Preferred), Valid: true}
	run.HarnessDecisionReason = pgtype.Text{String: d.Reason, Valid: true}
	run.HarnessPreferredScore, run.HarnessOtherScore = p.comparison(), o.comparison()
	run.HarnessPreferredMeasured, run.HarnessOtherMeasured = p.Measured, o.Measured
	run.HarnessPreferredHeld = pgtype.Bool{Bool: p.Held, Valid: true}
	run.HarnessOtherHeld = pgtype.Bool{Bool: o.Held, Valid: true}
	return nil
}

// Nil preserves bare CreateRun/fake compatibility via the SQL COALESCE fallback.
func capturedCreationFlag(p *store.GetHarnessCreationPolicyRow, plan bool) pgtype.Bool {
	if p == nil {
		return pgtype.Bool{}
	}
	value := p.CodeCrossCheckEnabled
	if plan {
		value = p.PlanCrossCheckEnabled
	}
	return pgtype.Bool{Bool: value, Valid: true}
}
