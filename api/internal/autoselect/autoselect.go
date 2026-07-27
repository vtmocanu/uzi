// Package autoselect decides which of a user's opted-in Anthropic credentials a
// claim should spend, ranking them by rate-limit headroom (PRD #111).
//
// # Where the database ends and this package begins
//
// The store query ends at []Candidate. Everything after that is PURE: this package
// imports `time` and `github.com/google/uuid` and nothing else — no context, no
// store, no pgtype, no vault, no clock. `now` is a parameter. The caller does the
// fetching, the time.Now(), and the recording.
//
// That line is not stylistic. It is what lets the ranking be unit-tested against
// hand-written fixtures with no database, and it is the thing a later refactor is
// most likely to erode: the first `import "context"` here turns every test into an
// integration test. Treat a new import in this package as a design change, not a
// detail.
//
// # One classifier, not two
//
// Classify is the WHOLE eligibility gate, and it has exactly one implementation on
// purpose (D21). Settings renders each token's live eligibility and the ranker
// (M4) gates candidates on it; written twice, the two drift, and the drift is
// invisible in the worst possible way — the settings page confidently tells a user
// a token is eligible while the selector silently skips it, with no failing test
// anywhere. So the status is computed HERE, shipped as a string, and rendered
// verbatim by the web and the CLI. A `100 - pct` or a synced_at comparison in
// web/ is a bug by construction, not a style preference.
//
// M4 adds Select (the ranker) to this package; M2 ships the gate it stands on.
package autoselect

import (
	"time"

	"github.com/google/uuid"
)

// Candidate is ONE of a user's anthropic_token secrets as the ranker sees it:
// already-fetched row data with the pgtype stripped off. A nil pointer is SQL NULL.
//
// HasReading distinguishes "the LEFT JOIN found no gauge row at all" from "there is
// a row whose columns are NULL", which are different facts with different causes: a
// credential the usage endpoint permanently refuses never produces a row, while a
// reading that measured neither window produces one full of NULLs. D16 turns the
// first into "never polled" and D12 the second into "unmeasured", so collapsing
// them would lose the one diagnosis a user can act on.
type Candidate struct {
	SecretID      uuid.UUID
	Label         string
	AutoEligible  bool // user_secrets.auto_eligible — the D2 opt-in
	HasReading    bool // false ⇒ no anthropic_rate_limits row for this token
	FiveHourPct   *int16
	SevenDayPct   *int16
	FiveResetsAt  *time.Time
	SevenResetsAt *time.Time
	SyncedAt      *time.Time // NOT NULL in the table, nullable through the LEFT JOIN
	// InFlight is the count of runs currently spending this credential. Populated
	// only by M4's ranking query and used only by the ranker's in-flight bias — it
	// is deliberately NOT part of the gate, so the settings list may leave it 0
	// without changing a single Classify answer.
	InFlight int
}

// Policy is the operator-set knobs (D6). They are server env rather than per-user
// settings so one policy is testable and the UI stays simple; per-user tuning is a
// future refinement, not a gap.
type Policy struct {
	// MinHeadroom is the floor, in percentage POINTS, below which a token is not
	// picked by preference. The gauge is a SMALLINT 0..100, so the unit is points
	// throughout this package; there are no fractions anywhere.
	MinHeadroom int
	// HeadroomTiePct is M4's clustering tolerance (T). Unused by Classify — it
	// belongs to the ranker, and lives here because Policy is one struct that
	// crosses the two milestones rather than two that have to be kept in step.
	HeadroomTiePct int
	// MaxStaleness bounds how old a reading may be and still steer a decision.
	// 0 (or negative) means NOTHING IS EVER FRESH, which is not a degenerate case
	// to guard against but the correct behaviour when the poller is disabled
	// (UZI_USAGE_POLL_INTERVAL=0, which is what the e2e overlay sets): with nothing
	// refreshing the gauge, every token is stale and auto degrades to the worker's
	// non-auto binding (R2).
	MaxStaleness time.Duration
	// InflightPenalty is M4's herd control, in points per in-flight run. Unused by
	// Classify, for the same reason as HeadroomTiePct.
	InflightPenalty int
}

// Status is the per-token vocabulary shared by the ranker and the settings
// rendering. It is a CLOSED set: every value has a rendering in the web and the
// CLI, and adding one without adding both is how a user ends up looking at a blank
// chip.
type Status string

const (
	// StatusEligible: pooled, measurable, fresh, and at or above MinHeadroom — the
	// selector will consider this token.
	StatusEligible Status = "eligible"
	// StatusNotPooled: auto_eligible = false. Not a problem, just not opted in.
	StatusNotPooled Status = "not_pooled"
	// StatusNoReading: pooled, but the gauge has never produced a row for it.
	// Rendered "never polled". This is R7's silent no-op made visible: a credential
	// the usage endpoint permanently refuses never produces a row at all, so opting
	// it in would otherwise change nothing while LOOKING active.
	StatusNoReading Status = "no_reading"
	// StatusUnmeasured: a gauge row exists but at least one window's pct is NULL
	// (D12). A reading that measured neither window carries no headroom signal, so
	// it cannot be ranked and is excluded rather than defaulted to some
	// assumed-full value.
	StatusUnmeasured Status = "unmeasured"
	// StatusStale: measurable, but the reading is older than MaxStaleness. This is
	// also where the poller's 15-minute refusal backoff lands (D11) and where every
	// token lands when the poller is disabled (R2).
	//
	// D11 originally promised a distinct `refused` state and D16 removed it,
	// because nothing can serve it: the backoff is an unexported in-process map
	// with no accessor and anthropic_rate_limits has no failed-poll column. A
	// refusing token observably stops being refreshed, which is what `stale` means.
	// What is lost is the DIAGNOSIS of a token that polled before and is currently
	// backing off; it reads stale.
	StatusStale Status = "stale"
	// StatusBelowThreshold: fresh and measurable, but headroom < MinHeadroom. The
	// selector skips it by preference — though D10's best-of-pool fallback may
	// still pick it when every pooled token is here, which is why this is a
	// distinct status and not folded into "not eligible".
	StatusBelowThreshold Status = "below_threshold"
)

// Eligibility is one token's live classification. Headroom is meaningful ONLY when
// Measured is true; it is 0 otherwise, and 0 is a legal headroom, so a consumer
// must branch on Measured rather than on Headroom != 0.
type Eligibility struct {
	Status Status
	// Measured reports that the reading is fresh AND both windows carry a value —
	// i.e. that this token can be RANKED at all. Note it is true for
	// StatusBelowThreshold: a below-threshold token has a perfectly good headroom
	// number, it is just a low one, and D10's fallback needs to rank exactly those.
	Measured bool
	Headroom int
}

// Classify answers "could this token be picked right now, and if not, why" for ONE
// candidate. It is total: any input, including a zero Candidate, yields an
// Eligibility.
//
// The order of the gates is load-bearing, because each one makes the next
// meaningful: a token that is not pooled has no reason to be described as stale, a
// token with no row at all has no synced_at to compare, and a row with a NULL pct
// has no headroom to threshold. Reordering them produces answers that are true but
// unhelpful — "stale" for a credential the poller has never once reached.
func Classify(c Candidate, p Policy, now time.Time) Eligibility {
	if !c.AutoEligible {
		return Eligibility{Status: StatusNotPooled}
	}
	// SyncedAt is checked alongside HasReading rather than trusted from it: the
	// column is NOT NULL in the table but nullable through the LEFT JOIN, so a
	// query that projects the join differently could hand us one without the other.
	if !c.HasReading || c.SyncedAt == nil {
		return Eligibility{Status: StatusNoReading}
	}
	if c.FiveHourPct == nil || c.SevenDayPct == nil {
		return Eligibility{Status: StatusUnmeasured}
	}
	if p.MaxStaleness <= 0 || now.Sub(*c.SyncedAt) > p.MaxStaleness {
		return Eligibility{Status: StatusStale}
	}
	h := headroom(*c.FiveHourPct, *c.SevenDayPct)
	if h < p.MinHeadroom {
		return Eligibility{Status: StatusBelowThreshold, Measured: true, Headroom: h}
	}
	return Eligibility{Status: StatusEligible, Measured: true, Headroom: h}
}

// headroom is how much of the BINDING window is left: min(100-five, 100-seven).
//
// The min, not an average and not the five-hour window alone, because the two
// windows are a conjunction — a token at 10% of its 5-hour allowance and 98% of its
// 7-day one has 2 points of usable capacity, not 46. The 7-day window is the hard
// cap and the 5-hour one the near-term one; whichever is fuller is what actually
// stops the next run.
func headroom(fivePct, sevenPct int16) int {
	five := 100 - int(fivePct)
	seven := 100 - int(sevenPct)
	if seven < five {
		return seven
	}
	return five
}
